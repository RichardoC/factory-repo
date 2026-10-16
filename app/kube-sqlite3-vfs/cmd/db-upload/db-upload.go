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

	f, err := os.Open(fn)
	if err != nil {
		fatal(err)
	}
	defer f.Close()

	buf := make([]byte, vfs.SectorSize)

	fileA := vfs.NewFile(fn, vfsN)

	var index int64
	for {
		n, err := f.Read(buf)
		if n == 0 && errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			fatal(err)
		}
		logger.Info("Writing sector", "sector", index)
		sect := &vfs.Sector{Index: index, Data: buf[:n]}
		if err := fileA.WriteSector(sect); err != nil {
			fatal(err)
		}
		index++
	}
}
