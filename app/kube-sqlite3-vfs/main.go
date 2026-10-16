// Command kube-sqlite3-vfs demonstrates the VFS end to end: it registers
// the kube-sqlite3-vfs VFS with sqlite, creates a books table in a database
// stored as ConfigMaps in the cluster, inserts 1000 rows and reads them
// back. The cmd tools build on the same VFS to upload, download and compare
// whole database files.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/RichardoC/kube-sqlite3-vfs/pkg/vfs"
	"github.com/google/uuid"
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
	// db.Close and stmt.Close. A failure now unwinds through run's defers
	// and leaves the exit code to main.
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
	vfsN := vfs.NewVFS(context.Background(), clientset, "test", logger, opts.Retries)

	// Register the custom kube-sqlite3-vfs VFS with sqlite. The name
	// specified here must match the `vfs` param passed to sql.Open in the
	// dataSourceName, e.g. `...?vfs=kube-sqlite3-vfs`.
	if err := sqlite3vfs.RegisterVFS("kube-sqlite3-vfs", vfsN); err != nil {
		return fmt.Errorf("register VFS: %w", err)
	}

	// file2 is the name of the file stored in kubernetes. The
	// `vfs=kube-sqlite3-vfs` instructs sqlite to use the custom VFS
	// implementation; the name must match the one passed to
	// sqlite3vfs.RegisterVFS above.
	db, err := sql.Open("sqlite3", "file2.db?_journal=MEMORY&_cache_size=-256&vfs=kube-sqlite3-vfs")
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	if _, err := db.Exec("PRAGMA page_size=65536"); err != nil {
		return fmt.Errorf("set page size: %w", err)
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS books (
	id text NOT NULL PRIMARY KEY,
	title text
	)`); err != nil {
		return fmt.Errorf("create books table: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	totalToInsert := 1000
	for i := range totalToInsert {
		if _, err := stmt.Exec(uuid.NewString(), fmt.Sprintf("%d", i)); err != nil {
			// A failed insert must not reach the commit below: roll back
			// and fail the run. SQLite also rolls back by itself on some
			// errors (SQLITE_IOERR among them); a second Rollback on the
			// already-rolled-back transaction reports ErrTxDone, which is
			// not a failure.
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				logger.Error("rollback after failed insert", "row", i, "err", rbErr)
			}
			return fmt.Errorf("insert row %d: %w", i, err)
		}
		if (10*i)%totalToInsert == 0 {
			logger.Warn("inserted", "row", i, "of", totalToInsert)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	rows, err := db.Query("SELECT COUNT(*) FROM books")
	if err != nil {
		return fmt.Errorf("count books: %w", err)
	}

	var count int
	rows.Next()
	err = rows.Scan(&count)
	logger.Info("Got rows of books", "count", count, "err", err)

	// explicitly close out the old query since we only allow one at a time
	rows.Close()

	rows, err = db.Query("SELECT id, title FROM books LIMIT 100")
	if err != nil {
		return fmt.Errorf("query books: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   string
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			logger.Error("scan row", "err", err)
			continue
		}
		logger.Info("book", "id", id, "name", name)
	}
	return nil
}
