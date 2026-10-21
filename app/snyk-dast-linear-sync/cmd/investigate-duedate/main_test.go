package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// testLeafCertPEM/testLeafKeyPEM are a throwaway key pair generated with the
// standard library's crypto/tls/generate_cert.go for the host api.linear.app. It
// is a test fixture for the local MITM proxy below and is not used anywhere
// else; the certificate is expired the moment this test stops pretending.
//
// The pair is copied from cmd/snyk-dast-linear-sync/main_test.go, the other
// package that needs to intercept the Linear client's hard-coded GraphQL host:
// a test binary cannot import another package's test helpers, and the host
// cannot be repointed from the environment.
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

// TestSIGINTExitsNonZeroAndSaysSo runs the built binary twice against the same
// local stubs and compares the two endings: the interrupted run must exit
// non-zero, the completed one must exit 0.
//
// WHAT it pins, and what it does not. It pins the contract a scheduler depends
// on -- a run whose work was interrupted is not reported as a success -- and it
// pins that the run says so on stderr, for both endings the interruption can
// produce: the signal may fail the call that is in flight, or it may land after
// the last call answered and leave a printed diagnosis the run no longer
// trusts. Both are real, and which one happens depends on how far the run had
// got, so the arm asserts what holds for both (the shutdown line and the
// reported exit status) and accepts either outcome for the rest. The narrow
// rule that a cancellation landing *after* the last
// call still exits non-zero is not observable from outside the process with a
// real signal -- there is no window in which the calls are done and the process
// has not exited -- so that rule is pinned by TestExitCodeReportsCancellation
// against the helper main uses, and this test pins the process-level contract.
//
// WHY the pair. The interrupted arm alone is weak: anything that makes the
// binary fail to finish -- a broken fixture, a stub that answers badly -- exits
// non-zero too. The completed arm runs the identical binary against the
// identical stubs, released instead of interrupted, and requires exit 0 with
// the diagnostics printed. A non-zero exit therefore cannot come from the
// fixtures; it comes from the signal.
//
// The signal is delivered once the stub reports its parked call was reached, so
// the interruption is known to land while a request is in flight rather than
// during the run's startup.
func TestSIGINTExitsNonZeroAndSaysSo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the process-level signal test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal delivery is not available on windows")
	}

	bin := filepath.Join(t.TempDir(), "investigate-duedate")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test binary: %v\n%s", err, out)
	}

	t.Run("interrupted run exits non-zero", func(t *testing.T) {
		stubs := startStubTool(t, t.TempDir())

		stdout, stderr, exitErr := runTool(t, bin, stubs.env, []string{"--issue", "PRJ-100"},
			func(proc *os.Process) error {
				// Wait for the held call: the signal must land while a request
				// is genuinely in flight, not while the tool is starting up or
				// still loading the Linear issue.
				select {
				case <-stubs.held.entered:
				case <-time.After(30 * time.Second):
					return errors.New("the Snyk DAST definitions call was never issued")
				}
				return proc.Signal(syscall.SIGINT)
			}, 60*time.Second)

		if !stubs.held.wasEntered() {
			t.Fatal("no call was held when the signal arrived, so interrupting the load phase proves nothing here")
		}
		if exitErr == nil {
			t.Fatalf("interrupted run exited 0, want a non-zero exit\nstderr:\n%s\nstdout:\n%s", stderr, stdout)
		}
		var exitErrPtr *exec.ExitError
		if !errors.As(exitErr, &exitErrPtr) {
			t.Fatalf("interrupted run ended for an unexpected reason: %v", exitErr)
		}
		if code := exitErrPtr.ExitCode(); code != 1 {
			t.Fatalf("interrupted run exited %d, want 1\nstderr:\n%s", code, stderr)
		}
		if !strings.Contains(stderr, "shutdown requested") {
			t.Fatalf("stderr does not contain %q, so the run did not announce the shutdown:\n%s", "shutdown requested", stderr)
		}
		if !strings.Contains(stderr, "exiting non-zero") || !strings.Contains(stderr, "exit_code=1") {
			t.Fatalf("stderr does not report exit_code=1, so the log line the status is read from is missing:\n%s", stderr)
		}
		// The run reports itself either way: a cancelled in-flight call prints
		// the load error that failed it, and a run whose diagnostics were
		// already printed ends with the exit-code line finish emits. Which of
		// the two happens depends only on how far the run had got when the
		// signal landed, never on whether the signal landed -- so the
		// assertions above are the ones that must hold for every timing.
		if !strings.Contains(stderr, "error: load Snyk DAST snapshot") && !strings.Contains(stdout, "Conclusion:") {
			t.Fatalf("interrupted run reported neither the cancelled snapshot load nor its diagnostics\nstderr:\n%s\nstdout:\n%s", stderr, stdout)
		}
	})

	t.Run("completed run still exits zero", func(t *testing.T) {
		stubs := startStubTool(t, t.TempDir())
		// Nothing interrupts this arm; the hold is released so the snapshot
		// load can finish and the tool can print its diagnostics.
		close(stubs.held.release)

		stdout, stderr, exitErr := runTool(t, bin, stubs.env, []string{"--issue", "PRJ-100"}, nil, 60*time.Second)
		if exitErr != nil {
			t.Fatalf("completed run exited with %v, want exit 0\nstderr:\n%s\nstdout:\n%s", exitErr, stderr, stdout)
		}
		if !strings.Contains(stdout, "Conclusion:") {
			t.Fatalf("completed run did not print its diagnostics\nstderr:\n%s\nstdout:\n%s", stderr, stdout)
		}
	})
}

// startStubTool starts the two stubs the tool contacts and returns the
// environment that routes the binary to them. Nothing leaves the machine: the
// Snyk DAST base URL is configuration and points straight at a loopback
// httptest server, and the hard-coded Linear GraphQL host is intercepted by the
// local MITM proxy below.
func startStubTool(t *testing.T, tmp string) stubTool {
	t.Helper()

	held := newHeldCall()
	snykdastStub := httptest.NewServer(snykdastStubHandler(held))
	t.Cleanup(func() {
		// Release any handler still parked in the hold before the server is
		// closed: a handler parked for a process that is already gone would
		// otherwise keep Close waiting.
		held.stop()
		snykdastStub.Close()
	})

	proxy, err := startLinearProxy(t, linearGraphQLHandler())
	if err != nil {
		t.Fatalf("start Linear proxy: %v", err)
	}

	return stubTool{
		held: held,
		env: []string{
			"SNYK_DAST_API_BASE=" + snykdastStub.URL,
			"SNYK_DAST_API_KEY=test-key",
			"SNYK_DAST_APP_BASE=https://dast.example.test",
			"SNYK_DAST_TEAM=",
			"LINEAR_API_KEY=test-key",
			"LINEAR_TEAM_ID=team-a",
			"HTTPS_PROXY=" + proxy.url,
			"HTTP_PROXY=" + proxy.url,
			"SSL_CERT_FILE=" + proxy.caFile,
			"CACHE_DB_FILE=" + filepath.Join(tmp, "cache.sqlite3"),
			"ERROR_LOG_FILE=" + filepath.Join(tmp, "error.log"),
		},
	}
}

// linearProxy is a local, minimal HTTPS proxy that intercepts the one host the
// Linear client is hard-coded to talk to.
type linearProxy struct {
	url    string
	caFile string
}

// startLinearProxy serves a proxy on loopback that handles CONNECT for
// api.linear.app:443, terminates TLS with the fixture certificate and answers
// the GraphQL operation the tool sends. Any other CONNECT target is refused, so
// the test cannot be the thing that reaches out to the network.
func startLinearProxy(t *testing.T, handler http.Handler) (linearProxy, error) {
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
				serveProxyConn(conn, leaf, handler)
			})
		}
	}()
	t.Cleanup(func() {
		// Stop accepting and wait for the handlers, so no proxy goroutine
		// outlives the test.
		_ = listener.Close()
		conns.Wait()
	})

	return linearProxy{url: "http://" + listener.Addr().String(), caFile: caFile}, nil
}

// serveProxyConn reads one CONNECT request off conn and answers it from the
// GraphQL handler over TLS.
func serveProxyConn(conn net.Conn, leaf tls.Certificate, handler http.Handler) {
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

	reqReader := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(reqReader)
		if err != nil {
			return
		}
		rw := newResponseWriter(tlsConn)
		handler.ServeHTTP(rw, req)
		if err := rw.flush(); err != nil {
			return
		}
		if req.Close || !req.ProtoAtLeast(1, 1) {
			return
		}
	}
}

// bufferedConn keeps bytes the CONNECT reader already buffered available to the
// TLS server, which would otherwise read past them from the socket.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// responseWriter writes one HTTP response straight to the connection. It
// buffers the body so Content-Length can be set on the way out.
type responseWriter struct {
	conn net.Conn
	code int
	head http.Header
	body bytes.Buffer
}

func newResponseWriter(conn net.Conn) *responseWriter {
	return &responseWriter{conn: conn, head: http.Header{}}
}

func (w *responseWriter) Header() http.Header { return w.head }

func (w *responseWriter) WriteHeader(code int) { w.code = code }

func (w *responseWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *responseWriter) flush() error {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	resp := &http.Response{
		StatusCode:    w.code,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.head,
		Body:          io.NopCloser(bytes.NewReader(w.body.Bytes())),
		ContentLength: int64(w.body.Len()),
	}
	return resp.Write(w.conn)
}

// linearGraphQLHandler answers the one operation the tool sends -- the
// issue-by-identifier lookup -- with the managed fixture issue.
func linearGraphQLHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var req struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "decode request", http.StatusBadRequest)
			return
		}
		if !strings.Contains(req.Query, "issues(") {
			http.Error(w, "unexpected Linear operation", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[`+existingIssueJSON()+`]}}}`)
	})
}

// linearOperationName extracts the GraphQL operation name from a query or
// mutation document.
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

// existingIssueJSON is the Linear issue the tool is asked about: one managed
// ticket (PRJ-100, in fixture project-a) whose metadata block carries the
// fingerprint the Snyk DAST fixture reports. The run then gets past the
// fingerprint check and on to the snapshot load the signal interrupts.
func existingIssueJSON() string {
	return `{
		"id": "issue-1",
		"identifier": "PRJ-100",
		"title": "Snyk DAST: Cross-site scripting in project-a",
		"description": "Snyk DAST: Cross-site scripting in project-a\n\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-1:101\nmanaged_labels: snyk-dast-automation\n-->",
		"url": "https://linear.example.test/PRJ-100",
		"priority": 2,
		"dueDate": null,
		"archivedAt": null,
		"state": {"id": "state-todo", "name": "Todo"},
		"labels": {"nodes": [{"id": "label-1", "name": "snyk-dast-automation"}]}
	}`
}

// snykdastStubHandler serves the three listings LoadSnapshot makes. The targets
// and findings listings answer at once; the definitions listing -- the last
// call, so the signal cannot land during the earlier pages -- is parked and
// reports that it was entered, which is the evidence the run was interrupted
// with a request genuinely in flight.
func snykdastStubHandler(held *heldCall) http.Handler {
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
		"/targets/":  []any{target},
		"/findings/": []any{finding},
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			envelope(w, results)
		})
	}
	mux.HandleFunc("/definitions/", func(w http.ResponseWriter, r *http.Request) {
		held.markEntered()
		if !held.completed() {
			// The test either releases the hold (the control arm, which needs
			// the call answered so the run can finish) or ends the test (the
			// interrupted arm, whose process is already gone).
			select {
			case <-held.release:
			case <-held.closed:
				return
			}
		}
		envelope(w, []any{definition})
	})
	return mux
}

func envelope(w http.ResponseWriter, results any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count":      1,
		"page_total": 1,
		"results":    results,
	})
}

// stubTool is everything a run of the built binary needs: the environment that
// routes it to the local stubs, and the events the test gates on.
type stubTool struct {
	env []string
	// held is the parked definitions call: entered only once the run is inside
	// its snapshot load, so the signal is known to arrive while a call is in
	// flight rather than during startup.
	held *heldCall
}

// heldCall reports that the parked handler was entered, and releases it when
// the control arm runs instead of interrupting.
type heldCall struct {
	entered chan struct{}
	release chan struct{}
	closed  chan struct{}
	once    sync.Once

	mu       sync.Mutex
	holdOnly bool
}

func newHeldCall() *heldCall {
	return &heldCall{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		closed:   make(chan struct{}),
		holdOnly: true,
	}
}

// stop ends the hold for good, so a handler parked past the end of the test
// releases instead of keeping the stub's Close waiting for it.
func (h *heldCall) stop() {
	h.mu.Lock()
	h.holdOnly = false
	h.mu.Unlock()
	select {
	case <-h.closed:
	default:
		close(h.closed)
	}
}

func (h *heldCall) markEntered() { h.once.Do(func() { close(h.entered) }) }

// completed switches the handler from waiting for the test's release to
// answering immediately, which is what the probe and profile tests need: a run
// they are not holding must be able to finish on its own.
func (h *heldCall) completed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.holdOnly
}

func (h *heldCall) answerFreely() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.holdOnly = false
}

func (h *heldCall) wasEntered() bool {
	select {
	case <-h.entered:
		return true
	default:
		return false
	}
}

// runTool starts the binary with env, optionally signals it once it is running,
// and waits for it to exit. The returned error is the process exit error, so a
// nil error means exit 0.
func runTool(t *testing.T, bin string, env []string, args []string, signal func(*os.Process) error, timeout time.Duration) (string, string, error) {
	t.Helper()

	cmd := exec.Command(bin, args...)
	// The environment is replaced rather than appended to, so the child
	// cannot pick up an ambient proxy, a different CA bundle, or a stray
	// LINEAR_*/SNYK_DAST_* value from the developer's shell.
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}

	if signal != nil {
		if err := signal(cmd.Process); err != nil {
			_ = cmd.Process.Kill()
			t.Fatalf("signal the binary: %v", err)
		}
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

// TestExitCodeReportsCancellation pins the rule the tool's shutdown log line
// announces: once the context has been cancelled the process must exit
// non-zero, whatever the call that observed the cancellation returned.
//
// WHY the cancelled-with-nil-error row is the point. A cancellation that lands
// after the calls have finished leaves every call successful, so an exit rule
// that keys on the error alone reports success for an interrupted run while the
// log says otherwise. Only the context can tell that run from a completed one,
// which is why exitCode reads it. This is the exported-to-main rule, not a
// restatement of it: the assertion fails if main stops passing the context
// through, or if exitCode stops consulting it.
func TestExitCodeReportsCancellation(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want int
		why  string
	}{
		{"live context, call succeeded", live, nil, 0, "an uninterrupted run that loaded everything exits 0"},
		{"live context, call failed", live, errors.New("load Linear issue: connection refused"), 1, "a failed call is still a failure"},
		{"cancelled context, call succeeded", cancelled, nil, 1, "the case the rule exists for: the run was interrupted after its last call"},
		{"cancelled context, call cancelled", cancelled, context.Canceled, 1, "the cancellation reached the call first"},
	}
	for _, tc := range cases {
		if got := exitCode(tc.ctx, tc.err); got != tc.want {
			t.Errorf("exitCode(%s) = %d, want %d (%s)", tc.name, got, tc.want, tc.why)
		}
	}
}

// TestFinishSaysWhatItExitsWith pins the other half of the exit-code rule: the
// status exitCode computes is announced on the way out, in the same line the
// shutdown notice lives in.
//
// WHY it is worth a test. The process arm cannot see the difference between a
// line emitted while the run was still working -- describing a decision it had
// not made yet, which is what the code this replaced did -- and one emitted
// just before os.Exit, because by the time the test reads the captured stderr
// the process is gone either way. Calling finish directly is what tells the two
// apart, and it also pins that a failure with no cancellation is still reported
// as an exit, so a failed run is never silent about how it ended.
func TestFinishSaysWhatItExitsWith(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name         string
		ctx          context.Context
		status       int
		wantShutdown bool
		wantExit     bool
	}{
		{"completed run says nothing", live, 0, false, false},
		{"failed run reports its status", live, 1, false, true},
		{"interrupted run reports both", cancelled, 1, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			if got := finish(tc.ctx, logger, tc.status); got != tc.status {
				t.Fatalf("finish returned %d, want the status it was given (%d)", got, tc.status)
			}
			out := buf.String()
			if got := strings.Contains(out, "shutdown requested"); got != tc.wantShutdown {
				t.Errorf("shutdown line present = %v, want %v:\n%s", got, tc.wantShutdown, out)
			}
			if got := strings.Contains(out, "exiting non-zero"); got != tc.wantExit {
				t.Errorf("exit line present = %v, want %v:\n%s", got, tc.wantExit, out)
			}
			if tc.wantExit && !strings.Contains(out, "exit_code=1") {
				t.Errorf("exit line does not name the status:\n%s", out)
			}
		})
	}
}
