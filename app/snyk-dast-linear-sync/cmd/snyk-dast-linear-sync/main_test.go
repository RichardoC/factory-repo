package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
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
// arm removes that possibility: the identical binary, with the identical Snyk
// DAST stub serving instead of hanging, exits 0, logs the cache refresh and
// leaves a row in both tables. So the SIGINT arm's zero rows cannot come from a
// binary that never manages to write a cache at all; they come from the
// interrupt path deliberately not refreshing it. Evidence that a run *does*
// write its cache is what gives "it did not write one" its meaning.
//
// The two arms share the built binary and nothing else: separate cache files,
// separate stubs (one hangs, one answers) and separate processes.
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
		env, err := startStubRun(t, tmp, false)
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

	// The interrupted run. Both stubs hang, so the signal lands while the run is
	// still loading its inputs and before it can refresh the cache.
	t.Run("interrupted run leaves the cache untouched", func(t *testing.T) {
		tmp := t.TempDir()
		env, err := startStubRun(t, tmp, true)
		if err != nil {
			t.Fatalf("build hanging stubs: %v", err)
		}

		stdout, stderr, exitErr := runBinary(t, bin, env, func(proc *os.Process) error {
			time.Sleep(time.Second)
			return proc.Signal(syscall.SIGINT)
		}, 30*time.Second)
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

		snapshot := loadCache(t, env.cacheDB)
		if len(snapshot.SnykDASTHashes) != 0 || len(snapshot.LinearHashes) != 0 {
			t.Fatalf("cache has rows after an interrupted run: %d snyk dast, %d linear, want 0 and 0",
				len(snapshot.SnykDASTHashes), len(snapshot.LinearHashes))
		}
	})
}

// stubRun is everything a binary run needs: the environment variables that
// point it at the local stubs, plus the paths the test reads back afterwards.
type stubRun struct {
	env     []string
	cacheDB string
}

// startStubRun starts the two stubs the sync contacts and returns the environment
// that routes the binary to them. When hang is true both stubs block until
// their context is cancelled, which is how the interrupted arm keeps the run
// mid-load; when it is false the Snyk DAST stub answers a small, valid fixture
// set and the Linear stub answers every GraphQL operation the run sends.
//
// Nothing leaves the machine either way. The Snyk DAST base URL is
// configuration, so it points straight at a loopback httptest server, and Go's
// proxy rules never send loopback through a proxy. Linear's GraphQL endpoint is
// hard-coded to https://api.linear.app/graphql, so that host is intercepted by
// the local MITM proxy below, trusted for this process tree through
// SSL_CERT_FILE.
func startStubRun(t *testing.T, tmp string, hang bool) (stubRun, error) {
	t.Helper()

	snykdastStub := httptest.NewServer(snykdastStubHandler(hang))
	t.Cleanup(snykdastStub.Close)

	proxy, err := startLinearProxy(t, hang)
	if err != nil {
		return stubRun{}, err
	}

	cacheDB := filepath.Join(tmp, "cache.sqlite3")
	return stubRun{
		cacheDB: cacheDB,
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

// runBinary starts the binary with env, optionally signals it once it is
// running, and waits for it to exit. A signal of nil just waits. The returned
// error is the process exit error, so a nil error means exit 0.
func runBinary(t *testing.T, bin string, env stubRun, signal func(*os.Process) error, timeout time.Duration) (string, string, error) {
	t.Helper()

	cmd := exec.Command(bin)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, env.env...)
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
// called and the fixture stays small. When hang is true the request is parked
// until the run is cancelled instead of answered.
func snykdastStubHandler(hang bool) http.Handler {
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
			if hang {
				<-r.Context().Done()
				return
			}
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
// api.linear.app:443. In answering mode it terminates TLS with the fixture
// certificate below and answers the GraphQL operations the sync sends; in
// hanging mode it establishes nothing and parks the connection until the run is
// cancelled. Any other CONNECT target is refused, so the test cannot be the
// thing that reaches out to the network.
//
// The certificate is one the test generates itself and the child trusts
// through SSL_CERT_FILE, so the twin needs no network and no privileged port.
//
// The proxy is hand-rolled on a net.Listener rather than an httptest.Server:
// after CONNECT the connection stops being HTTP, and hijacking it out of an
// httptest handler races the server's own shutdown and connection accounting.
func startLinearProxy(t *testing.T, hang bool) (linearProxy, error) {
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
				serveProxyConn(conn, leaf, hang)
			})
		}
	}()
	t.Cleanup(func() {
		// Stop accepting and wait for the handlers, so no proxy goroutine
		// outlives the test. Closing the listener unblocks Accept; the
		// per-connection goroutines end when the child closes its side.
		_ = listener.Close()
		conns.Wait()
	})

	return linearProxy{url: "http://" + listener.Addr().String(), caFile: caFile}, nil
}

// serveProxyConn reads one CONNECT request off conn and either answers it from
// the GraphQL handler (answering mode) or parks it (hanging mode).
func serveProxyConn(conn net.Conn, leaf tls.Certificate, hang bool) {
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
	if hang {
		// Nothing is established: the client waits for a TLS handshake that
		// never starts, exactly like a network that stopped answering. The
		// connection is held open by draining whatever the client sends — a
		// bare read would return as soon as the client's own bytes arrived
		// and close the connection, which is a connection reset, not a hang.
		_, _ = io.Copy(io.Discard, reader)
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
	handler := linearGraphQLHandler()
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

// flush writes the buffered response to the connection.
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

// linearGraphQLHandler answers the Linear operations the sync sends, keyed on
// the operation name in the request's query text. The fixture is deliberately
// empty where it can be: no existing issues, no managed labels. The run then
// has exactly one planned create, which both warms the cache and keeps the
// fixture small. When hang is true the response is parked instead.
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

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": linearGraphQLData(req.Query)})
	})
}

// fixtureExistingIssue is one managed ticket the sync already owns, for the
// same finding the Snyk DAST fixture reports. It gives the completed run a
// Linear cache row to write and the interrupted run a cache row to lose: with
// an empty workspace the Linear map would be empty either way, and the rows
// assertion would only ever exercise the Snyk DAST table.
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

// linearGraphQLData is the response body for one operation. The operation name
// is the first word after "query "/"mutation " in the query text.
func linearGraphQLData(query string) map[string]any {
	switch linearOperationName(query) {
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
