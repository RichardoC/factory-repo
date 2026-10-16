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
	"fmt"
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

	fn := "file2.db"

	if _, _, err := vfsN.Open(fn, sqlite3vfs.OpenFlag(0)); err != nil {
		fatal(err)
	}

	f, err := os.Open(opts.FileName)
	if err != nil {
		fatal(err)
	}
	defer f.Close()

	fileA := vfs.NewFile(fn, vfsN)

	fi, err := f.Stat()
	if err != nil {
		// Could not obtain stat, handle error
		fatal(err)
	}

	fSize := fi.Size()

	allBytesReal := make([]byte, fSize)

	n, err := f.Read(allBytesReal)
	if err != nil || n != int(fSize) {
		logger.Error("Failed to read all source file", "err", err, "bytesRead", n)
	}

	f.Seek(0, 0)

	n, err = fileA.WriteAt(allBytesReal, 0)

	if err != nil || n != int(fSize) {
		logger.Error("Failed to write all source file", "err", err, "bytesWritten", n, "trueLength", fSize)
	}

	// Confirm that remote and local files the same size

	rSize, err := fileA.FileSize()

	if err != nil {
		logger.Error("FileSize", "err", err)
	}

	if fSize != rSize {
		logger.Error("Size mismatch", "localFileSize", fSize, "remotefileSize", rSize)
	} else {
		logger.Info("Local and remote file sizes are the same")
	}

	// Confirm that the remote file is the same as the local one

	allBytes := make([]byte, fSize)

	bytesRead, err := fileA.ReadAt(allBytes, 0)

	if err != nil {
		logger.Error("ReadAt", "err", err)
	}
	if int64(bytesRead) != fSize {
		logger.Error("Failed to read all bytes", "readBytes", bytesRead, "trueLengthBytes", fSize)
	}

	if bytes.Equal(allBytesReal, allBytes) {
		logger.Info("Read the same bytes we wrote")
	} else {
		logger.Error("Failed to read the same bytes we wrote")
	}

	// register the custom kube-sqlite3-vfs vfs with sqlite
	// the name specifed here must match the `vfs` param
	// passed to sql.Open in the dataSourceName:
	// e.g. `...?vfs=kube-sqlite3-vfs`
	if err := sqlite3vfs.RegisterVFS("kube-sqlite3-vfs", vfsN); err != nil {
		fatal(err)
	}

	db, err := sql.Open("sqlite3", "file2.db?_query_only=true&_journal=MEMORY&mode=ro&sqlite_os_trace=&vfs=kube-sqlite3-vfs")
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	db.SetMaxOpenConns(1)

	rows, err := db.Query("SELECT COUNT(*) FROM books")
	if err != nil {
		fatal(err)
	}
	defer rows.Close()
	logger.Info("queried rows", "rows", rows)
	var count int
	rows.Next()
	err = rows.Scan(&count)
	logger.Info("Got rows of books", "count", count, "err", err)
}
