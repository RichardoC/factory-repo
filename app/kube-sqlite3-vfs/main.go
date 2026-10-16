// Command kube-sqlite3-vfs demonstrates the VFS end to end: it registers
// the kube-sqlite3-vfs VFS with sqlite, creates a books table in a database
// stored as ConfigMaps in the cluster, inserts 1000 rows and reads them
// back. The cmd tools build on the same VFS to upload, download and compare
// whole database files.
package main

import (
	"context"
	"database/sql"
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

	// Register the custom kube-sqlite3-vfs VFS with sqlite. The name
	// specified here must match the `vfs` param passed to sql.Open in the
	// dataSourceName, e.g. `...?vfs=kube-sqlite3-vfs`.
	if err := sqlite3vfs.RegisterVFS("kube-sqlite3-vfs", vfsN); err != nil {
		fatal(err)
	}

	// file2 is the name of the file stored in kubernetes. The
	// `vfs=kube-sqlite3-vfs` instructs sqlite to use the custom VFS
	// implementation; the name must match the one passed to
	// sqlite3vfs.RegisterVFS above.
	db, err := sql.Open("sqlite3", "file2.db?_journal=MEMORY&_cache_size=-256&vfs=kube-sqlite3-vfs")
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec("PRAGMA page_size=65536"); err != nil {
		fatal(err)
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS books (
	id text NOT NULL PRIMARY KEY,
	title text
	)`); err != nil {
		fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		fatal(err)
	}
	defer stmt.Close()
	totalToInsert := 1000
	for i := range totalToInsert {
		if _, err := stmt.Exec(uuid.NewString(), fmt.Sprintf("%d", i)); err != nil {
			logger.Error("insert", "row", i, "err", err)
		}
		if (10*i)%totalToInsert == 0 {
			logger.Warn("inserted", "row", i, "of", totalToInsert)
		}
	}

	if err := tx.Commit(); err != nil {
		fatal(err)
	}

	rows, err := db.Query("SELECT COUNT(*) FROM books")
	if err != nil {
		fatal(err)
	}

	var count int
	rows.Next()
	err = rows.Scan(&count)
	logger.Info("Got rows of books", "count", count, "err", err)

	// explicitly close out the old query since we only allow one at a time
	rows.Close()

	rows, err = db.Query("SELECT id, title FROM books LIMIT 100")
	if err != nil {
		fatal(err)
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
}
