package snykdast

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
)

func TestMapStatus(t *testing.T) {
	future := "2126-06-01"
	past := "2000-01-01"

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
