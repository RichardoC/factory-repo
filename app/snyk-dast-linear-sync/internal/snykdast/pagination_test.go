package snykdast

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
)

// recordedRequest is one request the test server received.
type recordedRequest struct {
	Path  string
	Query url.Values
	Auth  string
}

// requestLog records requests as the server receives them.
type requestLog struct {
	mu  sync.Mutex
	all []recordedRequest
}

func (l *requestLog) add(r recordedRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, r)
}

func (l *requestLog) received() []recordedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]recordedRequest(nil), l.all...)
}

// newListingServer starts a server that records every request and answers via
// handler. It shuts down with the test.
func newListingServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *requestLog) {
	t.Helper()
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(recordedRequest{
			Path:  r.URL.Path,
			Query: r.URL.Query(),
			Auth:  r.Header.Get("Authorization"),
		})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

// newTestClient builds a client through snykdast.New against apiBase, so the
// real AdaptiveTransport + HeaderTransport stack is exercised.
func newTestClient(t *testing.T, apiBase, team string) *Client {
	t.Helper()
	cfg := config.Config{
		SnykDAST: config.SnykDASTConfig{
			APIKey:  "test-key",
			APIBase: apiBase,
			AppBase: "https://app.example.test",
			Team:    team,
		},
		Sync: config.SyncConfig{SnykDASTConcurrency: 4},
	}
	client, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return client
}

// findingsPage renders one page of a findings listing with n results whose
// numeric ids start at firstID.
func findingsPage(t *testing.T, count, pageTotal, page, firstID, n int) string {
	t.Helper()
	results := make([]string, 0, n)
	for id := firstID; id < firstID+n; id++ {
		results = append(results, fmt.Sprintf(
			`{"id":%d,"state":"notfixed","target":{"id":"target-1","name":"project-a"}}`, id))
	}
	return fmt.Sprintf(`{"count":%d,"page":%d,"page_total":%d,"length":100,"results":[%s]}`,
		count, page, pageTotal, strings.Join(results, ","))
}

// emptyPage renders an empty page of a listing.
func emptyPage(count, pageTotal, page int) string {
	return fmt.Sprintf(`{"count":%d,"page":%d,"page_total":%d,"length":100,"results":[]}`,
		count, pageTotal, page)
}

func TestListFindingsPagesUntilPageTotal(t *testing.T) {
	srv, log := newListingServer(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if r.URL.Query().Get("length") != "100" {
			t.Errorf("length param = %q, want 100", r.URL.Query().Get("length"))
		}
		switch page {
		case 1, 2:
			fmt.Fprint(w, findingsPage(t, 250, 3, page, (page-1)*100+1, 100))
		case 3:
			fmt.Fprint(w, findingsPage(t, 250, 3, 3, 201, 50))
		default:
			t.Errorf("unexpected page request %d", page)
			fmt.Fprint(w, emptyPage(250, 3, page))
		}
	})

	client := newTestClient(t, srv.URL, "")
	findings, err := client.listFindings(context.Background())
	if err != nil {
		t.Fatalf("listFindings() error: %v", err)
	}
	if len(findings) != 250 {
		t.Fatalf("listFindings() returned %d findings, want 250", len(findings))
	}

	var pages []string
	for _, req := range log.received() {
		pages = append(pages, req.Query.Get("page"))
	}
	if want := []string{"1", "2", "3"}; !reflect.DeepEqual(pages, want) {
		t.Fatalf("requested pages = %v, want %v", pages, want)
	}
}

func TestListFindingsFailsWhenShorterThanCount(t *testing.T) {
	srv, _ := newListingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/targets/", "/definitions/":
			fmt.Fprint(w, emptyPage(0, 1, 1))
		case "/findings/":
			// No page_total in the response: only this page is served even
			// though the server reports four findings.
			fmt.Fprint(w, `{"count":4,"page":1,"length":100,"results":[`+
				`{"id":1,"state":"notfixed","target":{"id":"target-1","name":"project-a"}},`+
				`{"id":2,"state":"notfixed","target":{"id":"target-1","name":"project-a"}}]}`)
		default:
			http.NotFound(w, r)
		}
	})

	client := newTestClient(t, srv.URL, "")
	snapshot, err := client.LoadSnapshot(context.Background())
	if err == nil {
		t.Fatal("LoadSnapshot() with a truncated findings listing returned no error")
	}
	if !strings.Contains(err.Error(), "2 of 4") {
		t.Fatalf("LoadSnapshot() error = %q, want it to mention 2 of 4", err)
	}
	if len(snapshot.Findings) != 0 {
		t.Fatalf("LoadSnapshot() returned %d findings with the error, want 0", len(snapshot.Findings))
	}
}

func TestListFindingsFailsOnEmptyPageBeforePageTotal(t *testing.T) {
	srv, _ := newListingServer(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		switch page {
		case 1:
			fmt.Fprint(w, findingsPage(t, 200, 2, 1, 1, 100))
		default:
			// page_total says 2, but page 2 comes back empty.
			fmt.Fprint(w, emptyPage(200, 2, page))
		}
	})

	client := newTestClient(t, srv.URL, "")
	_, err := client.listFindings(context.Background())
	if err == nil {
		t.Fatal("listFindings() with an empty page before page_total returned no error")
	}
	if !strings.Contains(err.Error(), "100 of 200") {
		t.Fatalf("listFindings() error = %q, want it to mention 100 of 200", err)
	}
}

func TestListTargetsFailsWhenShorterThanCount(t *testing.T) {
	srv, _ := newListingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/targets/" {
			http.NotFound(w, r)
			return
		}
		// No page_total: only this page is served though the server reports
		// four targets.
		fmt.Fprint(w, `{"count":4,"page":1,"length":100,"results":[`+
			`{"id":"target-1","type":"web","site":{"name":"project-a","url":"https://project-a.example.test/","host":"project-a.example.test"}},`+
			`{"id":"target-2","type":"web","site":{"name":"project-b","url":"https://project-b.example.test/","host":"project-b.example.test"}}]}`)
	})

	client := newTestClient(t, srv.URL, "")
	_, err := client.listTargets(context.Background())
	if err == nil {
		t.Fatal("listTargets() with a truncated targets listing returned no error")
	}
	if !strings.Contains(err.Error(), "2 of 4") {
		t.Fatalf("listTargets() error = %q, want it to mention 2 of 4", err)
	}
}

func TestListTargetsSendsTeamFilter(t *testing.T) {
	for _, tc := range []struct {
		name string
		team string
	}{
		{"team set", "team-1"},
		{"team empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, log := newListingServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/targets/", "/findings/":
					fmt.Fprint(w, emptyPage(0, 1, 1))
				default:
					http.NotFound(w, r)
				}
			})

			client := newTestClient(t, srv.URL, tc.team)
			ctx := context.Background()
			if _, err := client.listTargets(ctx); err != nil {
				t.Fatalf("listTargets() error: %v", err)
			}
			if _, err := client.listFindings(ctx); err != nil {
				t.Fatalf("listFindings() error: %v", err)
			}

			seen := map[string]bool{}
			for _, req := range log.received() {
				if req.Path != "/targets/" && req.Path != "/findings/" {
					continue
				}
				seen[req.Path] = true
				if got := req.Query.Get("team"); got != tc.team {
					t.Errorf("%s team param = %q, want %q", req.Path, got, tc.team)
				}
			}
			if !seen["/targets/"] || !seen["/findings/"] {
				t.Fatalf("expected requests to /targets/ and /findings/, got %v", seen)
			}
		})
	}
}

func TestListDefinitionsMapsPassiveFlag(t *testing.T) {
	srv, _ := newListingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/definitions/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"count":3,"page":1,"page_total":1,"length":100,"results":[`+
			`{"id":"def-1","name":"missing security headers","passive":true},`+
			`{"id":"def-2","name":"sql injection","passive":false},`+
			`{"id":"def-3","name":"no passive field"}]}`)
	})

	client := newTestClient(t, srv.URL, "")
	definitions, err := client.listDefinitions(context.Background())
	if err != nil {
		t.Fatalf("listDefinitions() error: %v", err)
	}

	want := map[string]string{"def-1": "passive", "def-2": "active"}
	if !reflect.DeepEqual(definitions, want) {
		t.Fatalf("listDefinitions() = %v, want %v (def-3 must be absent)", definitions, want)
	}
}

func TestLoadSnapshotFailsOnNon2xx(t *testing.T) {
	srv, _ := newListingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/targets/" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	})

	client := newTestClient(t, srv.URL, "")
	_, err := client.LoadSnapshot(context.Background())
	if err == nil {
		t.Fatal("LoadSnapshot() with a 403 from /targets/ returned no error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("LoadSnapshot() error = %q, want it to mention the 403 status", err)
	}
}

// TestLoadSnapshotFailsOnTruncatedListingThroughNew is the end-to-end check:
// the client is built through snykdast.New so the real AdaptiveTransport and
// HeaderTransport stack carries the requests, and a truncated /findings/
// listing surfaces as the truncation error from LoadSnapshot.
func TestLoadSnapshotFailsOnTruncatedListingThroughNew(t *testing.T) {
	srv, log := newListingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/targets/", "/definitions/":
			fmt.Fprint(w, emptyPage(0, 1, 1))
		case "/findings/":
			fmt.Fprint(w, `{"count":4,"page":1,"length":100,"results":[`+
				`{"id":1,"state":"notfixed","target":{"id":"target-1","name":"project-a"}},`+
				`{"id":2,"state":"notfixed","target":{"id":"target-1","name":"project-a"}}]}`)
		default:
			http.NotFound(w, r)
		}
	})

	client := newTestClient(t, srv.URL, "")
	_, err := client.LoadSnapshot(context.Background())
	if err == nil {
		t.Fatal("LoadSnapshot() with a truncated findings listing returned no error")
	}
	if !strings.Contains(err.Error(), "2 of 4") {
		t.Fatalf("LoadSnapshot() error = %q, want it to mention 2 of 4", err)
	}

	received := log.received()
	if len(received) == 0 {
		t.Fatal("the server received no requests")
	}
	if got := received[0].Auth; got != "JWT test-key" {
		t.Fatalf("Authorization header on the first request = %q, want %q", got, "JWT test-key")
	}
}
