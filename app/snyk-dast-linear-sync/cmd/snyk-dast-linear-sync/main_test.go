package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tesslio/snyk-dast-linear-sync/internal/cache"
)

// TestSIGINTCancelsRunAndLeavesCacheUntouched runs the built binary against
// local stubs that hang, sends it SIGINT after a second, and asserts the three
// externally visible effects of an interrupted run: a non-zero exit, a
// "shutdown requested" line on stderr, and a SQLite cache with no rows — the
// run must not have described its never-written work as reconciled.
func TestSIGINTCancelsRunAndLeavesCacheUntouched(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the end-to-end signal test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal delivery is not available on windows")
	}

	bin := filepath.Join(t.TempDir(), "snyk-dast-linear-sync")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test binary: %v\n%s", err, out)
	}

	// The Snyk DAST API hangs, so the run is mid-load when the signal arrives.
	// The Linear GraphQL endpoint is hard-coded in the client, so it is kept
	// local too: the proxy variables point at a proxy that hangs, and Go's
	// proxy rules never route loopback addresses (the Snyk DAST stub) through
	// a proxy. Nothing leaves this machine.
	hang := make(chan struct{})
	snykdastStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	proxyStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	// close(hang) must run before the stubs are closed, or Close waits forever
	// on the handlers still blocked in <-hang.
	defer snykdastStub.Close()
	defer proxyStub.Close()
	defer close(hang)

	tmp := t.TempDir()
	cacheDB := filepath.Join(tmp, "cache.sqlite3")
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"SNYK_DAST_API_BASE="+snykdastStub.URL,
		"SNYK_DAST_API_KEY=test-key",
		"SNYK_DAST_APP_BASE=https://dast.example.test",
		"SNYK_DAST_TEAM=",
		"LINEAR_API_KEY=test-key",
		"LINEAR_TEAM_ID=team-a",
		"HTTPS_PROXY="+proxyStub.URL,
		"HTTP_PROXY="+proxyStub.URL,
		"CACHE_DB_FILE="+cacheDB,
		"ERROR_LOG_FILE="+filepath.Join(tmp, "error.log"),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	time.Sleep(time.Second)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("send SIGINT: %v", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err == nil {
			t.Fatalf("binary exited 0 after SIGINT, want a non-zero exit\nstderr:\n%s", stderr.String())
		}
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			t.Fatalf("binary exited for an unexpected reason: %v", err)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("binary did not exit after SIGINT within 15s")
	}
	if out := stderr.String(); !strings.Contains(out, "shutdown requested") {
		t.Fatalf("stderr does not contain %q:\n%s", "shutdown requested", out)
	}

	store, err := cache.Open(cacheDB)
	if err != nil {
		t.Fatalf("open interrupted run's cache: %v", err)
	}
	defer store.Close()
	snapshot, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load interrupted run's cache: %v", err)
	}
	if len(snapshot.SnykDASTHashes) != 0 || len(snapshot.LinearHashes) != 0 {
		t.Fatalf("cache has rows after an interrupted run: %d snyk dast, %d linear, want 0 and 0",
			len(snapshot.SnykDASTHashes), len(snapshot.LinearHashes))
	}
}
