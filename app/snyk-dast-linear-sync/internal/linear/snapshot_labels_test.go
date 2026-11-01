package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	gqlclient "git.sr.ht/~emersion/gqlclient"

	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
)

// snapshotLabelFixture builds a Linear issue node carrying count labels, with
// the managed label ("snyk-dast-automation") deliberately last, i.e. past
// Linear's default page size of 50 whenever count is 51 or more. That is the
// position a bare `labels { nodes }` read would silently drop.
func snapshotLabelFixture(t *testing.T, count int) string {
	t.Helper()
	labels := make([]string, 0, count)
	for i := 0; i < count-1; i++ {
		labels = append(labels, fmt.Sprintf(`{"id":"label-%d","name":"area-%02d"}`, i, i))
	}
	labels = append(labels, `{"id":"label-managed","name":"snyk-dast-automation"}`)
	return fmt.Sprintf(`{"id":"issue-1","identifier":"SNYK-100","title":"Snyk DAST: issue-1","state":{"id":"st-1","name":"Todo"},"labels":{"nodes":[%s],"pageInfo":{"hasNextPage":false}}}`,
		strings.Join(labels, ","))
}

func newSnapshotLabelClient(t *testing.T, body string, capturedQuery *string) *Client {
	t.Helper()
	return &Client{
		cfg:          config.LinearConfig{TeamID: "team-1"},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query string `json:"query"`
				}
				raw, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				*capturedQuery = payload.Query
				return jsonResponse(t, body), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}
}

// TestSnapshotQueriesRequestFullLabelPage pins the query shape of both
// snapshot reads: a bare `labels { nodes }` silently truncates at Linear's
// default page size of 50, so the snapshot must page labels at
// maxLiveLabelPage and ask for the pageInfo that says whether it truncated.
func TestSnapshotQueriesRequestFullLabelPage(t *testing.T) {
	body := `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"SNYK-100","state":{"id":"st-1","name":"Todo"},"labels":{"nodes":[],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`

	loads := map[string]func(*Client) error{
		"loadIssues": func(c *Client) error {
			_, err := c.loadIssues(context.Background())
			return err
		},
		"issueByIdentifier": func(c *Client) error {
			_, err := c.LoadIssueByIdentifier(context.Background(), "SNYK-100")
			return err
		},
	}
	for name, load := range loads {
		t.Run(name, func(t *testing.T) {
			var capturedQuery string
			client := newSnapshotLabelClient(t, body, &capturedQuery)
			if err := load(client); err != nil {
				t.Fatalf("load() error = %v", err)
			}
			if !strings.Contains(capturedQuery, fmt.Sprintf("labels(first: %d)", maxLiveLabelPage)) {
				t.Fatalf("snapshot query must request %d labels per issue, got:\n%s", maxLiveLabelPage, capturedQuery)
			}
			if !strings.Contains(capturedQuery, "pageInfo") {
				t.Fatalf("snapshot query must request the labels pageInfo, got:\n%s", capturedQuery)
			}
		})
	}
}

// TestSnapshotReturnsCompleteLabelSetPastDefaultPageSize serves an issue with
// 55 labels — the managed label last, past the default page size of 50 — and
// asserts both snapshot reads return the whole set. The hash feed is
// existingIssueHash in internal/sync, tested from there
// (TestExistingIssueHashSeesManagedLabelPastDefaultPageSize); this test proves
// the snapshot it hashes is complete.
func TestSnapshotReturnsCompleteLabelSetPastDefaultPageSize(t *testing.T) {
	const count = 55
	body := fmt.Sprintf(`{"data":{"issues":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		snapshotLabelFixture(t, count))

	t.Run("loadIssues", func(t *testing.T) {
		var capturedQuery string
		client := newSnapshotLabelClient(t, body, &capturedQuery)
		issues, err := client.loadIssues(context.Background())
		if err != nil {
			t.Fatalf("loadIssues() error = %v", err)
		}
		if len(issues) != 1 {
			t.Fatalf("loadIssues() returned %d issues, want 1", len(issues))
		}
		if got := len(issues[0].Labels); got != count {
			t.Fatalf("ExistingIssue.Labels has %d labels, want the complete set of %d", got, count)
		}
		if last := issues[0].Labels[count-1]; last.Name != "snyk-dast-automation" {
			t.Fatalf("last label = %q, want the managed label that sits past the default page size", last.Name)
		}
		if !issues[0].HasLabel("snyk-dast-automation") {
			t.Fatal("ExistingIssue.Labels is missing the managed label")
		}
	})

	t.Run("issueByIdentifier", func(t *testing.T) {
		var capturedQuery string
		client := newSnapshotLabelClient(t, body, &capturedQuery)
		issue, err := client.LoadIssueByIdentifier(context.Background(), "SNYK-100")
		if err != nil {
			t.Fatalf("LoadIssueByIdentifier() error = %v", err)
		}
		if got := len(issue.Labels); got != count {
			t.Fatalf("ExistingIssue.Labels has %d labels, want the complete set of %d", got, count)
		}
		if last := issue.Labels[count-1]; last.Name != "snyk-dast-automation" {
			t.Fatalf("last label = %q, want the managed label that sits past the default page size", last.Name)
		}
	})
}

// TestSnapshotRefusesTruncatedLabelPage pins the fail-closed path: if a
// snapshot label page can still truncate at maxLiveLabelPage, the read must
// fail that issue rather than return a partial set — an issue emitted with a
// partial set would both hide a removed managed label from the cache hash and
// misstate the labels an update must preserve.
func TestSnapshotRefusesTruncatedLabelPage(t *testing.T) {
	body := `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"SNYK-100","title":"Snyk DAST: issue-1","state":{"id":"st-1","name":"Todo"},"labels":{"nodes":[{"id":"label-0","name":"area-0"}],"pageInfo":{"hasNextPage":true}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`

	loads := map[string]func(*Client) error{
		"loadIssues": func(c *Client) error {
			_, err := c.loadIssues(context.Background())
			return err
		},
		"issueByIdentifier": func(c *Client) error {
			_, err := c.LoadIssueByIdentifier(context.Background(), "SNYK-100")
			return err
		},
	}
	for name, load := range loads {
		t.Run(name, func(t *testing.T) {
			var capturedQuery string
			client := newSnapshotLabelClient(t, body, &capturedQuery)
			if err := load(client); err == nil {
				t.Fatal("load() error = nil, want a refusal when the snapshot label page is truncated")
			} else if !strings.Contains(err.Error(), "SNYK-100") {
				t.Fatalf("load() error = %v, want it to name the issue", err)
			}
		})
	}
}
