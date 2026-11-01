// Command db-download replaces the remote image with a local SQLite
// database file: it writes the whole file into the cluster through the
// VFS's WriteAt (the reverse of db-upload's sector-by-sector upload),
// truncates the remote to the local size so no sector of a larger previous
// image survives, verifies that the stored bytes and size match the local
// file, and then reads the row count back through database/sql over the
// VFS. The remote file name is fixed ("file2.db").
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

	"github.com/RichardoC/kube-sqlite3-vfs/pkg/journalmode"
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
	KubeConfig  string `long:"kubeconfig" description:"(optional) absolute path to the kubeconfig file"`
	FileName    string `long:"filename" description:"local sqlite3 database file to store in the cluster" default:"file2.db"`
	JournalMode string `long:"journal-mode" description:"SQLite rollback journal mode this connection reads the database as: DELETE (default), TRUNCATE or PERSIST. Validated by the same rules as the writer's flag (MEMORY, OFF and WAL are refused); set it to the writer's --journal-mode so reader and writer agree"`
	Verbose     bool   `long:"verbosity" short:"v" description:"Log at debug level rather than info"`
	Retries     int    `long:"retries" description:"Number of retries for API calls" default:"1"`
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

	// Refuse an unusable --journal-mode before any cluster work, exactly
	// as the writer's main does — the validation in runWithClient would
	// otherwise only fire after the kubeconfig has been read and a
	// clientset built.
	if _, err := journalModeName(opts.JournalMode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// The only three os.Exit calls in the process are the flag-parse failure
	// above, the journal-mode refusal above, and this one, all before or
	// after run has returned: the old
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

// journalModeName validates --journal-mode for this command, delegating to
// the shared journalmode package so the reader and the writer (main.go)
// accept and refuse exactly the same values (issue #318).
func journalModeName(mode string) (string, error) {
	return journalmode.Name(mode)
}

// readerDSN builds the DSN this command opens the remote database with,
// with journalMode validated by journalModeName — the same shared
// validation the writer (main.go) applies, so reader and writer agree
// (issue #318). The chosen mode is carried to SQLite as the _journal DSN
// parameter, exactly the way the flag reaches the database.
func readerDSN(journalMode string) (string, error) {
	mode, err := journalModeName(journalMode)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("file2.db?_query_only=true&_journal=%s&mode=ro&sqlite_os_trace=&vfs=kube-sqlite3-vfs", mode), nil
}

// runWithClient holds the command's body. It takes a kubernetes.Interface
// so the tests can drive it — the error paths included — with the fake
// clientset, and it returns the first failure to its caller: a read,
// write, size or byte mismatch used to be logged and the process still
// exited zero on a half-failed transfer.
func runWithClient(clientset kubernetes.Interface, opts Options, logger *slog.Logger) error {
	// Validate --journal-mode and build the DSN before any cluster work, so
	// an unusable mode fails before the kubeconfig is read, a clientset is
	// built or a sector is written — the same front-door refusal main.go
	// gives its writer.
	dsn, err := readerDSN(opts.JournalMode)
	if err != nil {
		return err
	}

	vfsN := vfs.NewVFS(context.Background(), clientset, "test", logger, opts.Retries)

	fn := "file2.db"

	// A writer that crashed mid-transaction leaves its rollback journal
	// ("<db>-journal") on the cluster, and the journal describes the OLD
	// page contents: replaying it over the freshly written image would
	// silently resurrect the old database (issue #352). These tools are
	// documented as not for live databases, so a journal found here cannot
	// belong to a live writer; delete it through the VFS xDelete — which
	// removes the journal header sector first, in the crash-safe order, and
	// is therefore idempotent on a half-deleted journal — BEFORE the image
	// is written, never after. This also has to precede the read-only open
	// at the end of this command: a read-only opener would not delete the
	// journal, only read through it.
	journalName := fn + "-journal"
	exists, err := vfsN.Access(journalName, sqlite3vfs.AccessExists)
	if err != nil {
		return fmt.Errorf("look for a stale journal %q: %w", journalName, err)
	}
	if exists {
		// SQLite calls a journal hot when the first bytes at offset 0 (the
		// journal magic) are non-zero; a missing or zeroed header reads as
		// a cold journal. Either way the name must go before the transfer.
		hot := false
		var head [8]byte
		n, readErr := vfs.NewFile(journalName, vfsN).ReadAt(head[:], 0)
		if n > 0 && (readErr == nil || errors.Is(readErr, io.EOF)) {
			for _, b := range head[:n] {
				if b != 0 {
					hot = true
					break
				}
			}
		}
		if err := vfsN.Delete(journalName, false); err != nil {
			return fmt.Errorf("delete stale journal %q: %w", journalName, err)
		}
		if hot {
			logger.Warn("Deleted a HOT rollback journal left by a crashed writer: the download replaces the database outright, so the journal was removed rather than replayed", "journal", journalName)
		} else {
			logger.Info("Deleted a stale (cold) rollback journal", "journal", journalName)
		}
	}

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

	// WriteAt only creates and overwrites sectors: downloading a smaller
	// file over a larger remote left every trailing sector of the old
	// database in place, so FileSize kept reporting the old size and the
	// size check below failed the run (issue #413). Truncate deletes the
	// sectors past the new end the way SQLite itself shortens a file,
	// symmetrically with db-upload's fix for the same problem on the
	// reverse path (issue #332).
	if err := fileA.Truncate(fSize); err != nil {
		return fmt.Errorf("truncate remote to %d bytes: %w", fSize, err)
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

	// The journal mode comes from --journal-mode (issue #318), validated by
	// the same shared rules as the writer's flag, so an operator running
	// the writer with, say, TRUNCATE can align the reader with it; the
	// default is DELETE, and not MEMORY (issue #68): MEMORY leaves no
	// on-cluster record of an uncommitted transaction, so a crash cannot be
	// rolled back. DELETE measures 65.00 api-calls/op for a committed
	// 1000-row transaction against MEMORY's 25.00; TRUNCATE and PERSIST
	// measure slightly cheaper still (61.00/62.00), but leave journal
	// state on the cluster after a clean commit where DELETE leaves
	// nothing, which is why DELETE is the project-wide default — the full
	// measured table is in the module README (issue #120,
	// BenchmarkSQLiteInsert1000RowsJournalModes).
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
	// So the mode named here is deliberately one of DELETE, TRUNCATE or
	// PERSIST on a connection that cannot write journals: each keeps a
	// journal SQLite can read through, while MEMORY would be wrong the
	// other way — it says "do not consult the journal", which is exactly
	// what makes a crashed writer's half-written database look committed.
	// Nothing here writes the journal; the writer side (main.go) does,
	// under whichever --journal-mode its operator chose (default DELETE),
	// so the recovery above has something to read through.
	db, err := sql.Open("sqlite3", dsn)
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
