// Command db-upload uploads a local SQLite database file to the cluster:
// it reads the local file sector by sector and stores each sector as a
// ConfigMap, under the same name the VFS uses for the file. The local and
// remote names are fixed ("file2.db"); the tools are demo utilities.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/RichardoC/kube-sqlite3-vfs/pkg/stalejournal"
	"github.com/RichardoC/kube-sqlite3-vfs/pkg/vfs"
	"github.com/psanford/sqlite3vfs"

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
// so the tests can drive it — the error paths included — with the fake
// clientset, and it returns the first failure to its caller.
func runWithClient(clientset kubernetes.Interface, opts Options, logger *slog.Logger) error {
	return runWithClientFrom(clientset, opts, logger, openLocalFile)
}

// localFile is what the upload loop needs from the local image: the bytes,
// its size, and a way to close it. *os.File satisfies it; a test can supply
// a reader with other behaviour (a short-reader, an injected error) instead.
type localFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

// openLocalFile opens the local image the upload reads from.
func openLocalFile(name string) (localFile, error) {
	return os.Open(name)
}

// runWithClientFrom is runWithClient with the local-file source injectable,
// so a test can feed a reader that returns short reads — allowed by the
// io.Reader contract and realistic on FUSE, NFS or a file changing underneath
// — without needing a real file that misbehaves.
func runWithClientFrom(clientset kubernetes.Interface, opts Options, logger *slog.Logger, open func(name string) (localFile, error)) error {
	vfsN := vfs.NewVFS(context.Background(), clientset, "test", logger, opts.Retries)

	fn := "file2.db"

	// A writer that crashed mid-transaction leaves its rollback journal
	// ("<db>-journal") on the cluster, and the journal describes the OLD
	// page contents: replaying it over the freshly uploaded image would
	// silently resurrect the old database (issue #352). These tools are
	// documented as not for live databases, so a journal found here cannot
	// belong to a live writer; delete it through the VFS xDelete — which
	// removes the journal header sector first, in the crash-safe order, and
	// is therefore idempotent on a half-deleted journal — BEFORE the first
	// sector of the image is written, never after.
	// The probe classifies the journal as hot, cold, or — when its header
	// cannot be read — hotness unknown, which is logged as a warning rather
	// than reported as a verified-cold journal (issue #509). Either way the
	// name must go before the upload.
	journalName := fn + "-journal"
	if err := stalejournal.DeleteIfPresent(vfsN, vfs.NewFile(journalName, vfsN), journalName, "upload", logger); err != nil {
		return err
	}

	if _, _, err := vfsN.Open(fn, sqlite3vfs.OpenFlag(0)); err != nil {
		return fmt.Errorf("open %q in the cluster: %w", fn, err)
	}

	f, err := open(fn)
	if err != nil {
		return fmt.Errorf("open local file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat local file: %w", err)
	}
	localSize := fi.Size()

	buf := make([]byte, vfs.SectorSize)

	fileA := vfs.NewFile(fn, vfsN)

	// A bare Read may return fewer bytes than the buffer holds without
	// reaching EOF (allowed by io.Reader, and realistic on FUSE, NFS or a
	// file being modified underneath). Writing that partial buffer as a
	// whole sector shifts every later sector to the wrong offset: bytes
	// 124000 onward landed in sector 2 instead of sector 1. io.ReadFull
	// fills the buffer before each sector is written, so a sector is only
	// ever short at the true end of the file. It reports io.EOF with n == 0
	// at a clean end of file, io.ErrUnexpectedEOF for a partial final
	// sector (which must still be written), and any other error means the
	// image cannot be trusted.
	var index int64
read:
	for {
		n, err := io.ReadFull(f, buf)
		switch {
		case err == nil:
			// A full sector.
		case errors.Is(err, io.EOF) && n == 0:
			// A clean end of file, with no partial sector to write.
			break read
		case errors.Is(err, io.ErrUnexpectedEOF):
			// The file ends inside this sector: write the partial sector
			// and stop. The size check below still verifies the whole image
			// landed.
		default:
			return fmt.Errorf("read %s: %w", fn, err)
		}
		logger.Info("Writing sector", "sector", index)
		sect := &vfs.Sector{Index: index, Data: buf[:n]}
		if err := fileA.WriteSector(sect); err != nil {
			return fmt.Errorf("write sector %d: %w", index, err)
		}
		index++
		if errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
	}

	// WriteSector only creates and overwrites sectors: uploading a smaller
	// file over a larger remote left every trailing sector of the old
	// database in place, so FileSize kept reporting the old size and the
	// tail held stale bytes (issue #332). Truncate deletes the sectors past
	// the new end the way SQLite itself shortens a file, and the size check
	// afterwards fails the run rather than leaving a silently stale remote.
	if err := fileA.Truncate(localSize); err != nil {
		return fmt.Errorf("truncate remote to %d bytes: %w", localSize, err)
	}

	rSize, err := fileA.FileSize()
	if err != nil {
		return fmt.Errorf("remote FileSize: %w", err)
	}
	if rSize != localSize {
		return fmt.Errorf("size mismatch: local file is %d bytes, remote file is %d bytes", localSize, rSize)
	}
	logger.Info("Local and remote file sizes are the same", "size", localSize)

	return nil
}
