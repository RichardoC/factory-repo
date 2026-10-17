package snyk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/tesslio/snyk-linear-sync/internal/cache"
	"github.com/tesslio/snyk-linear-sync/internal/model"
)

func TestMapStatusTemporaryIgnoreOpen(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	issue := issueAttributes{
		Ignored: true,
		Status:  "open",
	}
	got := mapStatus(issue, future, false)
	if got != model.FindingOpen {
		t.Fatalf("mapStatus() = %q, want %q for temporary ignore", got, model.FindingOpen)
	}
}

func TestMapStatusPermanentIgnoreCancelled(t *testing.T) {
	issue := issueAttributes{
		Ignored: true,
		Status:  "open",
	}
	got := mapStatus(issue, time.Time{}, false)
	if got != model.FindingIgnored {
		t.Fatalf("mapStatus() = %q, want %q for permanent ignore", got, model.FindingIgnored)
	}
}

func TestMapStatusExpiredIgnoreKeepsOpen(t *testing.T) {
	// When a snooze expires but Snyk still reports ignored=true, the sync
	// should treat the finding as open (not ignored) to prevent flapping.
	// Mapping to FindingIgnored (Cancelled) would trigger the reopen guard
	// when the snooze is re-applied, creating duplicate tickets.
	past := time.Now().Add(-24 * time.Hour)
	issue := issueAttributes{
		Ignored: true,
		Status:  "open",
	}
	got := mapStatus(issue, past, false)
	if got != model.FindingOpen {
		t.Fatalf("mapStatus() = %q, want %q for expired temporary ignore (snooze expired but still ignored)", got, model.FindingOpen)
	}
}

// TestPermanentRuleIgnoreNotMaskedByExpiredKeySnooze reproduces SNYK-56009: a
// config finding whose issue-key hash carries only long-expired
// temporary-ignores, while the permanent wont-fix that actually keeps Snyk
// reporting ignored=true is filed under the Snyk rule ID. Gathering ignore
// records only under the issue-key hash sees the expired snooze and maps the
// finding to FindingOpen, minting a fresh birth-overdue ticket every run. The
// union across candidate keys must instead see the permanent ignore and map it
// to FindingIgnored (Cancelled).
func TestPermanentRuleIgnoreNotMaskedByExpiredKeySnooze(t *testing.T) {
	const (
		issueKeyHash = "1f9ffd2622ca886d18aebec3c3ca5ba5bb69f61b"
		issueID      = "d6dc045a-339e-40d3-8f94-095c2ed870fa"
		ruleID       = "SNYK-CC-K8S-42"
	)
	projectIgnores := v1ProjectIgnores{
		// Two temporary snoozes filed under the issue-key hash, both long expired.
		issueKeyHash: {
			{Created: "2026-04-15T13:17:41Z", Expires: "2026-04-29T13:17:39Z"},
			{Created: "2026-04-15T13:30:25Z", Expires: "2026-04-29T13:30:20Z"},
		},
		// The rule-level records: an older expired snooze and the permanent
		// wont-fix (no expiry) that is the latest action and keeps it ignored.
		ruleID: {
			{Created: "2026-04-16T13:22:35Z", Expires: "2026-04-30T13:22:35Z"},
			{Created: "2026-05-06T14:54:20Z", Expires: ""},
		},
	}

	problems := []problem{{ID: ruleID}}
	entries := ignoreEntriesForIssue(projectIgnores, issueKeyHash, issueID, problems, true)
	meta := maxExpiryIgnoreMeta(entries)
	if !meta.ExpiresAt.IsZero() {
		t.Fatalf("ignore meta ExpiresAt = %v, want zero (permanent) — expired key snooze masked the permanent rule ignore", meta.ExpiresAt)
	}

	issue := issueAttributes{Ignored: true, Status: "open"}
	if got := mapStatus(issue, meta.ExpiresAt, meta.DisregardIfFixable); got != model.FindingIgnored {
		t.Fatalf("mapStatus() = %q, want %q for finding permanently ignored via its rule ID", got, model.FindingIgnored)
	}
}

// TestRuleIgnoresNotAppliedToUnignoredFinding pins the other side of the
// rule-key union: rule IDs identify a class of finding, not one finding, so a
// sibling issue of the same rule that Snyk does NOT report as ignored must not
// inherit the rule-keyed records. Without the ignored gate it would pick up an
// ExpiresAt and have its due-date base silently switched from creation date to
// ignore expiry.
func TestRuleIgnoresNotAppliedToUnignoredFinding(t *testing.T) {
	const ruleID = "SNYK-CC-K8S-8"
	projectIgnores := v1ProjectIgnores{
		ruleID: {
			{Created: "2026-05-06T14:54:20Z", Expires: "2027-01-01T00:00:00Z"},
		},
	}

	problems := []problem{{ID: ruleID}}
	entries := ignoreEntriesForIssue(projectIgnores, "aaaa1111-unrelated-hash", "bbbb2222-issue-uuid", problems, false)
	if len(entries) != 0 {
		t.Fatalf("ignoreEntriesForIssue() returned %d entries for an unignored finding, want 0 — rule-keyed records leaked onto a sibling", len(entries))
	}

	// A finding with no issue.Attributes.Key must not smuggle its problem ID
	// past the gate either: the caller passes the raw (empty) attributes key,
	// not the coalesced issueKey fallback that resolves to the rule ID.
	entries = ignoreEntriesForIssue(projectIgnores, "", "bbbb2222-issue-uuid", problems, false)
	if len(entries) != 0 {
		t.Fatalf("ignoreEntriesForIssue() returned %d entries for an unignored finding with an empty attributes key, want 0 — rule key smuggled past the ignored gate", len(entries))
	}

	// Sanity: the same finding, once Snyk reports it ignored, does see the
	// rule-keyed records.
	entries = ignoreEntriesForIssue(projectIgnores, "", "bbbb2222-issue-uuid", problems, true)
	if len(entries) != 1 {
		t.Fatalf("ignoreEntriesForIssue() returned %d entries for an ignored finding, want 1", len(entries))
	}
}

func TestMapStatusDisregardIfFixableAwaitingFix(t *testing.T) {
	issue := issueAttributes{
		Ignored: true,
		Status:  "open",
	}
	got := mapStatus(issue, time.Time{}, true)
	if got != model.FindingAwaitingFix {
		t.Fatalf("mapStatus() = %q, want %q for disregard-if-fixable ignore", got, model.FindingAwaitingFix)
	}
}

func TestMapStatusWontFixIsNotFixed(t *testing.T) {
	// "wont_fix" contains the substring "fix" but does NOT mean the issue
	// was fixed. It must not map to FindingFixed (Done). When ignored=false
	// and status is not resolved, it should fall through to FindingOpen.
	issue := issueAttributes{
		Ignored:    false,
		Status:     "open",
		Resolution: resolution{Type: "wont_fix"},
	}
	got := mapStatus(issue, time.Time{}, false)
	if got != model.FindingOpen {
		t.Fatalf("mapStatus(wont_fix) = %q, want %q (must not be treated as fixed)", got, model.FindingOpen)
	}
}

func TestMapStatusFixedResolutionIsFixed(t *testing.T) {
	issue := issueAttributes{
		Ignored:    false,
		Status:     "open",
		Resolution: resolution{Type: "fixed"},
	}
	got := mapStatus(issue, time.Time{}, false)
	if got != model.FindingFixed {
		t.Fatalf("mapStatus(fixed) = %q, want %q", got, model.FindingFixed)
	}
}

func TestMapStatusDisregardIfFixableTakesPrecedenceOverExpiry(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour)
	issue := issueAttributes{
		Ignored: true,
		Status:  "open",
	}
	// Even with a past expiry, disregardIfFixable returns FindingAwaitingFix.
	got := mapStatus(issue, past, true)
	if got != model.FindingAwaitingFix {
		t.Fatalf("mapStatus() = %q, want %q for disregard-if-fixable with past expiry", got, model.FindingAwaitingFix)
	}
}

func TestMapStatusResolvedIgnoredWithExpiryIsFixed(t *testing.T) {
	// A finding Snyk reports as resolved must be terminal even when it still
	// carries an ignore with an expiry. Without the resolved-first check this
	// hit the expired-snooze branch and returned FindingOpen, producing an
	// open (and immediately overdue) ticket for a vulnerability that no longer
	// exists.
	past := time.Now().Add(-24 * time.Hour)
	issue := issueAttributes{
		Ignored:    true,
		Status:     "resolved",
		Resolution: resolution{Type: "disappeared"},
	}
	got := mapStatus(issue, past, false)
	if got != model.FindingFixed {
		t.Fatalf("mapStatus() = %q, want %q for resolved finding with expired ignore", got, model.FindingFixed)
	}
}

func TestMapStatusResolvedIgnoredActiveSnoozeIsFixed(t *testing.T) {
	// An active (future-dated) snooze must not keep a resolved finding open
	// either — the finding is gone regardless of the snooze window.
	future := time.Now().Add(24 * time.Hour)
	issue := issueAttributes{
		Ignored:    true,
		Status:     "resolved",
		Resolution: resolution{Type: "disappeared"},
	}
	got := mapStatus(issue, future, false)
	if got != model.FindingFixed {
		t.Fatalf("mapStatus() = %q, want %q for resolved finding with active snooze", got, model.FindingFixed)
	}
}

func TestMapStatusResolvedIgnoredDisregardIfFixableIsFixed(t *testing.T) {
	// disregard-if-fixable normally routes to FindingAwaitingFix (Backlog), a
	// non-terminal state. A resolved finding must still be terminal: the vuln
	// disappeared, so it is no longer awaiting anything.
	issue := issueAttributes{
		Ignored:    true,
		Status:     "resolved",
		Resolution: resolution{Type: "disappeared"},
	}
	got := mapStatus(issue, time.Time{}, true)
	if got != model.FindingFixed {
		t.Fatalf("mapStatus() = %q, want %q for resolved finding with disregard-if-fixable ignore", got, model.FindingFixed)
	}
}

func TestMapStatusResolvedCoordinatesIgnoredIsFixed(t *testing.T) {
	// The resolved signal can come from the coordinates alone (all resolved),
	// with an empty top-level status. That must also override an ignore.
	past := time.Now().Add(-24 * time.Hour)
	issue := issueAttributes{
		Ignored: true,
		Coordinates: []coordinate{
			{State: "resolved"},
			{State: "resolved"},
		},
	}
	got := mapStatus(issue, past, false)
	if got != model.FindingFixed {
		t.Fatalf("mapStatus() = %q, want %q for finding with all coordinates resolved and an ignore", got, model.FindingFixed)
	}
}

func TestMapStatusIgnoredWithUnresolvedCoordinateStaysOpen(t *testing.T) {
	// Guard against the resolved check over-firing: a finding with a still-open
	// coordinate is not resolved, so an active ignore must still keep it open.
	future := time.Now().Add(24 * time.Hour)
	issue := issueAttributes{
		Ignored: true,
		Status:  "open",
		Coordinates: []coordinate{
			{State: "resolved"},
			{State: "open"},
		},
	}
	got := mapStatus(issue, future, false)
	if got != model.FindingOpen {
		t.Fatalf("mapStatus() = %q, want %q for ignored finding with a still-open coordinate", got, model.FindingOpen)
	}
}

func TestMapStatusResolvedWithSnoozeMarkerIsFixed(t *testing.T) {
	// Resolved must win over a snooze marker: a finding Snyk no longer reports
	// is terminal even if its resolution type or details still mentions a snooze.
	for _, res := range []resolution{
		{Type: "snoozed"},
		{Type: "disappeared", Details: "snoozed then disappeared"},
	} {
		issue := issueAttributes{
			Status:     "resolved",
			Resolution: res,
		}
		got := mapStatus(issue, time.Time{}, false)
		if got != model.FindingFixed {
			t.Fatalf("mapStatus(%+v) = %q, want %q for resolved finding with snooze marker", res, got, model.FindingFixed)
		}
	}
}

func TestMapStatusUnresolvedSnoozeStaysSnoozed(t *testing.T) {
	// The reordering must not change behaviour for still-open findings: a snooze
	// marker on an unresolved finding still maps to FindingSnoozed.
	issue := issueAttributes{
		Status:     "open",
		Resolution: resolution{Type: "snoozed"},
	}
	got := mapStatus(issue, time.Time{}, false)
	if got != model.FindingSnoozed {
		t.Fatalf("mapStatus() = %q, want %q for unresolved snoozed finding", got, model.FindingSnoozed)
	}
}

func TestMaxExpiryIgnoreMeta(t *testing.T) {
	cases := []struct {
		name    string
		entries []v1IgnoreEntry
		want    ignoreMetadata
	}{
		{
			name: "single temporary ignore",
			entries: []v1IgnoreEntry{
				{Created: "2026-03-18T12:00:00Z", Expires: "2026-04-18T12:00:00Z"},
			},
			want: ignoreMetadata{
				ExpiresAt: time.Date(2026, time.April, 18, 12, 0, 0, 0, time.UTC),
				CreatedAt: time.Date(2026, time.March, 18, 12, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "permanent ignore no expires",
			entries: []v1IgnoreEntry{
				{Created: "2026-03-18T12:00:00Z", Expires: ""},
			},
			want: ignoreMetadata{
				CreatedAt: time.Date(2026, time.March, 18, 12, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "multiple ignores picks max expiry",
			entries: []v1IgnoreEntry{
				{Created: "2026-03-18T12:00:00Z", Expires: "2026-04-18T12:00:00Z"},
				{Created: "2026-04-01T12:00:00Z", Expires: "2026-05-01T12:00:00Z"},
			},
			want: ignoreMetadata{
				ExpiresAt: time.Date(2026, time.May, 1, 12, 0, 0, 0, time.UTC),
				CreatedAt: time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "max expiry can come from an older entry",
			entries: []v1IgnoreEntry{
				{Created: "2026-03-18T12:00:00Z", Expires: "2026-06-17T23:00:00Z"},
				{Created: "2026-04-01T12:00:00Z", Expires: "2026-06-14T23:00:00Z"},
			},
			want: ignoreMetadata{
				ExpiresAt:          time.Date(2026, time.June, 17, 23, 0, 0, 0, time.UTC),
				CreatedAt:          time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
				DisregardIfFixable: false,
			},
		},
		{
			name: "latest permanent ignore overrides earlier snooze expiry",
			entries: []v1IgnoreEntry{
				{Created: "2026-03-18T12:00:00Z", Expires: "2026-04-18T12:00:00Z"},
				{Created: "2026-04-01T12:00:00Z", Expires: ""},
			},
			want: ignoreMetadata{
				ExpiresAt: time.Time{}, // permanent ignore — no expiry
				CreatedAt: time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "ignore until fix available",
			entries: []v1IgnoreEntry{
				{Created: "2026-03-18T12:00:00Z", Expires: "", DisregardIfFixable: true},
			},
			want: ignoreMetadata{
				DisregardIfFixable: true,
				CreatedAt:          time.Date(2026, time.March, 18, 12, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "multiple snoozes all expired picks max expiry",
			entries: []v1IgnoreEntry{
				{Created: "2026-04-10T17:14:57Z", Expires: "2026-04-29T00:00:00Z"},
				{Created: "2026-05-29T09:35:35Z", Expires: "2026-06-11T00:00:00Z"},
				{Created: "2026-07-02T15:01:44Z", Expires: "2026-07-15T23:00:00Z"},
			},
			want: ignoreMetadata{
				ExpiresAt: time.Date(2026, time.July, 15, 23, 0, 0, 0, time.UTC),
				CreatedAt: time.Date(2026, time.July, 2, 15, 1, 44, 0, time.UTC),
			},
		},
		{
			name: "disregardIfFixable from latest created entry",
			entries: []v1IgnoreEntry{
				{Created: "2026-03-18T12:00:00Z", Expires: "2026-04-18T12:00:00Z", DisregardIfFixable: true},
				{Created: "2026-04-01T12:00:00Z", Expires: "2026-05-01T12:00:00Z", DisregardIfFixable: false},
			},
			want: ignoreMetadata{
				ExpiresAt:          time.Date(2026, time.May, 1, 12, 0, 0, 0, time.UTC),
				CreatedAt:          time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC),
				DisregardIfFixable: false,
			},
		},
		{
			// Fix 4: a single entry with a legitimate future expiry but an
			// empty/unparseable Created must NOT have its expiry zeroed. With
			// only one entry we can't determine "latest" reliably (Created
			// didn't parse), so the permanent-override must be skipped
			// entirely rather than defaulting to "no expiry seen yet".
			name: "unparseable created with future expiry keeps expiry",
			entries: []v1IgnoreEntry{
				{Created: "", Expires: "2026-08-18T12:00:00Z"},
			},
			want: ignoreMetadata{
				ExpiresAt: time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC),
			},
		},
		{
			// Fix 4: one entry parses fine and looks like a permanent ignore,
			// but another entry has an unparseable Created and a real expiry.
			// We can't reliably tell which entry is actually latest, so the
			// ambiguous case must NOT zero the expiry — that would silently
			// convert an active snooze into a permanent ignore.
			name: "ambiguous latest entry due to unparseable created keeps expiry",
			entries: []v1IgnoreEntry{
				{Created: "2026-03-18T12:00:00Z", Expires: ""},
				{Created: "", Expires: "2026-08-18T12:00:00Z"},
			},
			want: ignoreMetadata{
				ExpiresAt: time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC),
				CreatedAt: time.Date(2026, time.March, 18, 12, 0, 0, 0, time.UTC),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maxExpiryIgnoreMeta(tc.entries)
			if !got.ExpiresAt.Equal(tc.want.ExpiresAt) {
				t.Fatalf("maxExpiryIgnoreMeta().ExpiresAt = %v, want %v", got.ExpiresAt, tc.want.ExpiresAt)
			}
			if !got.CreatedAt.Equal(tc.want.CreatedAt) {
				t.Fatalf("maxExpiryIgnoreMeta().CreatedAt = %v, want %v", got.CreatedAt, tc.want.CreatedAt)
			}
			if got.DisregardIfFixable != tc.want.DisregardIfFixable {
				t.Fatalf("maxExpiryIgnoreMeta().DisregardIfFixable = %v, want %v", got.DisregardIfFixable, tc.want.DisregardIfFixable)
			}
		})
	}
}

func TestV1IgnoreEntryUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want v1IgnoreEntry
	}{
		{
			name: "flat format",
			raw:  `{"created":"2026-03-18T12:00:00Z","expires":"2026-04-18T12:00:00Z"}`,
			want: v1IgnoreEntry{Created: "2026-03-18T12:00:00Z", Expires: "2026-04-18T12:00:00Z"},
		},
		{
			name: "nested format",
			raw:  `{"*":{"created":"2026-03-18T12:00:00Z","expires":"2026-04-18T12:00:00Z"}}`,
			want: v1IgnoreEntry{Created: "2026-03-18T12:00:00Z", Expires: "2026-04-18T12:00:00Z"},
		},
		{
			name: "nested format with different path key",
			raw:  `{"path1":{"created":"2026-03-18T12:00:00Z","expires":"2026-04-18T12:00:00Z"}}`,
			want: v1IgnoreEntry{Created: "2026-03-18T12:00:00Z", Expires: "2026-04-18T12:00:00Z"},
		},
		{
			name: "flat format with disregardIfFixable",
			raw:  `{"created":"2026-03-18T12:00:00Z","expires":"","disregardIfFixable":true}`,
			want: v1IgnoreEntry{Created: "2026-03-18T12:00:00Z", DisregardIfFixable: true},
		},
		{
			name: "nested format with disregardIfFixable",
			raw:  `{"*":{"created":"2026-03-18T12:00:00Z","expires":"","disregardIfFixable":true}}`,
			want: v1IgnoreEntry{Created: "2026-03-18T12:00:00Z", DisregardIfFixable: true},
		},
		{
			name: "unparsable",
			raw:  `"not an object"`,
			want: v1IgnoreEntry{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got v1IgnoreEntry
			if err := got.UnmarshalJSON([]byte(tc.raw)); err != nil {
				t.Fatalf("UnmarshalJSON() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("UnmarshalJSON() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestKubernetesNamespace(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		want   string
	}{
		// Real shapes observed from the Snyk API (all 42 kubernetes projects
		// in the org matched ns/kind.group/workload:target).
		{"backend/deployment.apps/backend:/app/package.json", "kubernetes", "backend"},
		{"litellm/job.batch/litellm-db-migration:/opt/prisma/binaries/package.json", "kubernetes", "litellm"},
		{"cert-manager/job.batch/cert-manager-startupapicheck:quay.io/jetstack/cert-manager-startupapicheck", "kubernetes", "cert-manager"},
		// Other origins never get a namespace.
		{"owner/repo(main):package-lock.json", "github", ""},
		{"alpine:3.19", "docker-hub", ""},
		// Malformed kubernetes names yield no namespace rather than garbage.
		{"", "kubernetes", ""},
		{"no-slash", "kubernetes", ""},
		{"/deployment.apps/x:img", "kubernetes", ""},
		{"ns/", "kubernetes", ""},
	}
	for _, tc := range cases {
		got := kubernetesNamespace(tc.name, tc.origin)
		if got != tc.want {
			t.Errorf("kubernetesNamespace(%q, %q) = %q, want %q", tc.name, tc.origin, got, tc.want)
		}
	}
}

func TestFetchProjectCluster(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The v1 API 406s the JSON:API media type; assert we ask for plain JSON.
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		if r.URL.Path != "/org/test-org/project/test-project" {
			t.Errorf("path = %q, want /org/test-org/project/test-project", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Shape observed from the live v1 API (subset).
		if _, err := fmt.Fprint(w, `{"name":"backend/deployment.apps/backend:/app/package.json","origin":"kubernetes","imageCluster":"production","imageTag":"1.0.0"}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c := &Client{
		httpClient: server.Client(),
		v1Base:     base,
		orgID:      "test-org",
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	cluster, err := c.fetchProjectCluster(context.Background(), "test-project")
	if err != nil {
		t.Fatalf("fetchProjectCluster() error = %v", err)
	}
	if cluster != "production" {
		t.Fatalf("cluster = %q, want production", cluster)
	}
}

func TestFetchProjectClusterEmptyWhenNotReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprint(w, `{"name":"x","origin":"github"}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c := &Client{
		httpClient: server.Client(),
		v1Base:     base,
		orgID:      "test-org",
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	cluster, err := c.fetchProjectCluster(context.Background(), "test-project")
	if err != nil {
		t.Fatalf("fetchProjectCluster() error = %v", err)
	}
	if cluster != "" {
		t.Fatalf("cluster = %q, want empty when imageCluster absent", cluster)
	}
}

func TestFetchProjectClusterHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c := &Client{
		httpClient: server.Client(),
		v1Base:     base,
		orgID:      "test-org",
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if _, err := c.fetchProjectCluster(context.Background(), "test-project"); err == nil {
		t.Fatal("fetchProjectCluster() error = nil, want non-nil on 403")
	}
}

func TestIsActiveProjectStatus(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"active", true},
		{"Active", true},
		{"ACTIVE", true},
		{"inactive", false},
		{"Inactive", false},
		{"INACTIVE", false},
		// Unknown or empty status is treated as active for forward-compatibility.
		{"", true},
		{"unknown", true},
	}
	for _, tc := range cases {
		got := isActiveProjectStatus(tc.status)
		if got != tc.want {
			t.Errorf("isActiveProjectStatus(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestIssueClasses(t *testing.T) {
	cases := []struct {
		name    string
		classes []classEntry
		want    []model.IssueClass
	}{
		{
			name:    "nil",
			classes: nil,
			want:    nil,
		},
		{
			name: "drops empty ids",
			classes: []classEntry{
				{ID: "", Source: "CWE"},
				{ID: "CWE-22", Source: "CWE"},
			},
			want: []model.IssueClass{{ID: "CWE-22", Source: "CWE"}},
		},
		{
			name: "preserves ordering and trims whitespace",
			classes: []classEntry{
				{ID: "  CWE-22  ", Source: " CWE "},
				{ID: "CWE-78", Source: "CWE"},
			},
			want: []model.IssueClass{
				{ID: "CWE-22", Source: "CWE"},
				{ID: "CWE-78", Source: "CWE"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := issueClasses(tc.classes)
			if len(got) != len(tc.want) {
				t.Fatalf("issueClasses() = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("issueClasses()[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCVEIDs(t *testing.T) {
	cases := []struct {
		name     string
		problems []problem
		want     []string
	}{
		{
			name:     "no problems",
			problems: nil,
			want:     nil,
		},
		{
			name: "matches CVE id prefix regardless of source and deduplicates",
			problems: []problem{
				{ID: "SNYK-DEBIAN-ZLIB-1", Source: "SNYK"},
				{ID: "CVE-2024-12345", Source: "NVD"},
				{ID: "CVE-2024-12345", Source: "NVD"},
				{ID: "cve-2024-99999", Source: "NVD"},
				{ID: "CVE-2024-11111", Source: "Red Hat"},
				{ID: "NOT-A-CVE", Source: "CVE"},
				{ID: "", Source: "CVE"},
			},
			want: []string{"CVE-2024-12345", "cve-2024-99999", "CVE-2024-11111"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cveIDs(tc.problems)
			if len(got) != len(tc.want) {
				t.Fatalf("cveIDs() = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("cveIDs()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestAnyFixable(t *testing.T) {
	coords := []coordinate{
		{IsFixableManually: false, IsFixableSnyk: false},
		{IsFixableManually: true, IsFixableSnyk: false},
	}
	if !anyFixable(coords, func(c coordinate) bool { return c.IsFixableManually }) {
		t.Fatal("anyFixable(IsFixableManually) = false, want true")
	}
	if anyFixable(coords, func(c coordinate) bool { return c.IsFixableSnyk }) {
		t.Fatal("anyFixable(IsFixableSnyk) = true, want false")
	}
	if anyFixable(nil, func(c coordinate) bool { return c.IsFixableSnyk }) {
		t.Fatal("anyFixable(nil) = true, want false")
	}
	upgradeOnly := []coordinate{{IsUpgradeable: true}}
	if !anyFixable(upgradeOnly, func(c coordinate) bool { return c.IsUpgradeable }) {
		t.Fatal("anyFixable(IsUpgradeable) = false, want true")
	}
	pinOnly := []coordinate{{IsPinnable: true}}
	if !anyFixable(pinOnly, func(c coordinate) bool { return c.IsPinnable }) {
		t.Fatal("anyFixable(IsPinnable) = false, want true")
	}
}

func TestRemediationDescription(t *testing.T) {
	cases := []struct {
		name   string
		coords []coordinate
		want   string
	}{
		{name: "no coordinates", coords: nil, want: ""},
		{name: "empty remedies", coords: []coordinate{{Remedies: nil}}, want: ""},
		{name: "aggregates all non-empty in order, dedupes",
			coords: []coordinate{
				{Remedies: []remedy{{Description: "  "}}},
				{Remedies: []remedy{{Description: "Upgrade pkg to 1.2.3"}}},
				{Remedies: []remedy{{Description: "Pin pkg to 1.0.0"}, {Description: "Upgrade pkg to 1.2.3"}}},
			},
			want: "Upgrade pkg to 1.2.3\n\nPin pkg to 1.0.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := remediationDescription(tc.coords)
			if got != tc.want {
				t.Fatalf("remediationDescription() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelectCVSS(t *testing.T) {
	cases := []struct {
		name       string
		severities []severityEntry
		want       float64
	}{
		{name: "nil", severities: nil, want: 0},
		{name: "no scores", severities: []severityEntry{{Source: "NVD"}}, want: 0},
		{name: "single snyk", severities: []severityEntry{{Source: "Snyk", Score: new(7.5)}}, want: 7.5},
		{name: "prefers snyk over nvd", severities: []severityEntry{
			{Source: "NVD", Score: new(7.5)},
			{Source: "Snyk", Score: new(9.8)},
		}, want: 9.8},
		{name: "prefers red hat over nvd but not snyk", severities: []severityEntry{
			{Source: "Snyk", Score: new(9.8)},
			{Source: "Red Hat", Score: new(5.4)},
			{Source: "NVD", Score: new(6.0)},
		}, want: 9.8},
		{name: "red hat beats nvd when no snyk", severities: []severityEntry{
			{Source: "NVD", Score: new(6.0)},
			{Source: "Red Hat", Score: new(5.4)},
		}, want: 5.4},
		{name: "highest within same source wins", severities: []severityEntry{
			{Source: "Snyk", Score: new(9.3)}, // CVSSv4
			{Source: "Snyk", Score: new(9.8)}, // CVSSv3.1
		}, want: 9.8},
		{name: "falls back to other source", severities: []severityEntry{
			{Source: "SUSE", Score: new(4.2)},
		}, want: 4.2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectCVSS(tc.severities)
			if got != tc.want {
				t.Fatalf("selectCVSS() = %v, want %v", got, tc.want)
			}
		})
	}
}

//go:fix inline
func ptr(f float64) *float64 { return new(f) }

func TestMergeIgnoresPicksLatestExpiry(t *testing.T) {
	api := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{{Created: "2026-06-01T00:00:00Z", Expires: "2026-06-14T23:00:00Z"}},
	}
	cached := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{{Created: "2026-05-29T00:00:00Z", Expires: "2026-06-17T23:00:00Z"}},
	}

	merged := mergeIgnores(api, cached)
	entries, ok := merged["SNYK-1"]
	if !ok {
		t.Fatal("merged missing SNYK-1")
	}
	// mergeIgnores now returns the union of raw entries rather than a single
	// synthetic entry, so the two distinct (Created, Expires) pairs are both
	// preserved; maxExpiryIgnoreMeta is what summarizes them.
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	meta := maxExpiryIgnoreMeta(entries)
	if meta.ExpiresAt.Format(time.RFC3339) != "2026-06-17T23:00:00Z" {
		t.Fatalf("expires = %q, want 2026-06-17T23:00:00Z", meta.ExpiresAt.Format(time.RFC3339))
	}
	if meta.CreatedAt.Format(time.RFC3339) != "2026-06-01T00:00:00Z" {
		t.Fatalf("created = %q, want 2026-06-01T00:00:00Z (latest created from API)", meta.CreatedAt.Format(time.RFC3339))
	}
}

func TestFetchProjectIgnoresRetriesAndUsesMaxExpiry(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		var expires string
		if requestCount == 1 {
			expires = "2026-06-14T23:00:00Z"
		} else {
			expires = "2026-06-17T23:00:00Z"
		}

		body := map[string][]v1IgnoreEntry{
			"SNYK-1": {
				{Created: "2026-06-01T00:00:00Z", Expires: expires},
			},
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Fatalf("encode mock response: %v", err)
		}
	}))
	defer server.Close()

	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c := &Client{
		httpClient: server.Client(),
		v1Base:     base,
		orgID:      "test-org",
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ignores, err := c.fetchProjectIgnores(context.Background(), "test-project")
	if err != nil {
		t.Fatalf("fetchProjectIgnores() error = %v", err)
	}
	if requestCount != 3 {
		t.Fatalf("requestCount = %d, want 3", requestCount)
	}

	entries, ok := ignores["SNYK-1"]
	if !ok {
		t.Fatal("ignores missing SNYK-1")
	}
	// mergeIgnores now returns the deduplicated union of raw entries seen
	// across attempts rather than a single synthetic entry, so both distinct
	// expiries observed across the flaky attempts survive; maxExpiryIgnoreMeta
	// is what picks the maximum.
	meta := maxExpiryIgnoreMeta(entries)
	if meta.ExpiresAt.Format(time.RFC3339) != "2026-06-17T23:00:00Z" {
		t.Fatalf("expires = %q, want 2026-06-17T23:00:00Z", meta.ExpiresAt.Format(time.RFC3339))
	}
}

// TestFetchProjectIgnoresRetriesOn404 verifies Fix 3: a 404 from the v1
// ignores endpoint is retried like any other failure instead of being
// accepted immediately. The server 404s on the first attempt and returns
// real data on the remaining attempts (fetchProjectIgnoresWithRetry always
// runs all maxAttempts to find the max expiry, it doesn't stop at the first
// success), so the retry must recover the data rather than returning empty.
func TestFetchProjectIgnoresRetriesOn404(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body := map[string][]v1IgnoreEntry{
			"SNYK-1": {{Created: "2026-06-01T00:00:00Z", Expires: "2026-06-17T23:00:00Z"}},
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Fatalf("encode mock response: %v", err)
		}
	}))
	defer server.Close()

	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c := &Client{
		httpClient: server.Client(),
		v1Base:     base,
		orgID:      "test-org",
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ignores, err := c.fetchProjectIgnores(context.Background(), "test-project")
	if err != nil {
		t.Fatalf("fetchProjectIgnores() error = %v", err)
	}
	if requestCount != 3 {
		t.Fatalf("requestCount = %d, want 3 (404 on attempt 1 must be retried, not accepted immediately)", requestCount)
	}

	meta := maxExpiryIgnoreMeta(ignores["SNYK-1"])
	if meta.ExpiresAt.Format(time.RFC3339) != "2026-06-17T23:00:00Z" {
		t.Fatalf("expires = %q, want 2026-06-17T23:00:00Z (data from the retries after the 404 must survive)", meta.ExpiresAt.Format(time.RFC3339))
	}
}

// TestFetchProjectIgnoresLater404KeepsEarlierAttemptsData verifies the
// existing non-fatal behavior is preserved: if an earlier attempt returned
// real data and a later attempt 404s, the earlier data must not be erased.
func TestFetchProjectIgnoresLater404KeepsEarlierAttemptsData(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			body := map[string][]v1IgnoreEntry{
				"SNYK-1": {{Created: "2026-06-01T00:00:00Z", Expires: "2026-06-17T23:00:00Z"}},
			}
			if err := json.NewEncoder(w).Encode(body); err != nil {
				t.Fatalf("encode mock response: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c := &Client{
		httpClient: server.Client(),
		v1Base:     base,
		orgID:      "test-org",
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ignores, err := c.fetchProjectIgnores(context.Background(), "test-project")
	if err != nil {
		t.Fatalf("fetchProjectIgnores() error = %v", err)
	}
	if requestCount != 3 {
		t.Fatalf("requestCount = %d, want 3", requestCount)
	}

	meta := maxExpiryIgnoreMeta(ignores["SNYK-1"])
	if meta.ExpiresAt.Format(time.RFC3339) != "2026-06-17T23:00:00Z" {
		t.Fatalf("expires = %q, want 2026-06-17T23:00:00Z (a later 404 must not erase an earlier attempt's data)", meta.ExpiresAt.Format(time.RFC3339))
	}
}

// TestFetchProjectIgnoresPersistent404IsNonFatal verifies that if every
// attempt 404s, fetchProjectIgnores still succeeds non-fatally with an empty
// result (relying on the cache merge / caller fallback), matching the
// pre-existing behavior — it just now gets there after retrying instead of
// accepting the first 404.
func TestFetchProjectIgnoresPersistent404IsNonFatal(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c := &Client{
		httpClient: server.Client(),
		v1Base:     base,
		orgID:      "test-org",
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ignores, err := c.fetchProjectIgnores(context.Background(), "test-project")
	if err != nil {
		t.Fatalf("fetchProjectIgnores() error = %v, want nil (persistent 404 is non-fatal)", err)
	}
	if requestCount != 3 {
		t.Fatalf("requestCount = %d, want 3 (404 must be retried like any other failure)", requestCount)
	}
	if len(ignores) != 0 {
		t.Fatalf("ignores = %+v, want empty", ignores)
	}
}

// TestFetchProjectIgnoresPreservesUpdatedAtForUnconfirmedKeys is the
// findings.go-level integration test for Fix 2: an issue key that is present
// in the cache but NOT reported by the API this run must keep its original
// cached updated_at (so it eventually ages out past ignoreEntryTTL), while a
// key the API does confirm this run gets its updated_at refreshed.
func TestFetchProjectIgnoresPreservesUpdatedAtForUnconfirmedKeys(t *testing.T) {
	dir := t.TempDir()
	store, err := cache.Open(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("cache.Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	projectID := "test-project"

	staleUpdatedAt := time.Now().UTC().Add(-3 * 24 * time.Hour).Truncate(time.Second)
	seeded := map[string]cache.IgnoreMeta{
		"SNYK-STALE": {
			IssueKey:  "SNYK-STALE",
			ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
			CreatedAt: time.Now().UTC().Add(-48 * time.Hour),
			UpdatedAt: staleUpdatedAt,
		},
		"SNYK-CONFIRMED": {
			IssueKey:  "SNYK-CONFIRMED",
			ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
			CreatedAt: time.Now().UTC().Add(-48 * time.Hour),
			UpdatedAt: staleUpdatedAt,
		},
	}
	if err := store.SaveIgnores(ctx, projectID, seeded); err != nil {
		t.Fatalf("seed SaveIgnores() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Only SNYK-CONFIRMED is reported by the API this run; SNYK-STALE is
		// not mentioned at all, so it must be carried over from the cache.
		body := map[string][]v1IgnoreEntry{
			"SNYK-CONFIRMED": {
				{Created: time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339), Expires: time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)},
			},
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Fatalf("encode mock response: %v", err)
		}
	}))
	defer server.Close()

	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c := &Client{
		httpClient: server.Client(),
		v1Base:     base,
		orgID:      "test-org",
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		cache:      store,
	}

	if _, err := c.fetchProjectIgnores(ctx, projectID); err != nil {
		t.Fatalf("fetchProjectIgnores() error = %v", err)
	}

	loaded, err := store.LoadIgnores(ctx, projectID)
	if err != nil {
		t.Fatalf("LoadIgnores() error = %v", err)
	}

	stale, ok := loaded["SNYK-STALE"]
	if !ok {
		t.Fatal("SNYK-STALE missing after round trip")
	}
	if !stale.UpdatedAt.Equal(staleUpdatedAt) {
		t.Fatalf("SNYK-STALE UpdatedAt = %v, want unchanged %v (not confirmed by API this run, so TTL clock must keep ticking)", stale.UpdatedAt, staleUpdatedAt)
	}

	confirmed, ok := loaded["SNYK-CONFIRMED"]
	if !ok {
		t.Fatal("SNYK-CONFIRMED missing after round trip")
	}
	if !confirmed.UpdatedAt.After(staleUpdatedAt) {
		t.Fatalf("SNYK-CONFIRMED UpdatedAt = %v, want refreshed after %v (confirmed by API this run)", confirmed.UpdatedAt, staleUpdatedAt)
	}
}

func TestMergeIgnoresUsesLatestCreatedFromAPI(t *testing.T) {
	api := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{{Created: "2026-06-15T00:00:00Z", Expires: "2026-06-14T23:00:00Z", DisregardIfFixable: true}},
	}
	cached := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{{Created: "2026-05-29T00:00:00Z", Expires: "2026-06-17T23:00:00Z"}},
	}

	merged := mergeIgnores(api, cached)
	entries, ok := merged["SNYK-1"]
	if !ok {
		t.Fatal("merged missing SNYK-1")
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	meta := maxExpiryIgnoreMeta(entries)
	if meta.ExpiresAt.Format(time.RFC3339) != "2026-06-17T23:00:00Z" {
		t.Fatalf("expires = %q, want 2026-06-17T23:00:00Z", meta.ExpiresAt.Format(time.RFC3339))
	}
	if meta.CreatedAt.Format(time.RFC3339) != "2026-06-15T00:00:00Z" {
		t.Fatalf("created = %q, want 2026-06-15T00:00:00Z (latest created from API)", meta.CreatedAt.Format(time.RFC3339))
	}
	if !meta.DisregardIfFixable {
		t.Fatalf("DisregardIfFixable = false, want true (from latest created API entry)")
	}
}

func TestMergeIgnoresKeepsCachedKeyWhenMissingFromAPI(t *testing.T) {
	api := v1ProjectIgnores{}
	cached := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{{Created: "2026-05-29T00:00:00Z", Expires: "2026-06-17T23:00:00Z"}},
	}

	merged := mergeIgnores(api, cached)
	entries, ok := merged["SNYK-1"]
	if !ok {
		t.Fatal("merged missing SNYK-1")
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Expires != "2026-06-17T23:00:00Z" {
		t.Fatalf("expires = %q, want 2026-06-17T23:00:00Z", entries[0].Expires)
	}
}

// TestMergeIgnoresUnionResurrectsStaleExpiry reproduces the core bug: the API
// now correctly reports the latest action as a permanent ignore (an entry
// with no Expires, created after the old snooze), but the cache still holds
// the earlier snooze's expiry. Summarizing each side independently and then
// taking max(ExpiresAt) would resurrect the stale expiry; unioning the raw
// entries and letting maxExpiryIgnoreMeta run once over the whole set must
// not.
func TestMergeIgnoresUnionResurrectsStaleExpiry(t *testing.T) {
	api := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{
			{Created: "2026-05-29T00:00:00Z", Expires: "2026-06-17T23:00:00Z"},
			{Created: "2026-07-01T00:00:00Z", Expires: ""}, // latest action: permanent ignore
		},
	}
	cached := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{
			{Created: "2026-05-29T00:00:00Z", Expires: "2026-06-17T23:00:00Z"},
		},
	}

	merged := mergeIgnores(api, cached)
	meta := maxExpiryIgnoreMeta(merged["SNYK-1"])
	if !meta.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt = %v, want zero (latest action is a permanent ignore)", meta.ExpiresAt)
	}
}

// TestMergeIgnoresRetryUnionResurrectsStaleExpiry reproduces the same bug via
// the retry path inside fetchProjectIgnoresWithRetry: attempt 1 sees only the
// old snooze, attempt 2 sees the snooze plus the newer permanent ignore. The
// accumulated apiIgnores must end up with a zero expiry.
func TestMergeIgnoresRetryUnionResurrectsStaleExpiry(t *testing.T) {
	attempt1 := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{
			{Created: "2026-05-29T00:00:00Z", Expires: "2026-06-17T23:00:00Z"},
		},
	}
	attempt2 := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{
			{Created: "2026-05-29T00:00:00Z", Expires: "2026-06-17T23:00:00Z"},
			{Created: "2026-07-01T00:00:00Z", Expires: ""},
		},
	}

	var apiIgnores v1ProjectIgnores
	apiIgnores = mergeIgnores(apiIgnores, attempt1)
	apiIgnores = mergeIgnores(apiIgnores, attempt2)

	meta := maxExpiryIgnoreMeta(apiIgnores["SNYK-1"])
	if !meta.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt = %v, want zero (latest attempt reported a permanent ignore)", meta.ExpiresAt)
	}
}

// TestMergeIgnoresPreferAPIOnCreatedTie reproduces a previously-poisoned
// cache: the cache holds an entry {Created: X, Expires: stale} (itself the
// product of an earlier conflated merge), while the live API now cleanly
// reports {Created: X, no expiry} for that same moment. On an exact Created
// tie with conflicting Expires, the API entry (first argument to
// mergeIgnores) must win the "latest entry" determination so the merged
// result is a clean permanent ignore and the poisoned cached entry cannot
// keep resurrecting itself.
func TestMergeIgnoresPreferAPIOnCreatedTie(t *testing.T) {
	api := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{
			{Created: "2026-07-01T00:00:00Z", Expires: ""},
		},
	}
	cached := v1ProjectIgnores{
		"SNYK-1": []v1IgnoreEntry{
			{Created: "2026-07-01T00:00:00Z", Expires: "2026-08-01T00:00:00Z"},
		},
	}

	merged := mergeIgnores(api, cached)
	meta := maxExpiryIgnoreMeta(merged["SNYK-1"])
	if !meta.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt = %v, want zero (API's permanent ignore wins the Created tie)", meta.ExpiresAt)
	}

	// The same cache-write-back path (v1IgnoresToCache) must also see the
	// clean, zeroed result rather than the stale conflated expiry.
	cacheOut := v1IgnoresToCache(merged, map[string]struct{}{"SNYK-1": {}}, nil)
	if got, ok := cacheOut["SNYK-1"]; ok && !got.ExpiresAt.IsZero() {
		t.Fatalf("cached ExpiresAt = %v, want zero or absent", got.ExpiresAt)
	}
}

func TestLocationKey(t *testing.T) {
	tests := []struct {
		name   string
		coords []coordinate
		want   string
	}{
		{
			"source file",
			[]coordinate{{
				Representations: []representation{{
					SourceLocation: sourceLocationRepresentation{File: "e2e/prerequisite_gate.py"},
				}},
			}},
			"e2e/prerequisite_gate.py",
		},
		{
			"dependency with version",
			[]coordinate{{
				Representations: []representation{{
					Dependency: dependencyRepresentation{PackageName: "lodash", PackageVersion: "4.17.21"},
				}},
			}},
			"lodash@4.17.21",
		},
		{
			"dependency without version",
			[]coordinate{{
				Representations: []representation{{
					Dependency: dependencyRepresentation{PackageName: "lodash"},
				}},
			}},
			"lodash",
		},
		{
			"source file takes precedence over dependency",
			[]coordinate{{
				Representations: []representation{{
					Dependency:     dependencyRepresentation{PackageName: "lodash", PackageVersion: "4.17.21"},
					SourceLocation: sourceLocationRepresentation{File: "src/index.py"},
				}},
			}},
			"src/index.py",
		},
		{
			"empty coordinates",
			[]coordinate{},
			"",
		},
		{
			"no representations",
			[]coordinate{{}},
			"",
		},
		{
			// Fix 5: multiple coordinates, file order A, C.
			"multiple coordinates order A",
			[]coordinate{
				{Representations: []representation{{SourceLocation: sourceLocationRepresentation{File: "z/file.py"}}}},
				{Representations: []representation{{SourceLocation: sourceLocationRepresentation{File: "a/file.py"}}}},
			},
			"a/file.py",
		},
		{
			// Fix 5: same two coordinates, order flipped — must produce the
			// identical key as "multiple coordinates order A" above, since
			// Snyk does not guarantee coordinate ordering is stable across
			// API calls.
			"multiple coordinates order B (reversed)",
			[]coordinate{
				{Representations: []representation{{SourceLocation: sourceLocationRepresentation{File: "a/file.py"}}}},
				{Representations: []representation{{SourceLocation: sourceLocationRepresentation{File: "z/file.py"}}}},
			},
			"a/file.py",
		},
		{
			// Fix 5: same nondeterminism risk for dependency coordinates.
			"multiple dependency coordinates, order-independent",
			[]coordinate{
				{Representations: []representation{{Dependency: dependencyRepresentation{PackageName: "zeta", PackageVersion: "1.0.0"}}}},
				{Representations: []representation{{Dependency: dependencyRepresentation{PackageName: "alpha", PackageVersion: "2.0.0"}}}},
			},
			"alpha@2.0.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := locationKey(tt.coords)
			if got != tt.want {
				t.Fatalf("locationKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLocationKeyDeterministicAcrossCoordinateOrder is a focused regression
// test for Fix 5: given the same set of coordinates in any order, locationKey
// must always return the same key. Snyk's issue coordinates/representations
// ordering is not guaranteed stable across API calls; picking "whichever
// comes first" made the fingerprint flip between runs, orphaning the old
// Linear ticket and creating a duplicate.
func TestLocationKeyDeterministicAcrossCoordinateOrder(t *testing.T) {
	forward := []coordinate{
		{Representations: []representation{{SourceLocation: sourceLocationRepresentation{File: "src/b.py"}}}},
		{Representations: []representation{{SourceLocation: sourceLocationRepresentation{File: "src/a.py"}}}},
		{Representations: []representation{{SourceLocation: sourceLocationRepresentation{File: "src/c.py"}}}},
	}
	reversed := []coordinate{forward[2], forward[1], forward[0]}

	gotForward := locationKey(forward)
	gotReversed := locationKey(reversed)
	if gotForward != gotReversed {
		t.Fatalf("locationKey() not order-independent: forward = %q, reversed = %q", gotForward, gotReversed)
	}
	if gotForward != "src/a.py" {
		t.Fatalf("locationKey() = %q, want lexicographically smallest %q", gotForward, "src/a.py")
	}
}

func TestLatestResolvedAt(t *testing.T) {
	latest, invalid := latestResolvedAt(nil)
	if !latest.IsZero() || invalid {
		t.Fatalf("no coordinates: got %v invalid=%v", latest, invalid)
	}

	latest, invalid = latestResolvedAt([]coordinate{
		{LastResolvedAt: "2026-03-01T00:00:00Z"},
		{LastResolvedAt: ""},
		{LastResolvedAt: "2026-05-01T12:00:00Z"},
	})
	if invalid || !latest.Equal(time.Date(2026, time.May, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %v invalid=%v, want the most recent resolution", latest, invalid)
	}

	_, invalid = latestResolvedAt([]coordinate{{LastResolvedAt: "2026-03-01T00:00:00Z"}, {LastResolvedAt: "yesterday"}})
	if !invalid {
		t.Fatalf("an unparsable last_resolved_at must be reported, not read as never resolved")
	}
}

func TestFetchProjectClusterWithRetry(t *testing.T) {
	previous := clusterLookupBackoff
	clusterLookupBackoff = time.Millisecond
	defer func() { clusterLookupBackoff = previous }()

	for _, tc := range []struct {
		name         string
		failures     int
		wantErr      bool
		wantRequests int
	}{
		{name: "recovers after a transient 500", failures: 1, wantRequests: 2},
		{name: "fails after every attempt", failures: 10, wantErr: true, wantRequests: clusterLookupAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests <= tc.failures {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = w.Write([]byte(`{"imageCluster":"prod"}`))
			}))
			defer server.Close()
			base, err := url.Parse(server.URL + "/")
			if err != nil {
				t.Fatalf("parse server URL: %v", err)
			}
			c := &Client{
				httpClient: server.Client(),
				v1Base:     base,
				orgID:      "test-org",
				logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			cluster, err := c.fetchProjectClusterWithRetry(context.Background(), "test-project", clusterLookupAttempts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && cluster != "prod" {
				t.Fatalf("cluster = %q, want prod", cluster)
			}
			if requests != tc.wantRequests {
				t.Fatalf("requests = %d, want %d", requests, tc.wantRequests)
			}
		})
	}
}
