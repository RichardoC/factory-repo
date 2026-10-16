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

// fatal prints err to stderr and exits non-zero. The command line tools
// never panic on ordinary errors: a panic unwinds through the cgo sqlite
// boundary and kills the process without a usable message.
func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
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
		fatal(err)
	}

	logger := newLogger(opts.Verbose)
	slog.SetDefault(logger)

	logger.Info("Got config", "opts", opts)

	if wd, err := os.Getwd(); err == nil {
		logger.Info("working directory", "dir", wd)
	}

	var kubeconfig string
	if opts.KubeConfig != "" {
		kubeconfig = opts.KubeConfig
	} else if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fatal(err)
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fatal(err)
	}

	vfsN := vfs.NewVFS(context.Background(), clientset, "test", logger, opts.Retries)

	fn := "fakefile.txt"

	f, err := os.OpenFile("fn", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fatal(err)
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
		fatal(err)
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
		fatal(err)
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
	// after it are computed from the size the previous write left.
	testWriting := func(p []byte, off int64) {
		if err := dualWrite(p, off); err != nil {
			logger.Error("dualWrite", "err", err)
		}

		fi, err = f.Stat()
		if err != nil {
			fatal(err)
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
	}

	testWriting(randomToWrite, 0)
	randomToWrite = make([]byte, vfs.SectorSize+2)
	rand.Read(randomToWrite)
	testWriting(randomToWrite, fSize-46)
	randomToWrite = make([]byte, vfs.SectorSize+2)
	testWriting(randomToWrite, fSize-46)
	randomToWrite = make([]byte, 2*vfs.SectorSize-1)
	testWriting(randomToWrite, fSize-46-vfs.SectorSize)

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
		testWriting(randomToWrite, offset.Int64())

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
}
