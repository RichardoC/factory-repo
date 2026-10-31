package snykdast

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
)

func TestMapStatus(t *testing.T) {
	future := "2126-06-01"
	past := "2000-01-01"
	malformed := "soon-ish"
	blank := "   "

	cases := []struct {
		name           string
		state          string
		expirationDate *string
		want           model.FindingStatus
		wantExpires    bool
	}{
		{"notfixed", "notfixed", nil, model.FindingOpen, false},
		{"retesting is open", "retesting", nil, model.FindingOpen, false},
		{"fixed", "fixed", nil, model.FindingFixed, false},
		{"invalid is ignored", "invalid", nil, model.FindingIgnored, false},
		{"accepted without expiry is ignored", "accepted", nil, model.FindingIgnored, false},
		{"accepted with unparseable expiry is ignored", "accepted", &malformed, model.FindingIgnored, false},
		{"accepted with whitespace-only expiry is ignored", "accepted", &blank, model.FindingIgnored, false},
		{"accepted with unparseable expiry ignores case", "Accepted", &malformed, model.FindingIgnored, false},
		{"accepted with lapsed expiry reopens", "accepted", &past, model.FindingOpen, true},
		{"accepted with future expiry is snoozed", "accepted", &future, model.FindingSnoozed, true},
		{"unknown state defaults to open", "unknown", nil, model.FindingOpen, false},
		{"empty state defaults to open", "", nil, model.FindingOpen, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, expires := mapStatus(tc.state, tc.expirationDate)
			if got != tc.want {
				t.Fatalf("mapStatus() = %q, want %q", got, tc.want)
			}
			if tc.wantExpires && expires.IsZero() {
				t.Fatalf("mapStatus() expires = zero, want non-zero")
			}
			if !tc.wantExpires && !expires.IsZero() {
				t.Fatalf("mapStatus() expires = %v, want zero", expires)
			}
		})
	}
}

func TestMapStatusSnoozedParsesExpiry(t *testing.T) {
	expiry := "2126-06-01"
	_, expires := mapStatus("accepted", &expiry)
	want := time.Date(2126, time.June, 1, 0, 0, 0, 0, time.UTC)
	if !expires.Equal(want) {
		t.Fatalf("expires = %v, want %v", expires, want)
	}
}

func TestMapSeverity(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{40, "critical"},
		{30, "high"},
		{20, "medium"},
		{10, "low"},
		{0, "unknown"},
		{99, "unknown"},
	}
	for _, tc := range cases {
		if got := mapSeverity(tc.code); got != tc.want {
			t.Errorf("mapSeverity(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestEnrichCorrelationsFetchesAndAttachesMarkdown(t *testing.T) {
	// Stand up a fake Snyk DAST API that serves findings with snyk_sast=true
	// and correlation markdown for one finding.
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	base, _ := url.Parse(server.URL + "/")
	c := &Client{
		apiBase:     base,
		httpClient:  server.Client(),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		concurrency: 2,
	}

	// correlation endpoint for finding 1 (numeric id "1") on target-a
	mux.HandleFunc("/targets/target-a/findings/1/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(paginatedCorrelations{
			Count: 1, PageTotal: 1, Page: 1, Length: 100,
			Results: []correlationNode{
				{ID: "corr-1", Confirmed: true, Markdown: "**Snyk Code:** [SQLi in `db.go:42`](https://app.snyk.io/...)"},
			},
		})
	})

	findings := []model.Finding{
		{Fingerprint: "snyk-dast:target-a:1", SnykDASTFindingID: "1", TargetID: "target-a"},
		{Fingerprint: "snyk-dast:target-b:2", SnykDASTFindingID: "2", TargetID: "target-b"},
	}
	correlated := []bool{true, false}
	if err := c.enrichCorrelations(context.Background(), findings, correlated); err != nil {
		t.Fatalf("enrichCorrelations() error = %v", err)
	}
	if len(findings[0].CorrelationMarkdown) != 1 || !strings.Contains(findings[0].CorrelationMarkdown[0], "db.go:42") {
		t.Fatalf("finding 0 correlation markdown = %#v", findings[0].CorrelationMarkdown)
	}
	if len(findings[1].CorrelationMarkdown) != 0 {
		t.Fatalf("finding 1 should have no correlation markdown, got %#v", findings[1].CorrelationMarkdown)
	}
}

func TestNumericFindingID(t *testing.T) {
	cases := []struct {
		composite string
		want      string
	}{
		{"su2d3k-123", "123"},
		{"abc-456", "456"},
		{"123", "123"},
		{"", ""},
		{"abc-", ""},
	}
	for _, tc := range cases {
		if got := numericFindingID(tc.composite); got != tc.want {
			t.Errorf("numericFindingID(%q) = %q, want %q", tc.composite, got, tc.want)
		}
	}
}

// TestMapStatusDateOnlyExpiryCoversItsFinalDay pins the acceptance boundary. A
// Snyk DAST expiration_date of "2026-08-19" means the acceptance covers all of
// the 19th, so it must not lapse until the end of that day. Comparing against
// midnight expired it a day early, dragging the finding back into triage while
// the team still considered it accepted.
func TestMapStatusDateOnlyExpiryCoversItsFinalDay(t *testing.T) {
	nowUTC := time.Now().UTC()
	today := nowUTC.Format(time.DateOnly)
	yesterday := nowUTC.AddDate(0, 0, -1).Format(time.DateOnly)
	tomorrow := nowUTC.AddDate(0, 0, 1).Format(time.DateOnly)

	cases := []struct {
		name string
		date string
		want model.FindingStatus
	}{
		{"expiry today is still accepted", today, model.FindingSnoozed},
		{"expiry tomorrow is still accepted", tomorrow, model.FindingSnoozed},
		{"expiry yesterday has lapsed", yesterday, model.FindingOpen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, expires := mapStatus("accepted", &tc.date)
			if got != tc.want {
				t.Fatalf("mapStatus(accepted, %q) = %q, want %q", tc.date, got, tc.want)
			}
			// Either way the expiry is reported as written, so the SLA due date
			// is measured from the date itself rather than from end-of-day.
			wantExpires, err := time.Parse(time.DateOnly, tc.date)
			if err != nil {
				t.Fatalf("time.Parse() error = %v", err)
			}
			if !expires.Equal(wantExpires) {
				t.Fatalf("expires = %v, want %v (the date as written)", expires, wantExpires)
			}
		})
	}
}

// TestMapStatusRFC3339ExpiryUsesItsOwnTime confirms a timestamped expiry is
// compared at its stated instant, with no end-of-day extension.
func TestMapStatusRFC3339ExpiryUsesItsOwnTime(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	if got, _ := mapStatus("accepted", &past); got != model.FindingOpen {
		t.Fatalf("mapStatus(accepted, one hour ago) = %q, want %q", got, model.FindingOpen)
	}
	if got, _ := mapStatus("accepted", &future); got != model.FindingSnoozed {
		t.Fatalf("mapStatus(accepted, one hour ahead) = %q, want %q", got, model.FindingSnoozed)
	}
}

// --- helpers for the correlation-failure tests ---

// correlationTestPage builds a one-page listing envelope for the fake API.
func correlationTestPage(t *testing.T, nodes any) pageEnvelope {
	t.Helper()
	raw, err := json.Marshal(nodes)
	if err != nil {
		t.Fatalf("marshal page results: %v", err)
	}
	return pageEnvelope{Count: 1, PageTotal: 1, Results: raw}
}

// correlationTestPageNoT is correlationTestPage for callers outside a test
// (the shared handler registrations).
func correlationTestPageNoT(nodes any) pageEnvelope {
	raw, err := json.Marshal(nodes)
	if err != nil {
		panic(err)
	}
	return pageEnvelope{Count: 1, PageTotal: 1, Results: raw}
}

// registerCorrelationTestAPI serves a minimal Snyk DAST API on mux: one
// target (project-a / example.test), definitions, and two findings, both
// with has_sast_correlations=true. The caller registers the per-finding
// correlation handlers under /targets/target-a/findings/{1,2}/... .
func registerCorrelationTestAPI(mux *http.ServeMux) {
	target := targetNode{ID: "target-a", Type: "web"}
	target.Site.Name = "project-a"
	target.Site.URL = "https://project-a.example.test/"
	target.Site.Host = "project-a.example.test"

	passive := false
	definition := definitionNode{ID: "def-1", Name: "Cross-site scripting", Passive: &passive}

	finding := func(id string) findingNode {
		var fn findingNode
		fn.ID = json.Number(id)
		fn.State = "notfixed"
		fn.Severity = 30
		fn.Definition.ID = "def-1"
		fn.Definition.Name = "Cross-site scripting"
		fn.HasSASTCorrelations = true
		fn.Target.ID = "target-a"
		fn.Target.Name = "project-a"
		return fn
	}

	mux.HandleFunc("/targets/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(correlationTestPageNoT([]targetNode{target}))
	})
	mux.HandleFunc("/findings/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(correlationTestPageNoT([]findingNode{finding("1"), finding("2")}))
	})
	mux.HandleFunc("/definitions/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(correlationTestPageNoT([]definitionNode{definition}))
	})
}

// TestEnrichCorrelationsReportsMissingPermission: a 403 from the correlation
// endpoint fails the snapshot with an error naming the missing
// correlation_viewer permission, and the message carries no target or
// finding URL.
func TestEnrichCorrelationsReportsMissingPermission(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	registerCorrelationTestAPI(mux)
	mux.HandleFunc("/targets/target-a/findings/1/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	mux.HandleFunc("/targets/target-a/findings/2/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})

	base, _ := url.Parse(server.URL + "/")
	c := &Client{
		apiBase:     base,
		httpClient:  server.Client(),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		concurrency: 2,
	}

	_, err := c.LoadSnapshot(context.Background())
	if err == nil {
		t.Fatal("LoadSnapshot() = nil error, want the missing-permission error")
	}
	msg := err.Error()
	if !errors.Is(err, ErrCorrelationForbidden) {
		t.Fatalf("LoadSnapshot() error = %v, want errors.Is ErrCorrelationForbidden", err)
	}
	if !strings.Contains(msg, "correlation_viewer") {
		t.Fatalf("error %q does not name the missing correlation_viewer permission", msg)
	}
	if strings.Contains(msg, server.URL) {
		t.Fatalf("error %q leaks the server URL %q", msg, server.URL)
	}
}

// TestEnrichCorrelationsToleratesNotFoundForOneFinding: a 404 for a single
// finding's correlations degrades to "no correlations" for that finding
// only; the rest of the snapshot loads and the other finding keeps its block.
func TestEnrichCorrelationsToleratesNotFoundForOneFinding(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	registerCorrelationTestAPI(mux)
	mux.HandleFunc("/targets/target-a/findings/1/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/targets/target-a/findings/2/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(correlationTestPage(t, []correlationNode{
			{ID: "corr-2", Confirmed: true, Markdown: "**Snyk Code:** XSS in `page.go:7`"},
		}))
	})

	base, _ := url.Parse(server.URL + "/")
	c := &Client{
		apiBase:     base,
		httpClient:  server.Client(),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		concurrency: 2,
	}

	snapshot, err := c.LoadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("LoadSnapshot() error = %v, want success with the 404 tolerated", err)
	}
	var first, second *model.Finding
	for i := range snapshot.Findings {
		switch snapshot.Findings[i].SnykDASTFindingID {
		case "1":
			first = &snapshot.Findings[i]
		case "2":
			second = &snapshot.Findings[i]
		}
	}
	if first == nil || second == nil {
		t.Fatalf("snapshot findings = %+v, want findings 1 and 2", snapshot.Findings)
	}
	if len(first.CorrelationMarkdown) != 0 {
		t.Fatalf("finding 1 CorrelationMarkdown = %#v, want none after its 404", first.CorrelationMarkdown)
	}
	if len(second.CorrelationMarkdown) != 1 || !strings.Contains(second.CorrelationMarkdown[0], "page.go:7") {
		t.Fatalf("finding 2 CorrelationMarkdown = %#v, want one block", second.CorrelationMarkdown)
	}
}

// TestEnrichCorrelationsStillFailsOnServerError: any other error from the
// correlation endpoint (here a 500, with a plain http.Client so the adaptive
// transport's retries are out of the picture) still fails the snapshot.
func TestEnrichCorrelationsStillFailsOnServerError(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	registerCorrelationTestAPI(mux)
	mux.HandleFunc("/targets/target-a/findings/1/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/targets/target-a/findings/2/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(correlationTestPage(t, []correlationNode{{ID: "corr-2", Markdown: "ok"}}))
	})

	base, _ := url.Parse(server.URL + "/")
	c := &Client{
		apiBase:     base,
		httpClient:  server.Client(),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		concurrency: 2,
	}

	if _, err := c.LoadSnapshot(context.Background()); err == nil {
		t.Fatal("LoadSnapshot() = nil error, want the 500 to fail the snapshot")
	} else if errors.Is(err, ErrCorrelationForbidden) {
		t.Fatalf("LoadSnapshot() error = %v, a server error must not map to the permission error", err)
	}
}

// TestEnrichCorrelationsSkipsUncorrelatedFindings pins existing behaviour: a
// finding with has_sast_correlations=false triggers no correlation request.
func TestEnrichCorrelationsSkipsUncorrelatedFindings(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	registerCorrelationTestAPI(mux)

	var correlationRequests atomic.Int64
	mux.HandleFunc("/targets/target-a/findings/1/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		correlationRequests.Add(1)
		json.NewEncoder(w).Encode(correlationTestPage(t, []correlationNode{{ID: "corr-1", Markdown: "ok"}}))
	})
	mux.HandleFunc("/targets/target-a/findings/2/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		correlationRequests.Add(1)
		json.NewEncoder(w).Encode(correlationTestPage(t, []correlationNode{{ID: "corr-2", Markdown: "ok"}}))
	})

	base, _ := url.Parse(server.URL + "/")
	c := &Client{
		apiBase:     base,
		httpClient:  server.Client(),
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		concurrency: 2,
	}

	// Drive enrichCorrelations directly: both findings correlated=false.
	findings := []model.Finding{
		{Fingerprint: "snyk-dast:target-a:1", SnykDASTFindingID: "1", TargetID: "target-a"},
		{Fingerprint: "snyk-dast:target-a:2", SnykDASTFindingID: "2", TargetID: "target-a"},
	}
	if err := c.enrichCorrelations(context.Background(), findings, []bool{false, false}); err != nil {
		t.Fatalf("enrichCorrelations() error = %v", err)
	}
	if got := correlationRequests.Load(); got != 0 {
		t.Fatalf("correlation endpoint received %d requests, want 0 for uncorrelated findings", got)
	}
}

// TestDecodeJSONErrorOmitsQueryString: a failed request's error names the
// method, path and status, but not the query string — the host and the
// filters must not leak into error excerpts pasted into public issues.
func TestDecodeJSONErrorOmitsQueryString(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/findings/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})

	resp, err := server.Client().Get(server.URL + "/findings/?length=100&page=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	c := &Client{}
	err = c.decodeJSON(resp, &pageEnvelope{})
	if err == nil {
		t.Fatal("decodeJSON() = nil error, want a 403 apiError")
	}
	msg := err.Error()
	if !strings.Contains(msg, "/findings/") {
		t.Fatalf("error %q does not name the path", msg)
	}
	if !strings.Contains(msg, "403") {
		t.Fatalf("error %q does not name the status", msg)
	}
	if strings.Contains(msg, "page=1") || strings.Contains(msg, "length=100") {
		t.Fatalf("error %q leaks the query string", msg)
	}
	if strings.Contains(msg, server.URL) {
		t.Fatalf("error %q leaks the server host %q", msg, server.URL)
	}
}

// TestLoadSnapshotFailsFastWithoutCorrelationViewer is the end-to-end check:
// the real transport stack (snykdast.New) against a fake API where the
// correlation endpoint answers 403. LoadSnapshot must return the typed
// permission error, and the server must see exactly one correlation request
// — a 403 is not retried.
func TestLoadSnapshotFailsFastWithoutCorrelationViewer(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	registerCorrelationTestAPI(mux)

	var correlationRequests atomic.Int64
	mux.HandleFunc("/targets/target-a/findings/1/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		correlationRequests.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	mux.HandleFunc("/targets/target-a/findings/2/integrations/snyk-sast/correlations/", func(w http.ResponseWriter, r *http.Request) {
		correlationRequests.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	})

	cfg := config.Config{
		SnykDAST: config.SnykDASTConfig{
			APIKey:  "test-key",
			APIBase: server.URL,
			AppBase: "https://app.example.test/",
		},
		// Concurrency 1 makes the fails-fast count deterministic: finding 1's
		// 403 cancels the errgroup before finding 2's fetch is sent.
		Sync: config.SyncConfig{SnykDASTConcurrency: 1},
	}
	c, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = c.LoadSnapshot(context.Background())
	if err == nil {
		t.Fatal("LoadSnapshot() = nil error, want the missing-permission error")
	}
	if !errors.Is(err, ErrCorrelationForbidden) {
		t.Fatalf("LoadSnapshot() error = %v, want errors.Is ErrCorrelationForbidden", err)
	}
	if !strings.Contains(err.Error(), "correlation_viewer") {
		t.Fatalf("error %q does not name the missing correlation_viewer permission", err.Error())
	}
	// The errgroup cancels the remaining fetches as soon as the first 403
	// maps to the permission error, and the adaptive transport does not retry
	// a 403: exactly one correlation request must reach the server.
	if got := correlationRequests.Load(); got != 1 {
		t.Fatalf("correlation endpoint received %d requests, want 1 (fails fast, no retries for a 403)", got)
	}
}

// --- tests for the unparseable-acceptance-expiry warning ---

// captureHandler is a slog handler that records every record so tests can
// assert on what the client logged.
type captureHandler struct {
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// loadSnapshotWithExpiry serves a minimal Snyk DAST API with one target and
// one accepted finding carrying the given expiration_date, then loads a
// snapshot through a client logging to a captureHandler.
func loadSnapshotWithExpiry(t *testing.T, expirationDate *string) (*captureHandler, model.SnykDASTSnapshot) {
	t.Helper()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	target := targetNode{ID: "target-a", Type: "web"}
	target.Site.Name = "project-a"
	target.Site.URL = "https://project-a.example.test/"
	target.Site.Host = "project-a.example.test"

	passive := false
	definition := definitionNode{ID: "def-1", Name: "Cross-site scripting", Passive: &passive}

	var fn findingNode
	fn.ID = json.Number("1")
	fn.State = "accepted"
	fn.Severity = 30
	fn.ExpirationDate = expirationDate
	fn.Definition.ID = "def-1"
	fn.Definition.Name = "Cross-site scripting"
	fn.Target.ID = "target-a"
	fn.Target.Name = "project-a"

	mux.HandleFunc("/targets/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(correlationTestPageNoT([]targetNode{target}))
	})
	mux.HandleFunc("/findings/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(correlationTestPageNoT([]findingNode{fn}))
	})
	mux.HandleFunc("/definitions/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(correlationTestPageNoT([]definitionNode{definition}))
	})

	base, _ := url.Parse(server.URL + "/")
	handler := &captureHandler{}
	c := &Client{
		apiBase:     base,
		httpClient:  server.Client(),
		logger:      slog.New(handler),
		concurrency: 2,
	}
	snapshot, err := c.LoadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("LoadSnapshot() error = %v, want success", err)
	}
	if len(snapshot.Findings) != 1 {
		t.Fatalf("LoadSnapshot() returned %d findings, want 1", len(snapshot.Findings))
	}
	return handler, snapshot
}

// TestLoadSnapshotWarnsOnUnparseableAcceptanceExpiry: an accepted finding
// whose expiration_date is present but unparseable still maps to ignored
// (a permanent acceptance, so the sync cancels the ticket), and LoadSnapshot
// logs a warning naming the finding and the value, so the cancellation is
// visible instead of silent.
func TestLoadSnapshotWarnsOnUnparseableAcceptanceExpiry(t *testing.T) {
	malformed := "soon-ish"
	handler, snapshot := loadSnapshotWithExpiry(t, &malformed)

	if got := snapshot.Findings[0].Status; got != model.FindingIgnored {
		t.Fatalf("status = %q, want %q (an unparseable acceptance stays a permanent cancellation)", got, model.FindingIgnored)
	}

	warned := false
	for _, r := range handler.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		var id, raw string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "finding_id":
				id = a.Value.String()
			case "expiration_date":
				raw = a.Value.String()
			}
			return true
		})
		if id != "1" || raw != malformed {
			t.Fatalf("warning %q has finding_id=%q expiration_date=%q, want finding_id=%q expiration_date=%q", r.Message, id, raw, "1", malformed)
		}
		warned = true
	}
	if !warned {
		t.Fatal("LoadSnapshot logged no warning naming the finding and the unparseable expiration_date")
	}
}

// TestLoadSnapshotDoesNotWarnOnParseableAcceptanceExpiry: an accepted finding
// with a well-formed (or absent) expiration_date follows the documented
// mappings and produces no warning.
func TestLoadSnapshotDoesNotWarnOnParseableAcceptanceExpiry(t *testing.T) {
	future := "2126-06-01"
	empty := ""
	for name, expirationDate := range map[string]*string{"future": &future, "absent": nil, "empty": &empty} {
		t.Run(name, func(t *testing.T) {
			handler, snapshot := loadSnapshotWithExpiry(t, expirationDate)
			for _, r := range handler.records {
				if r.Level == slog.LevelWarn && strings.Contains(r.Message, "unparseable") {
					t.Fatalf("LoadSnapshot logged an unexpected warning: %q", r.Message)
				}
			}
			want := model.FindingSnoozed
			if expirationDate == nil || *expirationDate == "" {
				want = model.FindingIgnored
			}
			if got := snapshot.Findings[0].Status; got != want {
				t.Fatalf("status = %q, want %q", got, want)
			}
		})
	}
}
