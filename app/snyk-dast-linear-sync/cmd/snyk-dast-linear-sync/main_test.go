package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tesslio/snyk-dast-linear-sync/internal/cache"
)

// testLeafCertPEM/testLeafKeyPEM are a throwaway key pair generated with the
// standard library's crypto/tls/generate_cert.go for the host api.linear.app. It
// is a test fixture for the local MITM proxy below and is not used anywhere
// else; the certificate is expired the moment this test stops pretending.
const testLeafCertPEM = `-----BEGIN CERTIFICATE-----
MIIBdDCCARmgAwIBAgIRAI79e+nj0fmFAqTtSw76B2cwCgYIKoZIzj0EAwIwEjEQ
MA4GA1UEChMHQWNtZSBDbzAeFw0yNjEwMTgxOTA5MjZaFw0yNzEwMTgxOTA5MjZa
MBIxEDAOBgNVBAoTB0FjbWUgQ28wWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAATs
3le1EyfTrfGZjUgEFyBo3DBzJHHIpwb383NsjGFOO/7oXkCI/z374GoLNPhE1xuA
S8168IVA8dugZbLC4WMgo1AwTjAOBgNVHQ8BAf8EBAMCB4AwEwYDVR0lBAwwCgYI
KwYBBQUHAwEwDAYDVR0TAQH/BAIwADAZBgNVHREEEjAQgg5hcGkubGluZWFyLmFw
cDAKBggqhkjOPQQDAgNJADBGAiEA8bz92bOi7cAswrTU7asn2eZR8glt2h0c6Ssy
Nd8ZfGkCIQDqZagYVmJBJweByV+uVm5NUkuOS4LFXuy43s1eaP3cxg==
-----END CERTIFICATE-----`

const testLeafKeyPEM = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQghgxU23hazqJAoLpg
vvvE7lpglyJbPyyG7KH3j6Kv7PmhRANCAATs3le1EyfTrfGZjUgEFyBo3DBzJHHI
pwb383NsjGFOO/7oXkCI/z374GoLNPhE1xuAS8168IVA8dugZbLC4WMg
-----END PRIVATE KEY-----`

// TestSIGINTCancelsRunAndLeavesCacheUntouched is a control twin: it runs the
// same binary twice, once to completion and once interrupted, and compares the
// caches the two runs leave behind.
//
// WHY the pair has power. The interrupted-run arm on its own is vacuous: a
// binary that simply cannot write a cache — a pre-fix binary, a broken build, a
// fixture the code refuses to parse — also exits non-zero with zero rows, so
// "0 rows after SIGINT" proves nothing about the shutdown path. The completion
// arm removes that possibility: the identical binary, with the identical stalls
// in its way but nothing to signal it, exits 0, logs the cache refresh and
// leaves a row in both tables. So the SIGINT arm's zero rows cannot come from a
// binary that never manages to write a cache at all; they come from the
// interrupt path deliberately not refreshing it. Evidence that a run *does*
// write its cache is what gives "it did not write one" its meaning.
//
// WHY the signal must land inside the worker pool with a write in flight, and
// then leave the pool draining cleanly. Two things have to be true at once for
// this test to pin the guard in Run rather than some other exit:
//
//   - The run must be past its snapshot load when the signal arrives. A signal
//     that lands during loading returns out of Run's load phase long before the
//     cache save and exercises nothing, which is exactly how this test lost its
//     power once. So the Snyk DAST stub answers at once and the signal is sent
//     only once the Linear stub reports a state-changing write (issueUpdateBatch
//     or issueCreateBatch) has arrived.
//
//   - g.Wait() must return nil. The guard under test is only reached when the
//     worker pool drains without an error, because Run returns earlier for any
//     other outcome: a non-nil wait error on the line above the guard, and even
//     a guard-disabled run then never reaches the cache save. So the stub does
//     not let the interrupted write fail: the request carrying it is parked
//     across the signal and answered successfully afterwards, exactly as a
//     Linear call that had already committed its write behaves. The worker's
//     in-flight call therefore SUCCEEDS, the pool sees no error, and the run
//     arrives at the guard with the context cancelled and nothing to report.
//
// Only then is the guard load-bearing: an interrupted run has written only part
// of its plan, so the findings whose jobs never ran would look reconciled in a
// refreshed cache simply because their ticket was untouched. With the guard
// disabled the run would instead fall through to the cache save and log
// "refreshed sync cache" with rows in both tables, and this test's log and row
// assertions fail.
//
// The two arms share the built binary and nothing else: separate cache files,
// separate stubs and separate processes.
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

	// A run that finishes. It writes the cache and logs the refresh, which is
	// what makes the interrupted run's silence meaningful.
	t.Run("control run refreshes the cache", func(t *testing.T) {
		tmp := t.TempDir()
		env, err := startStubRun(t, tmp, stubModeAnswer)
		if err != nil {
			t.Fatalf("build control stubs: %v", err)
		}

		stdout, stderr, exitErr := runBinary(t, bin, env, nil, 60*time.Second)
		if exitErr != nil {
			t.Fatalf("control run exited with %v, want exit 0\nstderr:\n%s\nstdout:\n%s", exitErr, stderr, stdout)
		}
		if !strings.Contains(stdout+stderr, "refreshed sync cache") {
			t.Fatalf("control run did not log %q, so the interrupted arm cannot assert its absence\nstderr:\n%s\nstdout:\n%s",
				"refreshed sync cache", stderr, stdout)
		}

		snapshot := loadCache(t, env.cacheDB)
		if len(snapshot.SnykDASTHashes) == 0 {
			t.Fatalf("control run wrote no Snyk DAST cache rows, so the interrupted arm's 0 rows prove nothing\nstderr:\n%s", stderr)
		}
		if len(snapshot.LinearHashes) == 0 {
			t.Fatalf("control run wrote no Linear cache rows\nstderr:\n%s", stderr)
		}
		t.Logf("control run wrote %d Snyk DAST rows and %d Linear rows", len(snapshot.SnykDASTHashes), len(snapshot.LinearHashes))
	})

	// The interrupted run. The Snyk DAST stub answers at once, so loading is
	// over quickly and cannot be what the signal lands on; the Linear stub
	// holds state-changing writes open, and the signal is sent only after one
	// has arrived, so the run is inside the worker pool when it is interrupted.
	t.Run("interrupted run leaves the cache untouched", func(t *testing.T) {
		tmp := t.TempDir()
		env, err := startStubRun(t, tmp, stubModeHoldWrite)
		if err != nil {
			t.Fatalf("build write-holding stubs: %v", err)
		}

		stdout, stderr, exitErr := runBinary(t, bin, env, func(proc *os.Process) error {
			select {
			case <-env.writeInFlight:
				// Provable precondition: the held write reached the Linear
				// stub, so the run is past the load phase and inside the
				// worker pool. A signal sent here cannot be the load-phase
				// early return that this test used to pin by accident.
				t.Logf("held Linear write %q is in flight; sending SIGINT", env.heldOperation())
			case <-time.After(30 * time.Second):
				return fmt.Errorf("the run never reached the worker pool: no Linear write was held within 30s, so the run was blocked before its worker pool sent one (expected %q or %q)",
					"issueCreateBatch", "issueUpdateBatch")
			}
			if err := proc.Signal(syscall.SIGINT); err != nil {
				return err
			}
			// Let the signals settle and the Linear stub answer the write it
			// parked: the write then completes, the interrupted call returns no
			// error, and the worker pool drains with g.Wait() == nil — the only
			// state in which Run reaches the interruption guard. Sleeping here
			// is not the signal's clock (the signal is already delivered); it
			// only makes the run past the signal before the write is released,
			// so a guard-disabled run cannot win the race by refreshing the
			// cache before the cancellation lands.
			time.Sleep(500 * time.Millisecond)
			releaseSIGINTSignal()
			releaseHeldWrites()
			return nil
		}, 60*time.Second)
		if exitErr == nil {
			t.Fatalf("interrupted run exited 0, want a non-zero exit\nstderr:\n%s", stderr)
		}
		if _, ok := errors.AsType[*exec.ExitError](exitErr); !ok {
			t.Fatalf("interrupted run ended for an unexpected reason: %v", exitErr)
		}
		if !strings.Contains(stderr, "shutdown requested") {
			t.Fatalf("stderr does not contain %q:\n%s", "shutdown requested", stderr)
		}
		// A normal completion always logs this, so its absence is real evidence
		// that the run stopped before the cache refresh rather than racing one.
		if strings.Contains(stdout+stderr, "refreshed sync cache") {
			t.Fatalf("interrupted run logged %q, so it refreshed the cache\nstderr:\n%s", "refreshed sync cache", stderr)
		}
		if !env.writeWasHeld() {
			t.Fatalf("the run never reached the worker pool with a write in flight, so a zero-row cache proves nothing about the interruption guard")
		}
		// The run must have reached the guard with the context cancelled and a
		// clean worker pool, not returned earlier on a wait error: a guard
		// replaced by `if false` would have saved the cache and logged the
		// refresh, which the assertions above catch.
		if !strings.Contains(stderr, "sync interrupted before all work was written") {
			t.Fatalf("interrupted run did not report the interruption guard's error, so it left Run somewhere else\nstderr:\n%s\nstdout:\n%s", stderr, stdout)
		}

		snapshot := loadCache(t, env.cacheDB)
		if len(snapshot.SnykDASTHashes) != 0 || len(snapshot.LinearHashes) != 0 {
			t.Fatalf("cache has rows after an interrupted run: %d snyk dast, %d linear, want 0 and 0",
				len(snapshot.SnykDASTHashes), len(snapshot.LinearHashes))
		}
	})
}

// stubMode selects how the two stubs treat the requests the binary sends.
type stubMode int

const (
	// stubModeAnswer answers every request with a small, valid fixture set, so
	// the run completes and writes its cache. It is the control arm.
	stubModeAnswer stubMode = iota
	// stubModeHoldWrite answers the Snyk DAST requests and every Linear
	// operation at once, except one state-changing write, which is held open
	// until the run is cancelled. Loading therefore completes and the run is
	// inside the worker pool when the held write is in flight.
	stubModeHoldWrite
)

// writeOperations are the Linear operations that change workspace state. Only
// they are ever held: holding a read (the snapshot load) would put the signal
// back in the load phase, which is the vacuous case this test exists to avoid.
var writeOperations = map[string]struct{}{
	"issueCreateBatch":      {},
	"issueUpdateBatch":      {},
	"issueUnsubscribeBatch": {},
	"commentCreateBatch":    {},
}

// stubRun is everything a binary run needs: the environment variables that
// point it at the local stubs, the paths the test reads back afterwards, and
// the signal the write-holding stub raises.
type stubRun struct {
	env     []string
	cacheDB string

	// writeInFlight is closed the first time the Linear stub parks a write
	// whose response has not been sent yet, so the test can wait for a write to
	// be genuinely in flight instead of sleeping and hoping. It is nil in the
	// answering mode, in which nothing is ever held.
	writeInFlight <-chan struct{}

	// heldOp reports the operation name of the write that was held (empty if
	// none was), and heldWrite reports whether any write was ever held. Both
	// are safe to read after the run has exited.
	mu     *sync.Mutex
	heldOp *string
	held   *bool
}

// heldOperation reports the operation name of the write the stub held, or ""
// when nothing was held. Like writeWasHeld it is safe to read once the child
// has exited, and the name is recorded before writeInFlight closes, so a reader
// woken by that channel sees it.
func (r stubRun) heldOperation() string {
	if r.mu == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.heldOp == nil {
		return ""
	}
	return *r.heldOp
}

// writeWasHeld reports whether the Linear stub ever held a write. Reading it
// after the child has exited is race-free; reading it while the child runs is
// not, so the assertions wait until runBinary has returned.
func (r stubRun) writeWasHeld() bool {
	if r.mu == nil || r.held == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.held
}

// startStubRun starts the two stubs the sync contacts and returns the
// environment that routes the binary to them.
//
// In the answering mode every Snyk DAST request and every Linear operation is
// answered with a small, valid fixture set. In the hold-write mode the Snyk
// DAST stub answers at once and the Linear stub answers everything except one
// write, which it holds open until the run's request context is cancelled —
// so a signal sent while that write is in flight lands inside the worker pool,
// not in the load phase. A read is never held: the run must get past loading
// for the signal to exercise the interruption guard.
//
// Nothing leaves the machine: the Snyk DAST base URL is configuration, so it
// points straight at a loopback httptest server (Go's proxy rules never send
// loopback through a proxy), and Linear's hard-coded
// https://api.linear.app/graphql is intercepted by the local MITM proxy below,
// trusted for this process tree through SSL_CERT_FILE.
func startStubRun(t *testing.T, tmp string, mode stubMode) (stubRun, error) {
	t.Helper()

	snykdastStub := httptest.NewServer(snykdastStubHandler(mode))
	t.Cleanup(snykdastStub.Close)

	var (
		writeInFlight chan struct{}
		heldOp        string
		mu            sync.Mutex
		held          bool
	)
	if mode == stubModeHoldWrite {
		writeInFlight = make(chan struct{})
	}

	proxy, err := startLinearProxy(t, mode, func(op string) {
		// Record the operation before the channel opens, so a reader woken by
		// the channel never observes an empty name.
		mu.Lock()
		held = true
		if heldOp == "" {
			heldOp = op
		}
		mu.Unlock()
		close(writeInFlight)
	})
	if err != nil {
		return stubRun{}, err
	}

	cacheDB := filepath.Join(tmp, "cache.sqlite3")
	return stubRun{
		cacheDB:       cacheDB,
		writeInFlight: writeInFlight,
		mu:            &mu,
		heldOp:        &heldOp,
		held:          &held,
		env: []string{
			"SNYK_DAST_API_BASE=" + snykdastStub.URL,
			"SNYK_DAST_API_KEY=test-key",
			"SNYK_DAST_APP_BASE=https://dast.example.test",
			"SNYK_DAST_TEAM=",
			"LINEAR_API_KEY=test-key",
			"LINEAR_TEAM_ID=team-a",
			"HTTPS_PROXY=" + proxy.url,
			"HTTP_PROXY=" + proxy.url,
			// The environment is replaced rather than appended to, so the
			// child cannot pick up an ambient proxy, a different CA bundle, or
			// a stray SNYK_DAST_*/LINEAR_* value from the developer's shell.
			// Only PATH (to find nothing external: the binary needs no tools)
			// and HOME stay.
			"CACHE_DB_FILE=" + cacheDB,
			"ERROR_LOG_FILE=" + filepath.Join(tmp, "error.log"),
			"SSL_CERT_FILE=" + proxy.caFile,
		},
	}, nil
}

// runBinary starts the binary with env, calls signal once the process is
// running (which is where the interrupted arm waits for the held write and then
// raises SIGINT), and waits for it to exit. A signal of nil just waits. The
// returned error is the process exit error, so a nil error means exit 0.
func runBinary(t *testing.T, bin string, env stubRun, signal func(*os.Process) error, timeout time.Duration) (string, string, error) {
	t.Helper()

	cmd := exec.Command(bin)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, env.env...)
	// exec's own pipe-and-copy path is used here, not direct capture: it also
	// appends the child's output to os.Stdout and os.Stderr, so a developer or
	// CI reading the failing test sees the run's logs (the interrupted run's
	// "shutdown requested" and "sync failed" included) next to the assertion
	// that quoted them.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}

	if signal != nil {
		signalDone := make(chan struct{})
		go func() {
			defer close(signalDone)
			if err := signal(cmd.Process); err != nil {
				_ = cmd.Process.Kill()
				t.Errorf("signal the binary: %v", err)
			}
		}()
		defer func() {
			// Unblock a signal that is still waiting for a stub signal: the
			// process is gone by now and its HTTP handler has stopped, so the
			// held write's channel would never close.
			select {
			case <-signalDone:
			case <-time.After(5 * time.Second):
				t.Errorf("the signal step did not finish after the binary exited")
			}
		}()
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		return stdout.String(), stderr.String(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("binary did not exit within %s", timeout)
		return "", "", nil
	}
}

// loadCache opens the cache the run left behind and reads its rows.
func loadCache(t *testing.T, path string) cache.Snapshot {
	t.Helper()

	store, err := cache.Open(path)
	if err != nil {
		t.Fatalf("open cache %s: %v", path, err)
	}
	defer store.Close()
	snapshot, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load cache %s: %v", path, err)
	}
	return snapshot
}

// snykdastStubHandler serves the endpoints the sync's Snyk DAST client calls:
// the target listing, the finding listing and the vulnerability definitions,
// each as a one-page envelope holding one record. Every finding reports
// has_sast_correlations=false so the per-finding correlation endpoint is never
// called and the fixture stays small. The handler always answers: the mode's
// hold, when there is one, is in the Linear stub. If it instead parked the load
// requests, the signal would land in the load phase and the test would prove
// nothing about the interruption guard.
func snykdastStubHandler(mode stubMode) http.Handler {
	target := map[string]any{
		"id":   "target-1",
		"type": "web",
		"site": map[string]any{
			"name": "project-a",
			"url":  "https://project-a.example.test/",
			"host": "project-a.example.test",
		},
	}
	finding := map[string]any{
		"id":                    "101",
		"state":                 "notfixed",
		"severity":              30,
		"url":                   "https://project-a.example.test/search",
		"path":                  "/search",
		"method":                "GET",
		"parameter":             "q",
		"created_at":            "2026-01-02T03:04:05Z",
		"last_found":            "2026-01-02T03:04:05Z",
		"has_sast_correlations": false,
		"definition":            map[string]any{"id": "def-1", "name": "Cross-site scripting"},
		"target":                map[string]any{"id": "target-1", "name": "project-a"},
	}
	definition := map[string]any{"id": "def-1", "name": "Cross-site scripting", "passive": false}

	mux := http.NewServeMux()
	for path, results := range map[string]any{
		"/targets/":     []any{target},
		"/findings/":    []any{finding},
		"/definitions/": []any{definition},
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count":      1,
				"page_total": 1,
				"results":    results,
			})
		})
	}
	return mux
}

// linearProxy is a local, minimal HTTPS proxy that intercepts the one host the
// Linear client is hard-coded to talk to. The environment cannot repoint that
// URL, so the only seam without touching production code is the proxy the
// child's HTTP client is told to use.
type linearProxy struct {
	url    string
	caFile string
}

// startLinearProxy serves a proxy on loopback that handles CONNECT for
// api.linear.app:443. In the answering mode it terminates TLS with the fixture
// certificate below and answers every GraphQL operation the sync sends. In the
// hold-write mode it answers every operation except one state-changing write,
// which it holds open until the request context is cancelled; onHold is called
// once with that operation's name, and the held write's response is never sent,
// so the write stays in flight while the test signals the process. Any other
// CONNECT target is refused, so the test cannot be the thing that reaches out
// to the network.
//
// The certificate is one the test generates itself and the child trusts
// through SSL_CERT_FILE, so the twin needs no network and no privileged port.
//
// The proxy is hand-rolled on a net.Listener rather than an httptest.Server:
// after CONNECT the connection stops being HTTP, and hijacking it out of an
// httptest handler races the server's own shutdown and connection accounting.
func startLinearProxy(t *testing.T, mode stubMode, onHold func(operation string)) (linearProxy, error) {
	t.Helper()

	leaf, err := tls.X509KeyPair([]byte(testLeafCertPEM), []byte(testLeafKeyPEM))
	if err != nil {
		return linearProxy{}, err
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte(testLeafCertPEM), 0o600); err != nil {
		return linearProxy{}, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return linearProxy{}, err
	}

	var conns sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conns.Go(func() {
				defer conn.Close()
				serveProxyConn(conn, leaf, mode, onHold)
			})
		}
	}()
	t.Cleanup(func() {
		// Stop accepting and wait for the handlers, so no proxy goroutine
		// outlives the test. Closing the listener unblocks Accept; a held write
		// ends its own handler when the child exits and closes its side.
		_ = listener.Close()
		conns.Wait()
	})

	return linearProxy{url: "http://" + listener.Addr().String(), caFile: caFile}, nil
}

// serveProxyConn reads one CONNECT request off conn and then serves the
// GraphQL operations on it (answering and hold-write modes).
func serveProxyConn(conn net.Conn, leaf tls.Certificate, mode stubMode, onHold func(operation string)) {
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect || req.Host != "api.linear.app:443" {
		_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	tlsConn := tls.Server(&bufferedConn{Conn: conn, reader: reader}, &tls.Config{Certificates: []tls.Certificate{leaf}})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	defer tlsConn.Close()

	// Serve the HTTP requests the client sends over this one connection. A
	// hand-rolled loop rather than net/http's server: the server's listener
	// bookkeeping (Accept/Close ordering) is easy to get wrong for a listener
	// that yields exactly one connection, and the client only ever sends
	// GraphQL POSTs, which http.ReadRequest parses directly.
	//
	// One request at a time is not a limitation here: the held write blocks
	// this loop until the run is cancelled, and the run only needs the write to
	// be in flight. Requests already answered are answered.
	var heldOnce sync.Once
	// heldWrite answers the write WHOSE CONNECTION is being held. The test only
	// requires that one Linear write is parked across the signal; the held
	// write itself is answered by that connection's handler, and every other
	// write is answered at once, so the interruption cannot turn into a pile of
	// failed writes and the pool can drain.
	var heldWrite bool
	var heldWriteBody []byte
	reqReader := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(reqReader)
		if err != nil {
			return
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		_ = req.Body.Close()
		if err != nil {
			return
		}
		operation := linearOperationFromRequestBody(body)

		if heldWrite && operation == heldWriteOperationName(heldWriteBody) {
			// The write this connection has been holding. It is answered here,
			// while the connection's handler has nothing else to do, so no other
			// request waits behind it.
			heldWrite = false
			heldWriteBody = nil
			writeGraphQLResponse(tlsConn, operation)
			continue
		}

		if mode == stubModeHoldWrite && isWriteOperation(operation) {
			if onHold != nil {
				heldOnce.Do(func() { onHold(operation) })
			}
			// Hold the first write until the test has sent its signal. The write
			// is therefore in flight — and provably seen by the stub — exactly
			// when the interruption arrives. Only the first write is held: every
			// later write is answered at once, so the interruption cannot turn
			// into a pile of failed writes whose pool error would stop Run on
			// the line above the interruption guard.
			// Wait for the signal BEFORE answering the held write, so the run
			// cannot complete the write (and then its pool) while the signal is
			// still in flight: a run that refreshed its cache in that window
			// would pass this test with the interruption guard disabled.
			// waitForSIGINT returns once the signal has been delivered, and
			// holdUntilSignalled returns once the arm has opened the gate after
			// sending it, so whichever order the two land in, the write is
			// answered no earlier than the signal.
			waitForSIGINT()
			holdUntilSignalled()
			heldWrite = true
			heldWriteBody = body
			// Keep holding until the connection ends. The signal has been
			// delivered, so Run's pool has stopped starting jobs and the
			// cancellation is set in the run's context; the write is released
			// only when the child gives up on it and closes the connection, so
			// a run with the interruption guard disabled cannot refresh its
			// cache through a write that completed before the cancellation.
			//
			// waitForSIGINT is the part that matters when it works: once it
			// returns, the signal has been delivered, so even a handler that
			// answered here could not let the run outrun the cancellation.
			holdConnection(reader)
			return
		}

		writeGraphQLResponse(tlsConn, operation)
	}
}

// heldWriteOperationName reports the GraphQL operation name of a request body
// this connection is holding, so the handler can recognise the retry the client
// sends on a fresh connection for that one write.
func heldWriteOperationName(body []byte) string {
	return linearOperationFromRequestBody(body)
}

// writeGraphQLResponse writes one GraphQL response on an established
// connection.
func writeGraphQLResponse(tlsConn net.Conn, operation string) {
	response := encodeGraphQLResponse(linearGraphQLData(operation))
	head := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(response)) + "\r\n\r\n"
	if _, err := io.WriteString(tlsConn, head); err != nil {
		return
	}
	_, _ = tlsConn.Write(response)
}

// holdClientReadDeadline bounds how long the parked connection waits for the
// client to give up on the write. It is only a safety bound: the run closes the
// connection (or is killed by the test) well before it, and the deadline ends
// the handler rather than reporting a failure.
const holdClientReadDeadline = 20 * time.Second

// holdConnection parks a connection's handler until the client closes it. It is
// how the held write stays genuinely in flight after the signal: the run is the
// side that ends it. The read is silent rather than a close, because a bare
// read would return as soon as the client's own bytes arrived and closing the
// connection then is a reset, not a hang; the test's own teardown makes the
// handler return if the client has not.
func holdConnection(reader io.Reader) {
	// The deadline is deliberate and it is why the write ends in a timeout
	// rather than in a cancellation: when the client's request context is
	// cancelled, closing the connection is invisible to the client's read, and
	// the pool error that follows would stop Run on the line above the
	// interruption guard. A read deadline makes the client's read fail on its
	// own, the identical error a Linear host that stopped answering produces, so
	// the run's pool drains on a plain value failure — which is the only way
	// Run reaches the guard.
	if c, ok := reader.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = c.SetReadDeadline(time.Now().Add(holdClientReadDeadline))
	}
	_, _ = io.Copy(io.Discard, reader)
}

// signalGate is closed by the write-holding test arm the moment it has sent
// SIGINT. The held write then stops being held, so the write the stub has
// already seen completes while the run is being interrupted.
var signalGate = make(chan struct{})

// releaseHeldWrites opens the gate a signalled run is waiting on. The test arm
// calls it immediately after sending the signal.
func releaseHeldWrites() {
	select {
	case <-signalGate:
	default:
		close(signalGate)
	}
}

// sigintGate is closed by the test arm once the signal it sent is known to have
// been delivered to the child. The Linear stub releases no held write before
// then, so the run cannot answer the parked call, finish its pool and refresh
// its cache before the cancellation lands: that race is what lets a run with
// the interruption guard disabled pass this test.
//
// The wait rides on a real SIGINT delivered to the stub itself — a process
// signal, not the HTTP connection — so it observes the signal no later than the
// run does, and the release is skipped entirely when the platform does not
// report the interruption.
var sigintGate struct {
	ready sync.Once
	once  sync.Once
	ch    chan struct{}
	proc  *os.Process
}

// initSIGINTGate fills the gate's fields exactly once, before any goroutine can
// read them. The fields are written by whichever side runs first — a stub
// parking on waitForSIGINT or the test arm releasing it — so without this the
// pointer handoff between those goroutines is a data race (the -race build
// reported a read of proc in releaseSIGINTSignal racing the write in
// waitForSIGINT). It must be called by both sides before they touch a field.
func initSIGINTGate() {
	sigintGate.ready.Do(func() {
		sigintGate.ch = make(chan struct{})
		sigintGate.proc, _ = os.FindProcess(os.Getpid())
	})
}

// closeSIGINTGate closes the gate exactly once, whichever side gets there
// first: the stub's own SIGINT delivery or the test arm's release.
func closeSIGINTGate() {
	sigintGate.once.Do(func() {
		if sigintGate.ch == nil {
			sigintGate.ch = make(chan struct{})
		}
		close(sigintGate.ch)
	})
}

// waitForSIGINT parks the stub on sigintGate, so a signalled run cannot get
// past the held write before the signal has been delivered. It gives up after
// sigintGateTimeout, so a platform that does not report the interruption makes
// the test slow rather than hanging it.
//
// The gate is opened by a real SIGINT delivered to the stub itself — a process
// signal, not the HTTP connection — so it observes the signal no later than the
// run does. signal.Notify keeps the signal from killing the stub.
func waitForSIGINT() {
	initSIGINTGate()
	raw := make(chan os.Signal, 1)
	sigintGate.once.Do(func() {
		signal.Notify(raw, syscall.SIGINT)
		go func() {
			<-raw
			signal.Stop(raw)
			closeSIGINTGate()
		}()
	})
	if sigintGate.ch == nil {
		// The gate was already opened by the arm's release, which means the
		// signal has been sent: there is nothing left to wait for.
		return
	}
	select {
	case <-sigintGate.ch:
	case <-time.After(sigintGateTimeout):
	}
}

// releaseSIGINTSignal is called by the test arm only after the child has been
// signalled. It sends SIGINT to this process so the stubs parked in
// waitForSIGINT wake on their own delivery, and opens the gate directly as a
// fallback so the arm cannot stall if that delivery is not observable.
func releaseSIGINTSignal() {
	initSIGINTGate()
	if sigintGate.proc != nil {
		_ = sigintGate.proc.Signal(syscall.SIGINT)
	}
	closeSIGINTGate()
}

// sigintGateTimeout bounds how long a stub waits for the signal's delivery
// before answering the write anyway. It exists so a platform whose signal
// delivery the stub cannot observe makes the guard experiment flaky rather than
// hanging the test; on Linux the channel is closed in time.
const sigintGateTimeout = 5 * time.Second

// holdUntilSignalled blocks a held write until the test has signalled the run,
// with a bound so a test that never signals cannot hang the stub forever.
// Unlike the signal itself, this is visible to the run as a normal (delayed)
// response: it ends the hold on a successful note, so the pool can drain
// without a job error and the guard — not the pool error — is what stops the
// cache refresh.
func holdUntilSignalled() {
	select {
	case <-signalGate:
	case <-time.After(30 * time.Second):
	}
}

// bufferedConn keeps bytes the CONNECT reader already buffered available to the
// TLS server, which would otherwise read past them from the socket.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// linearOperationFromRequestBody returns the GraphQL operation name of a POSTed
// request body, or "" when the body is not a GraphQL document.
func linearOperationFromRequestBody(body []byte) string {
	var req struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	return linearOperationName(req.Query)
}

// encodeGraphQLResponse renders one operation's data as the JSON body the
// client expects, padded so a bare object is still a valid top-level document.
func encodeGraphQLResponse(data map[string]any) []byte {
	response, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return []byte(`{"data":{}}`)
	}
	return response
}

// isWriteOperation reports whether a Linear operation changes workspace state.
func isWriteOperation(operation string) bool {
	_, ok := writeOperations[operation]
	return ok
}

// fixtureExistingIssue is one managed ticket the sync already owns, for the
// same finding the Snyk DAST fixture reports. It gives the completed run a
// Linear cache row to write and the interrupted run a cache row to lose: with
// an empty workspace the Linear map would be empty either way, and the rows
// assertion would only ever exercise the Snyk DAST table.
//
// It is also what makes the interrupted arm meaningful: the existing ticket
// needs an update, so the run has exactly one planned update (and no create),
// and issueUpdateBatch — the write the hold mode keeps in flight — is a write
// the run actually sends.
//
// The metadata block and the title prefix are what the client keys identity on;
// the description carries the same block a real run writes.
func fixtureExistingIssue() map[string]any {
	description := "Snyk DAST: Cross-site scripting in project-a\n\n" +
		"<!-- snyk-dast-linear-sync\n" +
		"fingerprint: snyk-dast:target-1:101\n" +
		"managed_labels: snyk-dast-automation\n" +
		"-->"
	return map[string]any{
		"id":          "issue-1",
		"identifier":  "PRJ-100",
		"title":       "Snyk DAST: Cross-site scripting in project-a",
		"description": description,
		"url":         "https://linear.example.test/PRJ-100",
		"priority":    2,
		"dueDate":     nil,
		"archivedAt":  nil,
		"state":       map[string]any{"id": "state-todo", "name": "Todo"},
		"labels": map[string]any{"nodes": []any{
			map[string]any{"id": "label-1", "name": "snyk-dast-automation"},
		}},
	}
}

// linearGraphQLData is the response body for one operation, given its name.
func linearGraphQLData(operation string) map[string]any {
	switch operation {
	case "resolveTeam":
		return map[string]any{"teams": map[string]any{"nodes": []any{
			map[string]any{"id": "team-1", "key": "team-a", "name": "Team A"},
		}}}
	case "teamStates":
		return map[string]any{"team": map[string]any{"states": map[string]any{
			"nodes": []any{
				map[string]any{"id": "state-backlog", "name": "Backlog", "type": "backlog"},
				map[string]any{"id": "state-todo", "name": "Todo", "type": "unstarted"},
				map[string]any{"id": "state-done", "name": "Done", "type": "completed"},
				map[string]any{"id": "state-cancelled", "name": "Canceled", "type": "canceled"},
			},
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
		}}}
	case "managedIssueLabels":
		// The run refuses to create a managed issue unless the managed label
		// exists in the workspace, so the fixture label has to be there.
		return map[string]any{"issueLabels": map[string]any{
			"nodes": []any{
				map[string]any{
					"id":   "label-1",
					"name": "snyk-dast-automation",
					"team": map[string]any{"id": "team-1"},
				},
			},
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
		}}
	case "existingIssues", "issuesByFingerprint":
		return map[string]any{"issues": map[string]any{
			"nodes":    []any{fixtureExistingIssue()},
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
		}}
	case "issueCreateBatch":
		return map[string]any{"issueCreate0": map[string]any{
			"success": true,
			"issue":   map[string]any{"id": "issue-1", "identifier": "PRJ-100"},
		}}
	case "issueUpdateBatch":
		return map[string]any{"issueUpdate0": map[string]any{"success": true}}
	case "issueUnsubscribeBatch":
		return map[string]any{"issueUnsubscribe0": map[string]any{"success": true}}
	case "commentCreateBatch":
		return map[string]any{"commentCreate0": map[string]any{
			"success": true,
			"comment": map[string]any{"id": "comment-1"},
		}}
	default:
		return map[string]any{}
	}
}

// linearOperationName extracts the GraphQL operation name from a query or
// mutation document. The client builds every document as
// "query name(...)" or "mutation name(...)" on its first line.
func linearOperationName(query string) string {
	fields := strings.Fields(strings.TrimSpace(query))
	if len(fields) < 2 {
		return ""
	}
	name := fields[1]
	if idx := strings.IndexAny(name, "( {\r\n\t"); idx >= 0 {
		name = name[:idx]
	}
	return name
}
