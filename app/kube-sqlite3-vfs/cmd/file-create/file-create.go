// Command file-create exercises the VFS's ReadAt/WriteAt against a real
// local file: every write goes to both a plain os.File and the VFS-backed
// file, and both are read back and compared. It is a manual differential
// test harness, not part of any workflow.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"

	"github.com/RichardoC/kube-sqlite3-vfs/pkg/vfs"

	// The driver is never opened here, but its cgo package provides the
	// sqlite3_* symbols sqlite3vfs's C shim links against.
	_ "github.com/mattn/go-sqlite3"
	"github.com/thought-machine/go-flags"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// Options are the command's flags.
type Options struct {
	KubeConfig string `long:"kubeconfig" description:"(optional) absolute path to the kubeconfig file"`
	Verbose    bool   `long:"verbosity" short:"v" description:"Log at debug level rather than info"`
	Retries    int    `long:"retries" description:"Number of retries for API calls" default:"1"`
}

// newLogger returns the process logger: debug-level text logging with -v,
// info-level JSON logging without.
func newLogger(verbose bool) *slog.Logger {
	if verbose {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
}

func main() {
	var opts Options
	parser := flags.NewParser(&opts, flags.Default)
	if _, err := parser.Parse(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	logger := newLogger(opts.Verbose)
	slog.SetDefault(logger)

	logger.Info("Got config", "opts", opts)

	if wd, err := os.Getwd(); err == nil {
		logger.Info("working directory", "dir", wd)
	}

	// The only two os.Exit calls in the process are the flag-parse failure
	// above and this one, both before or after run has returned: the old
	// fatal() exited from the middle of the work, skipping the deferred
	// f.Close. A failure now unwinds through run's defers and leaves the
	// exit code to main.
	if err := run(opts, logger); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run does the command's work and returns the first failure instead of
// exiting, so main can print it and exit non-zero and the deferred closes
// in run and runWithClient run on every path out.
func run(opts Options, logger *slog.Logger) error {
	clientset, err := newClientset(opts.KubeConfig)
	if err != nil {
		return err
	}
	return runWithClient(clientset, opts, logger)
}

// newClientset builds a clientset from --kubeconfig, or from the current
// context in the kubeconfig under the user's home directory.
func newClientset(kubeconfigFlag string) (kubernetes.Interface, error) {
	var kubeconfig string
	if kubeconfigFlag != "" {
		kubeconfig = kubeconfigFlag
	} else if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	// create the clientset
	return kubernetes.NewForConfig(config)
}

// runWithClient holds the command's body. It takes a kubernetes.Interface
// so the tests can drive it with the fake clientset, and it returns the
// first hard failure (the local file cannot be opened or stat'ed) to its
// caller; the per-write comparison failures stay logged, as this is a
// differential test harness.
func runWithClient(clientset kubernetes.Interface, opts Options, logger *slog.Logger) error {
	vfsN := vfs.NewVFS(context.Background(), clientset, "test", logger, opts.Retries)

	fn := "fakefile.txt"

	f, err := os.OpenFile(fn, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open local file: %w", err)
	}
	defer f.Close()

	fileA := vfs.NewFile(fn, vfsN)

	dualWrite := func(p []byte, off int64) error {
		n1, err1 := f.WriteAt(p, off)
		n2, err2 := fileA.WriteAt(p, off)
		if err1 != nil {
			return err1
		}
		if err2 != nil {
			return err2
		}
		if n1 != n2 {
			return fmt.Errorf("wrong number of bytes written %d, %d", n1, n2)
		}
		return nil
	}

	dualRead := func(pFile []byte, pVFS []byte, off int64) error {
		n1, err1 := f.ReadAt(pFile, off)
		n2, err2 := fileA.ReadAt(pVFS, off)
		if n1 != n2 {
			return fmt.Errorf("wrong no of bytes read %d, %d", n1, n2)
		}
		if err1 != nil {
			return err1
		}
		if err2 != nil {
			return err2
		}
		return nil
	}

	compare := func(p1, p2 []byte, label string) {
		if bytes.Equal(p1, p2) {
			logger.Info("Read the same bytes we wrote")
		} else {
			logger.Error("Failed to read the same bytes we wrote", "label", label)
		}
	}

	toWrite := []byte{1, 2, 3, 4}
	if err := dualWrite(toWrite, 0); err != nil {
		logger.Error("dualWrite", "err", err)
	}

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat local file: %w", err)
	}

	fSize := fi.Size()

	p1 := make([]byte, fSize)
	p2 := make([]byte, fSize)

	if err := dualRead(p1, p2, 0); err != nil {
		logger.Error("dualRead", "err", err)
	}
	compare(p1, p2, "first write")

	toWrite = []byte{1, 2, 3, 4}
	if err := dualWrite(toWrite, 3); err != nil {
		logger.Error("dualWrite", "err", err)
	}

	fi, err = f.Stat()
	if err != nil {
		return fmt.Errorf("stat local file: %w", err)
	}

	fSize = fi.Size()

	p1 = make([]byte, fSize)
	p2 = make([]byte, fSize)

	if err := dualRead(p1, p2, 0); err != nil {
		logger.Error("dualRead", "err", err)
	}
	compare(p1, p2, "offset write")

	randomToWrite := make([]byte, vfs.SectorSize+2)
	rand.Read(randomToWrite)

	// testWriting updates the outer fi/fSize: the offsets of the calls
	// after it are computed from the size the previous write left. A
	// failed stat of the local file fails the run — every offset after it
	// would be computed from a stale size.
	testWriting := func(p []byte, off int64) error {
		if err := dualWrite(p, off); err != nil {
			logger.Error("dualWrite", "err", err)
		}

		fi, err = f.Stat()
		if err != nil {
			return fmt.Errorf("stat local file: %w", err)
		}

		fSize = fi.Size()

		p1 = make([]byte, fSize/2)
		p2 = make([]byte, fSize/2)

		if err := dualRead(p1, p2, fSize/2); err != nil {
			logger.Error("dualRead", "err", err)
		}
		compare(p1, p2, "random write")

		virtualFileSize, _ := fileA.FileSize()
		if fi.Size() != virtualFileSize {
			logger.Error("Mismatched file sizes", "realFile", fi.Size(), "virtualFile", virtualFileSize)
		}
		return nil
	}

	if err := testWriting(randomToWrite, 0); err != nil {
		return err
	}
	randomToWrite = make([]byte, vfs.SectorSize+2)
	rand.Read(randomToWrite)
	if err := testWriting(randomToWrite, fSize-46); err != nil {
		return err
	}
	randomToWrite = make([]byte, vfs.SectorSize+2)
	if err := testWriting(randomToWrite, fSize-46); err != nil {
		return err
	}
	randomToWrite = make([]byte, 2*vfs.SectorSize-1)
	if err := testWriting(randomToWrite, fSize-46-vfs.SectorSize); err != nil {
		return err
	}

	p1 = make([]byte, fSize)
	p2 = make([]byte, fSize)

	if err := dualRead(p1, p2, fSize); err != nil {
		logger.Error("dualRead", "err", err)
	}
	compare(p1, p2, "read at EOF offset")

	for range 10 {
		s := fi.Size()
		offset, _ := rand.Int(rand.Reader, new(big.Int).SetInt64(s))
		length, _ := rand.Int(rand.Reader, new(big.Int).SetInt64(s+1))

		randomToWrite = make([]byte, length.Int64())
		rand.Read(randomToWrite)
		if err := testWriting(randomToWrite, offset.Int64()); err != nil {
			return err
		}

		length, _ = rand.Int(rand.Reader, new(big.Int).SetInt64(fi.Size()))
		off := fi.Size() - length.Int64()

		p1 = make([]byte, length.Int64())
		p2 = make([]byte, length.Int64())

		if err := dualRead(p1, p2, off); err != nil {
			logger.Error("dualRead", "err", err)
		}
		virtualFileSize, _ := fileA.FileSize()
		if fi.Size() != virtualFileSize {
			logger.Error("Mismatched file sizes", "realFile", fi.Size(), "virtualFile", virtualFileSize)
		}
	}
	return nil
}
