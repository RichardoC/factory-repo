// Command db-download "uploads" a local SQLite database file into the
// cluster through the VFS's WriteAt (the reverse of db-upload's
// sector-by-sector upload), verifies that the stored bytes and size match
// the local file, and then reads the row count back through database/sql
// over the VFS. The remote file name is fixed ("file2.db").
package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/RichardoC/kube-sqlite3-vfs/pkg/vfs"
	_ "github.com/mattn/go-sqlite3"
	"github.com/psanford/sqlite3vfs"
	"github.com/thought-machine/go-flags"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// Options are the command's flags.
type Options struct {
	KubeConfig string `long:"kubeconfig" description:"(optional) absolute path to the kubeconfig file"`
	FileName   string `long:"filename" description:"local sqlite3 database file to store in the cluster" default:"file2.db"`
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
	// f.Close and db.Close. A failure now unwinds through run's defers and
	// leaves the exit code to main.
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
// clientset, and it returns the first failure to its caller: a read,
// write, size or byte mismatch used to be logged and the process still
// exited zero on a half-failed transfer.
func runWithClient(clientset kubernetes.Interface, opts Options, logger *slog.Logger) error {
	vfsN := vfs.NewVFS(context.Background(), clientset, "test", logger, opts.Retries)

	fn := "file2.db"

	if _, _, err := vfsN.Open(fn, sqlite3vfs.OpenFlag(0)); err != nil {
		return fmt.Errorf("open %q in the cluster: %w", fn, err)
	}

	f, err := os.Open(opts.FileName)
	if err != nil {
		return fmt.Errorf("open local file: %w", err)
	}
	defer f.Close()

	fileA := vfs.NewFile(fn, vfsN)

	fi, err := f.Stat()
	if err != nil {
		// Could not obtain stat, handle error
		return fmt.Errorf("stat local file: %w", err)
	}

	fSize := fi.Size()

	allBytesReal := make([]byte, fSize)

	n, err := f.Read(allBytesReal)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read %s: %w", opts.FileName, err)
	}
	if n != len(allBytesReal) {
		return fmt.Errorf("failed to read all of %s: read %d of %d bytes", opts.FileName, n, fSize)
	}

	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("rewind %s: %w", opts.FileName, err)
	}

	n, err = fileA.WriteAt(allBytesReal, 0)
	if err != nil {
		return fmt.Errorf("failed to write the source file to the cluster: %w (wrote %d of %d bytes)", err, n, fSize)
	}
	if n != int(fSize) {
		return fmt.Errorf("failed to write all of the source file: wrote %d of %d bytes", n, fSize)
	}

	// Confirm that remote and local files the same size

	rSize, err := fileA.FileSize()
	if err != nil {
		return fmt.Errorf("remote FileSize: %w", err)
	}

	if fSize != rSize {
		return fmt.Errorf("size mismatch: local file is %d bytes, remote file is %d bytes", fSize, rSize)
	}
	logger.Info("Local and remote file sizes are the same")

	// Confirm that the remote file is the same as the local one

	allBytes := make([]byte, fSize)

	bytesRead, err := fileA.ReadAt(allBytes, 0)
	// A read that stops at the exact end of the file reports io.EOF; with
	// every byte read that is success, not failure.
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read back from the cluster: %w", err)
	}
	if int64(bytesRead) != fSize {
		return fmt.Errorf("failed to read all bytes back: read %d of %d bytes", bytesRead, fSize)
	}

	if bytes.Equal(allBytesReal, allBytes) {
		logger.Info("Read the same bytes we wrote")
	} else {
		return errors.New("the bytes read back from the cluster differ from the local file")
	}

	// register the custom kube-sqlite3-vfs vfs with sqlite
	// the name specifed here must match the `vfs` param
	// passed to sql.Open in the dataSourceName:
	// e.g. `...?vfs=kube-sqlite3-vfs`
	if err := sqlite3vfs.RegisterVFS("kube-sqlite3-vfs", vfsN); err != nil {
		return fmt.Errorf("register VFS: %w", err)
	}

	// The journal mode is DELETE, not MEMORY (issue #68): MEMORY leaves no
	// on-cluster record of an uncommitted transaction, so a crash cannot be
	// rolled back. Click through the same reasoning as main.go: DELETE pays
	// one xDelete at commit (one LIST + one Delete per journal sector),
	// where TRUNCATE/PERSIST keep the file and pay a header rewrite plus
	// extra xAccess/xOpen on later transactions. The measured api-calls/op
	// for a committed 1000-row transaction is 25.00 with _journal=MEMORY
	// and 65.00 with _journal=DELETE (module README, BenchmarkSQLiteInsert1000Rows).
	//
	// This connection is read-only (`mode=ro` and `_query_only=true`), and
	// that changes what it can and cannot do, so it is stated here rather
	// than left to the reader:
	//
	//   - a read-only connection performs no journal WRITE, so it can never
	//     create or delete a journal: it cannot commit a transaction and so
	//     never has one to roll back. The mode named here is therefore what
	//     the file is read as, not a write it performs.
	//   - it still RECOVERS: opening a database with a hot journal (a
	//     writer that crashed mid-transaction) in mode=ro, SQLite reads
	//     through the journal and sees the pre-transaction data, leaving
	//     the journal file itself on the cluster for the next read-write
	//     opener to remove. Measured against a real file with a SIGKILLed
	//     writer: the read-only opener saw the old value and
	//     PRAGMA integrity_check=ok, and the -journal file was still there
	//     afterwards.
	//
	// So the setting is deliberately DELETE on a connection that cannot
	// write journals: forcing DELETE blindly would be a request this mode
	// cannot honour, while MEMORY would be wrong the other way — it says
	// "do not consult the journal", which is exactly what makes a crashed
	// writer's half-written database look committed. Nothing here writes
	// the journal; the writer side (main.go) does, and it uses DELETE so
	// the recovery above has something to read through.
	db, err := sql.Open("sqlite3", "file2.db?_query_only=true&_journal=DELETE&mode=ro&sqlite_os_trace=&vfs=kube-sqlite3-vfs")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(1)

	rows, err := db.Query("SELECT COUNT(*) FROM books")
	if err != nil {
		return fmt.Errorf("query row count: %w", err)
	}
	defer rows.Close()
	logger.Info("queried rows", "rows", rows)
	var count int
	// The row count is the command's final check, so a failed read of it
	// fails the run rather than being logged away. A count query always
	// yields exactly one row: a false Next is an error (rows.Err), not an
	// empty result.
	if !rows.Next() {
		return fmt.Errorf("scan row count: no row: %w", rows.Err())
	}
	if err := rows.Scan(&count); err != nil {
		return fmt.Errorf("scan row count: %w", err)
	}
	logger.Info("Got rows of books", "count", count)
	return nil
}
