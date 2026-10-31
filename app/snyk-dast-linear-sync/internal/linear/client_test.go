package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gqlclient "git.sr.ht/~emersion/gqlclient"

	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
	linSync "github.com/tesslio/snyk-dast-linear-sync/internal/sync"
)

func TestDesiredLabelIDsReplacesPreviousManagedLabel(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed: "snyk-dast-automation",
			},
		},
		managedLabelIDs: map[string]string{
			"snyk-dast-automation": "label-new",
			"snyk-dast-api":        "label-api",
		},
	}

	existing := model.ExistingIssue{
		ManagedLabels: []string{"old-managed"},
		Labels: []model.IssueLabel{
			{ID: "label-unrelated", Name: "customer-visible"},
			{ID: "label-old", Name: "old-managed"},
		},
	}
	desired := model.DesiredIssue{
		ManagedLabels: []string{"snyk-dast-automation", "snyk-dast-api"},
	}

	labelIDs, err := client.desiredLabelIDs(existing, desired, nil)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if len(labelIDs) != 3 {
		t.Fatalf("labelIDs len = %d, want 3", len(labelIDs))
	}
	if !containsString(labelIDs, "label-unrelated") {
		t.Fatalf("labelIDs = %#v, want unrelated label preserved", labelIDs)
	}
	if !containsString(labelIDs, "label-new") {
		t.Fatalf("labelIDs = %#v, want new managed label present", labelIDs)
	}
	if !containsString(labelIDs, "label-api") {
		t.Fatalf("labelIDs = %#v, want target-type label present", labelIDs)
	}
	if containsString(labelIDs, "label-old") {
		t.Fatalf("labelIDs = %#v, want old managed label removed", labelIDs)
	}
}

func TestDesiredLabelIDsRemovesManagedLabelWhenDisabled(t *testing.T) {
	client := &Client{}
	existing := model.ExistingIssue{
		ManagedLabels: []string{"snyk-dast-automation", "snyk-dast-api"},
		Labels: []model.IssueLabel{
			{ID: "label-unrelated", Name: "customer-visible"},
			{ID: "label-managed", Name: "snyk-dast-automation"},
			{ID: "label-type", Name: "snyk-dast-api"},
		},
	}

	labelIDs, err := client.desiredLabelIDs(existing, model.DesiredIssue{}, nil)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if len(labelIDs) != 1 || labelIDs[0] != "label-unrelated" {
		t.Fatalf("labelIDs = %#v, want only unrelated label", labelIDs)
	}
}

// TestIssueUpdateInputSerializesEmptyLabelIdsAsArray guards against a regression
// where an empty LabelIds slice was omitted from the update mutation via
// `omitempty`. When an issue carried only managed labels and they are all
// being removed (no unrelated labels to preserve), desiredLabelIDs returns a
// non-nil empty slice. With omitempty that slice was dropped from the payload,
// so Linear never received a labelIds field and left the stale managed labels
// on the issue. The JSON must serialize as "labelIds":[] so Linear clears them.
func TestIssueUpdateInputSerializesEmptyLabelIdsAsArray(t *testing.T) {
	input := issueUpdateInput{
		LabelIds: make([]string, 0, 4), // non-nil empty slice, as desiredLabelIDs returns
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !strings.Contains(string(raw), `"labelIds":[]`) {
		t.Fatalf("issueUpdateInput JSON must include \"labelIds\":[] for an empty slice so Linear clears labels, got: %s", raw)
	}
}

// TestUpdateIssuesClearsManagedLabelsWhenNoneDesired verifies the end-to-end
// behavior: an issue that had only managed labels, with label management now
// disabled, must send labelIds:[] in the update mutation so Linear removes them.
func TestUpdateIssuesClearsManagedLabelsWhenNoneDesired(t *testing.T) {
	var capturedInput map[string]any
	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			States: config.StateConfig{
				Todo: "Todo",
			},
		},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				body, _ := io.ReadAll(req.Body)
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				if strings.Contains(payload.Query, "mutation issueUpdateBatch") {
					for key, val := range payload.Variables {
						if strings.HasPrefix(key, "input") {
							capturedInput, _ = val.(map[string]any)
						}
					}
					return jsonResponse(t, `{"data":{"issueUpdate0":{"success":true}}}`), nil
				}
				t.Fatalf("unexpected GraphQL query: %s", payload.Query)
				return nil, nil
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam: "team-1",
		statesByName: map[string]string{"todo": "state-1"},
		statesByType: map[string]string{"unstarted": "state-1"},
	}

	err := client.UpdateIssues(t.Context(), []model.IssueUpdate{{
		Existing: model.ExistingIssue{
			ID:            "issue-1",
			Identifier:    "SNYK-1",
			ManagedLabels: []string{"snyk-dast-automation"},
			Labels:        []model.IssueLabel{{ID: "label-1", Name: "snyk-dast-automation"}},
		},
		Desired: model.DesiredIssue{
			Fingerprint:   "snyk-dast:target-1:finding-1",
			Title:         "Snyk DAST: title",
			Description:   "body",
			State:         model.StateTodo,
			ManagedLabels: nil, // label management off / none desired
			Priority:      2,
		},
	}})
	if err != nil {
		t.Fatalf("UpdateIssues() error = %v", err)
	}
	if capturedInput == nil {
		t.Fatal("no update input captured")
	}
	rawLabelIds, has := capturedInput["labelIds"]
	if !has {
		t.Fatalf("update mutation must include labelIds to clear managed labels, got: %#v", capturedInput)
	}
	arr, ok := rawLabelIds.([]any)
	if !ok || len(arr) != 0 {
		t.Fatalf("labelIds must be an empty array to clear labels, got: %#v", rawLabelIds)
	}
}

func TestExtractFingerprintPrefersMetadataBlock(t *testing.T) {
	description := "## Example\n\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\nmanaged_labels: snyk-dast-automation,snyk-dast-api\n-->"

	got := extractFingerprint(description)

	if got != "snyk-dast:target-a:finding-1" {
		t.Fatalf("extractFingerprint() = %q, want %q", got, "snyk-dast:target-a:finding-1")
	}
}

func TestExtractManagedLabelsSupportsLegacyAndNewMetadata(t *testing.T) {
	if got := extractManagedLabels("<!-- snyk-dast-linear-sync\nmanaged_label: snyk-dast-automation\n-->"); !slices.Equal(got, []string{"snyk-dast-automation"}) {
		t.Fatalf("extractManagedLabels(legacy) = %#v", got)
	}
	if got := extractManagedLabels("<!-- snyk-dast-linear-sync\nmanaged_labels: snyk-dast-api,snyk-dast-automation\n-->"); !slices.Equal(got, []string{"snyk-dast-api", "snyk-dast-automation"}) {
		t.Fatalf("extractManagedLabels(new) = %#v", got)
	}
}

// TestExtractFingerprintIgnoresLinesOutsideMetadataBlock verifies that a
// "Fingerprint:" line appearing in user-written body text is not extracted as
// the managed fingerprint. Only the fingerprint inside the line-anchored
// metadata block counts.
func TestExtractFingerprintIgnoresLinesOutsideMetadataBlock(t *testing.T) {
	description := "Notes\nFingerprint: fake-from-user-text\n\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\nmanaged_labels: snyk-dast-automation\n-->"

	if got := extractFingerprint(description); got != "snyk-dast:target-a:finding-1" {
		t.Fatalf("extractFingerprint() = %q, want the metadata-block fingerprint", got)
	}
}

// TestExtractFingerprintIgnoresInlineMarker verifies that an inline marker
// (mid-sentence) is not treated as a metadata block, so no fingerprint is
// extracted from a description that has no real line-anchored block.
func TestExtractFingerprintIgnoresInlineMarker(t *testing.T) {
	description := "See <!-- snyk-dast-linear-sync notes --> for context\nFingerprint: not-a-real-block"

	if got := extractFingerprint(description); got != "" {
		t.Fatalf("extractFingerprint() = %q, want empty (no line-anchored block)", got)
	}
	if got := extractManagedLabels(description); got != nil {
		t.Fatalf("extractManagedLabels() = %#v, want nil (no line-anchored block)", got)
	}
}

func TestActorSubscriberIDsForCreateEnabledEncodesEmptyList(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{
			UnsubscribeActor: true,
		},
	}

	input := issueCreateInput{
		SubscriberIds: client.actorSubscriberIDsForCreate(),
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(raw) != `{"subscriberIds":[],"teamId":""}` {
		t.Fatalf("json.Marshal() = %s, want subscriberIds empty list", raw)
	}
}

// TestIssueUpdateInputNeverContainsSubscriberIds guards against regressions where
// subscriberIds is added back to issueUpdateInput. Linear's IssueUpdateInput GraphQL
// type does not have a subscriberIds field; sending it causes an Argument Validation Error.
func TestIssueUpdateInputNeverContainsSubscriberIds(t *testing.T) {
	input := issueUpdateInput{
		Title:       new("title"),
		Description: new("body"),
		StateId:     new("state-1"),
		LabelIds:    []string{"label-1"},
		DueDate:     new("2026-04-07"),
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(raw), "subscriberIds") || strings.Contains(string(raw), "subscriberId") {
		t.Fatalf("issueUpdateInput JSON must not contain subscriberIds, got: %s", raw)
	}
}

//go:fix inline
func strPtr(s string) *string { return new(s) }

// TestUpdateIssuesDoesNotSendSubscriberIdsInPayload verifies that UpdateIssues never
// includes subscriberIds in the GraphQL mutation variables, even when UnsubscribeActor is true.
func TestUpdateIssuesDoesNotSendSubscriberIdsInPayload(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{
			TeamID:           "team-1",
			UnsubscribeActor: true,
			States: config.StateConfig{
				Todo: "Todo",
			},
		},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				if strings.Contains(payload.Query, "mutation issueUpdateBatch") {
					for key, val := range payload.Variables {
						if !strings.HasPrefix(key, "input") {
							continue
						}
						input, ok := val.(map[string]any)
						if !ok {
							continue
						}
						if _, has := input["subscriberIds"]; has {
							t.Fatalf("issueUpdate mutation must not include subscriberIds in %s, got: %#v", key, input)
						}
					}
					return jsonResponse(t, `{"data":{"issueUpdate0":{"success":true}}}`), nil
				}
				t.Fatalf("unexpected GraphQL query: %s", payload.Query)
				return nil, nil
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam: "team-1",
		statesByName: map[string]string{"todo": "state-1"},
		statesByType: map[string]string{"unstarted": "state-1"},
		managedLabelIDs: map[string]string{
			"snyk-dast-automation": "label-1",
		},
	}

	err := client.UpdateIssues(t.Context(), []model.IssueUpdate{{
		Existing: model.ExistingIssue{
			ID:            "issue-1",
			Identifier:    "SNYK-1",
			ManagedLabels: []string{"snyk-dast-automation"},
			Labels:        []model.IssueLabel{{ID: "label-1", Name: "snyk-dast-automation"}},
		},
		Desired: model.DesiredIssue{
			Fingerprint:   "snyk-dast:target-1:finding-1",
			Title:         "Snyk DAST: updated title",
			Description:   "updated body",
			State:         model.StateTodo,
			ManagedLabels: []string{"snyk-dast-automation"},
			Priority:      2,
		},
	}})
	if err != nil {
		t.Fatalf("UpdateIssues() error = %v", err)
	}
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func jsonResponse(t *testing.T, body string) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(bytes.NewBufferString(body)),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestUpdateIssuesOmitsStateIdWhenPreserveStateTrue verifies that UpdateIssues
// does not include stateId in the GraphQL mutation payload when
// Desired.PreserveState is true, preventing the sync tool from fighting
// manual state moves (e.g. human triage from Triage → Todo).
func TestUpdateIssuesOmitsStateIdWhenPreserveStateTrue(t *testing.T) {
	var requests []struct {
		Query     string
		Variables map[string]any
	}

	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			States: config.StateConfig{
				Todo: "Todo",
			},
		},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				requests = append(requests, struct {
					Query     string
					Variables map[string]any
				}{
					Query:     payload.Query,
					Variables: payload.Variables,
				})

				if strings.Contains(payload.Query, "mutation issueUpdateBatch") {
					return jsonResponse(t, `{"data":{"issueUpdate0":{"success":true}}}`), nil
				}
				t.Fatalf("unexpected GraphQL query: %s", payload.Query)
				return nil, nil
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam: "team-1",
		statesByName: map[string]string{"todo": "state-todo"},
		statesByType: map[string]string{"unstarted": "state-todo"},
		managedLabelIDs: map[string]string{
			"snyk-dast-automation": "label-1",
		},
	}

	err := client.UpdateIssues(t.Context(), []model.IssueUpdate{{
		Existing: model.ExistingIssue{
			ID:            "issue-1",
			Identifier:    "SEC-1",
			StateID:       "state-todo",
			StateName:     "Todo",
			ManagedLabels: []string{"snyk-dast-automation"},
			Labels:        []model.IssueLabel{{ID: "label-1", Name: "snyk-dast-automation"}},
		},
		Desired: model.DesiredIssue{
			Fingerprint:   "snyk-dast:target-1:finding-1",
			Title:         "updated title",
			Description:   "updated body",
			State:         model.StateTodo,
			PreserveState: true,
			ManagedLabels: []string{"snyk-dast-automation"},
			Priority:      2,
		},
	}})
	if err != nil {
		t.Fatalf("UpdateIssues() error = %v", err)
	}

	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}

	input0 := requests[0].Variables["input0"].(map[string]any)
	if _, has := input0["stateId"]; has {
		t.Fatalf("issueUpdate mutation must omit stateId when PreserveState=true, got: %#v", input0)
	}
	if input0["title"] != "updated title" {
		t.Fatalf("title = %v, want 'updated title'", input0["title"])
	}
}

func TestBuildChangeCommentGeneratesSummary(t *testing.T) {
	update := model.IssueUpdate{
		Existing: model.ExistingIssue{
			ID:            "issue-1",
			Identifier:    "SEC-1",
			Title:         "old title",
			Description:   "old body",
			DueDate:       "2026-04-01",
			StateName:     "Todo",
			Priority:      3,
			ManagedLabels: []string{"snyk-dast-automation"},
			Labels:        []model.IssueLabel{{ID: "l1", Name: "snyk-dast-automation"}},
		},
		Desired: model.DesiredIssue{
			Title:         "new title",
			Description:   "new body",
			DueDate:       "2026-05-01",
			State:         model.StateBacklog,
			StateReason:   "Snyk DAST reports this finding as accepted with a time-limited acceptance",
			DueDateReason: "high severity SLA: 30 days from finding creation",
			Priority:      1,
			ManagedLabels: []string{"snyk-dast-automation", "snyk-dast-api"},
			LabelReasons:  map[string]string{"snyk-dast-api": "Snyk DAST target type is api"},
		},
		Diff: &model.IssueDiff{
			TitleChanged:       true,
			TitleFrom:          "old title",
			TitleTo:            "new title",
			DescriptionChanged: true,
			DueDateChanged:     true,
			DueDateFrom:        "2026-04-01",
			DueDateTo:          "2026-05-01",
			StateChanged:       true,
			StateFrom:          "Todo",
			StateTo:            "backlog",
			PriorityChanged:    true,
			PriorityFrom:       3,
			PriorityTo:         1,
			LabelsAdded:        []string{"snyk-dast-api"},
		},
	}

	comment := buildChangeComment(update)

	if comment == "" {
		t.Fatal("expected non-empty comment")
	}
	if !strings.Contains(comment, "**snyk-dast-linear-sync**") {
		t.Fatalf("comment missing header: %s", comment)
	}
	if !strings.Contains(comment, "Moved to **backlog** — Snyk DAST reports this finding as accepted with a time-limited acceptance") {
		t.Fatalf("comment missing state change with reason: %s", comment)
	}
	if !strings.Contains(comment, "Due date set to **2026-05-01** — high severity SLA: 30 days from finding creation") {
		t.Fatalf("comment missing due date with reason: %s", comment)
	}
	if !strings.Contains(comment, "Description updated — Snyk DAST finding data changed") {
		t.Fatalf("comment missing description reason: %s", comment)
	}
	if !strings.Contains(comment, "Title updated — Snyk DAST finding data changed") {
		t.Fatalf("comment missing title reason: %s", comment)
	}
	if !strings.Contains(comment, "Priority set to **Urgent** — Snyk DAST severity changed") {
		t.Fatalf("comment missing priority with reason: %s", comment)
	}
	if !strings.Contains(comment, "Added **snyk-dast-api** — Snyk DAST target type is api") {
		t.Fatalf("comment missing label with reason: %s", comment)
	}
}

func TestBuildChangeCommentReturnsEmptyWhenNoChanges(t *testing.T) {
	update := model.IssueUpdate{
		Existing: model.ExistingIssue{
			Title:       "title",
			Description: "desc",
			DueDate:     "2026-04-01",
			StateName:   "Todo",
			Priority:    2,
		},
		Desired: model.DesiredIssue{
			Title:       "title",
			Description: "desc",
			DueDate:     "2026-04-01",
			State:       model.StateTodo,
			Priority:    2,
		},
		Diff: &model.IssueDiff{},
	}

	comment := buildChangeComment(update)

	if comment != "" {
		t.Fatalf("expected empty comment, got: %s", comment)
	}
}

func TestBuildChangeCommentReturnsEmptyWhenDiffIsNil(t *testing.T) {
	update := model.IssueUpdate{
		Existing: model.ExistingIssue{
			Title:       "title",
			Description: "desc",
			DueDate:     "2026-04-01",
			StateName:   "Todo",
			Priority:    2,
		},
		Desired: model.DesiredIssue{
			Title:       "new title",
			Description: "new desc",
			DueDate:     "2026-05-01",
			State:       model.StateBacklog,
			Priority:    1,
		},
		Diff: nil,
	}

	comment := buildChangeComment(update)

	if comment != "" {
		t.Fatalf("expected empty comment when diff is nil, got: %s", comment)
	}
}

// TestLoadIssuesRequestsArchivedIssues guards the fix for closed managed
// tickets disappearing from the snapshot. Linear excludes auto-archived issues
// from the default issues query, and Snyk DAST keeps fixed/accepted findings in
// its API indefinitely, so without includeArchived the sync stops seeing its own
// closed tickets and mints a fresh duplicate for each of them on every run.
//
// It also pins the AutoArchivedAt.Null boolean. Per Linear's
// NullableDateComparator, "null: true" matches records where the field IS null
// (i.e. NOT archived) while "null: false" matches non-null values (archived).
// Inverting it would make the live-issue clauses match only archived tickets.
func TestLoadIssuesRequestsArchivedIssues(t *testing.T) {
	var capturedQuery string
	var capturedFilter map[string]any

	client := &Client{
		cfg: config.LinearConfig{
			TeamID:              "team-1",
			ArchiveLookbackDays: 21,
		},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				capturedQuery = payload.Query
				if filter, ok := payload.Variables["filter"].(map[string]any); ok {
					capturedFilter = filter
				}
				return jsonResponse(t, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	if _, err := client.loadIssues(context.Background()); err != nil {
		t.Fatalf("loadIssues() error = %v", err)
	}

	if !strings.Contains(capturedQuery, "includeArchived: true") {
		t.Fatalf("issues query must pass includeArchived: true, got:\n%s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "archivedAt") {
		t.Fatalf("issues query must select archivedAt, got:\n%s", capturedQuery)
	}

	archiveArm := archiveWindowArm(t, capturedFilter)
	nullTrue, gte := 0, 0
	for _, raw := range archiveArm {
		clause, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("archive OR clause is not an object: %#v", raw)
		}
		archived, ok := clause["autoArchivedAt"].(map[string]any)
		if !ok {
			t.Fatalf("archive OR clause missing autoArchivedAt: %#v", clause)
		}
		if val, has := archived["null"]; has {
			isNull, ok := val.(bool)
			if !ok || !isNull {
				t.Fatalf("autoArchivedAt.null = %#v, want true (matches NOT-archived issues)", val)
			}
			nullTrue++
			continue
		}
		if val, has := archived["gte"]; has {
			cutoff, ok := val.(string)
			if !ok || cutoff == "" {
				t.Fatalf("autoArchivedAt.gte = %#v, want a non-empty cutoff timestamp", val)
			}
			gte++
			continue
		}
		t.Fatalf("autoArchivedAt has neither null nor gte: %#v", archived)
	}
	if nullTrue != 1 {
		t.Fatalf("clauses with autoArchivedAt.null=true = %d, want 1", nullTrue)
	}
	if gte != 1 {
		t.Fatalf("clauses with autoArchivedAt.gte = %d, want 1", gte)
	}
}

// andArms returns the filter's top-level AND arms. The filter is deliberately
// shaped as team AND (archive window) AND (identity) so that neither group can be
// read as a sibling of the other -- mixing an `or` with sibling fields leaves the
// grouping to Linear's interpretation, and this query is too expensive to get
// wrong.
func andArms(t *testing.T, filter map[string]any) []any {
	t.Helper()
	arms, ok := filter["and"].([]any)
	if !ok {
		t.Fatalf("filter must carry an AND arm list, got: %#v", filter)
	}
	// The snapshot filter has 2 (archive window, identity); the
	// reconciliation filter has 1 (identity), so both are accepted here and
	// each test asserts on the arm it cares about.
	if len(arms) < 1 || len(arms) > 2 {
		t.Fatalf("AND arms = %d, want 1 (identity) or 2 (archive window, identity)", len(arms))
	}
	return arms
}

// armOr extracts the OR clause list from one AND arm.
func armOr(t *testing.T, arm any) []any {
	t.Helper()
	obj, ok := arm.(map[string]any)
	if !ok {
		t.Fatalf("AND arm is not an object: %#v", arm)
	}
	clauses, ok := obj["or"].([]any)
	if !ok {
		t.Fatalf("AND arm must carry an OR clause list, got: %#v", obj)
	}
	return clauses
}

// archiveWindowArm returns the OR clauses of whichever AND arm constrains
// autoArchivedAt, so the tests do not depend on arm ordering.
func archiveWindowArm(t *testing.T, filter map[string]any) []any {
	t.Helper()
	for _, arm := range andArms(t, filter) {
		clauses := armOr(t, arm)
		if len(clauses) == 0 {
			continue
		}
		if first, ok := clauses[0].(map[string]any); ok {
			if _, isArchive := first["autoArchivedAt"]; isArchive {
				return clauses
			}
		}
	}
	t.Fatalf("no AND arm constrains autoArchivedAt: %#v", filter)
	return nil
}

// identityArm returns the OR clauses of whichever AND arm selects this sync's own
// tickets.
func identityArm(t *testing.T, filter map[string]any) []any {
	t.Helper()
	for _, arm := range andArms(t, filter) {
		clauses := armOr(t, arm)
		if len(clauses) == 0 {
			continue
		}
		if first, ok := clauses[0].(map[string]any); ok {
			if _, isArchive := first["autoArchivedAt"]; !isArchive {
				return clauses
			}
		}
	}
	t.Fatalf("no AND arm selects managed issues: %#v", filter)
	return nil
}

// loadIssuesFilter runs loadIssues against a stub that captures the filter
// variable, so a test can assert the query's shape.
func loadIssuesFilter(t *testing.T, managedLabel string) map[string]any {
	t.Helper()
	var captured map[string]any

	client := &Client{
		cfg: config.LinearConfig{
			TeamID:              "team-1",
			ArchiveLookbackDays: 21,
			Labels:              config.LabelConfig{Managed: managedLabel},
		},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Variables map[string]any `json:"variables"`
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				if filter, ok := payload.Variables["filter"].(map[string]any); ok {
					captured = filter
				}
				return jsonResponse(t, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	if _, err := client.loadIssues(context.Background()); err != nil {
		t.Fatalf("loadIssues() error = %v", err)
	}
	if captured == nil {
		t.Fatal("loadIssues() did not send a filter variable")
	}
	return captured
}

// TestLoadIssuesScopesSnapshotToManagedLabel is the regression guard for the
// snapshot blowing up on a shared Linear team. A run against a team holding
// another sync's tickets loaded 59,866 issues for 7 findings, because the
// identity arm filtered on `description: { contains: <metadata header> }` and
// that predicate did not bound the result set. The identity arm must instead use
// exact predicates: the managed title prefix and the managed label name.
func TestLoadIssuesScopesSnapshotToManagedLabel(t *testing.T) {
	filter := loadIssuesFilter(t, "snyk-dast-automation")

	raw, err := json.Marshal(filter)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(raw), "description") {
		t.Fatalf("filter must not constrain description; it does not bound the result set: %s", raw)
	}

	clauses := identityArm(t, filter)
	if len(clauses) != 2 {
		t.Fatalf("identity clauses = %d, want 2 (title prefix, managed label)", len(clauses))
	}

	var sawTitle, sawLabel bool
	for _, rawClause := range clauses {
		clause, ok := rawClause.(map[string]any)
		if !ok {
			t.Fatalf("identity clause is not an object: %#v", rawClause)
		}
		if title, ok := clause["title"].(map[string]any); ok {
			if got := title["startsWith"]; got != titlePrefix {
				t.Fatalf("title.startsWith = %#v, want %q", got, titlePrefix)
			}
			sawTitle = true
			continue
		}
		if labels, ok := clause["labels"].(map[string]any); ok {
			some, ok := labels["some"].(map[string]any)
			if !ok {
				t.Fatalf("labels filter must use `some`, got: %#v", labels)
			}
			name, ok := some["name"].(map[string]any)
			if !ok {
				t.Fatalf("labels.some must constrain name, got: %#v", some)
			}
			if got := name["eqIgnoreCase"]; got != "snyk-dast-automation" {
				t.Fatalf("labels.some.name.eqIgnoreCase = %#v, want the managed label", got)
			}
			sawLabel = true
			continue
		}
		t.Fatalf("identity clause is neither a title nor a labels predicate: %#v", clause)
	}
	if !sawTitle {
		t.Fatal("identity arm lost the title-prefix clause; a retitled ticket is the only thing the label clause misses")
	}
	if !sawLabel {
		t.Fatal("identity arm is missing the managed-label clause")
	}
}

// TestLoadIssuesOmitsLabelClauseWhenLabelManagementDisabled keeps
// LINEAR_MANAGED_LABEL=off working. An arm matching a label that is never applied
// would narrow the snapshot, hiding managed tickets and minting duplicates.
func TestLoadIssuesOmitsLabelClauseWhenLabelManagementDisabled(t *testing.T) {
	filter := loadIssuesFilter(t, "")

	clauses := identityArm(t, filter)
	if len(clauses) != 1 {
		t.Fatalf("identity clauses = %d, want 1 (title prefix only)", len(clauses))
	}
	clause, ok := clauses[0].(map[string]any)
	if !ok {
		t.Fatalf("identity clause is not an object: %#v", clauses[0])
	}
	if _, hasLabels := clause["labels"]; hasLabels {
		t.Fatalf("identity clause must not constrain labels with label management off: %#v", clause)
	}
	if _, hasTitle := clause["title"]; !hasTitle {
		t.Fatalf("identity clause must keep the title prefix: %#v", clause)
	}
}

// TestCreateIssuesReportsOnlyFailedIndices pins the partial-failure contract:
// a per-alias failure must be reported as an index, not as a batch-wide error,
// so the caller retries just that entry instead of duplicating the rest.
func TestCreateIssuesReportsOnlyFailedIndices(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{
			TeamID:           "team-1",
			UnsubscribeActor: false,
			States:           config.StateConfig{Todo: "Todo"},
		},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if !strings.Contains(string(body), "mutation issueCreateBatch") {
					t.Fatalf("unexpected request body: %s", body)
				}
				// Entry 1 fails; entries 0 and 2 are created.
				return jsonResponse(t, `{"data":{
					"issueCreate0":{"success":true,"issue":{"id":"i0","identifier":"SEC-1"}},
					"issueCreate1":{"success":false,"issue":{"id":"","identifier":""}},
					"issueCreate2":{"success":true,"issue":{"id":"i2","identifier":"SEC-3"}}
				}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{"todo": "state-todo"},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	desired := []model.DesiredIssue{
		{Fingerprint: "f0", Title: "a", State: model.StateTodo},
		{Fingerprint: "f1", Title: "b", State: model.StateTodo},
		{Fingerprint: "f2", Title: "c", State: model.StateTodo},
	}
	failed, err := client.CreateIssues(context.Background(), desired)
	if err != nil {
		t.Fatalf("CreateIssues() error = %v, want nil: a per-alias failure must not fail the batch", err)
	}
	if !slices.Equal(failed, []int{1}) {
		t.Fatalf("failed indices = %#v, want [1]", failed)
	}
}

// TestCreateIssuesKeepsPartialDataOnGraphQLError is the regression test for the
// duplicate path CodeRabbit caught. gqlclient decodes the response body into the
// out param BEFORE returning joinErrors(...), so a mutation that errored for one
// alias still leaves the successful aliases decoded. Reporting a batch error and
// discarding them makes the caller recreate everything that already succeeded.
func TestCreateIssuesKeepsPartialDataOnGraphQLError(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{
			TeamID:           "team-1",
			UnsubscribeActor: false,
			States:           config.StateConfig{Todo: "Todo"},
		},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if _, err := io.ReadAll(req.Body); err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				// Entries 0 and 2 created; entry 1 failed with a GraphQL error.
				// Linear returns both data and errors in this situation.
				return jsonResponse(t, `{
					"data":{
						"issueCreate0":{"success":true,"issue":{"id":"i0","identifier":"SEC-1"}},
						"issueCreate2":{"success":true,"issue":{"id":"i2","identifier":"SEC-3"}}
					},
					"errors":[{"message":"Entity not found: WorkflowState"}]
				}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{"todo": "state-todo"},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	desired := []model.DesiredIssue{
		{Fingerprint: "f0", Title: "a", State: model.StateTodo},
		{Fingerprint: "f1", Title: "b", State: model.StateTodo},
		{Fingerprint: "f2", Title: "c", State: model.StateTodo},
	}
	failed, err := client.CreateIssues(context.Background(), desired)
	if err != nil {
		t.Fatalf("CreateIssues() error = %v, want nil: partial data must not be discarded, "+
			"otherwise the caller recreates the two issues that succeeded", err)
	}
	if !slices.Equal(failed, []int{1}) {
		t.Fatalf("failed indices = %#v, want [1]", failed)
	}
}

// TestCreateIssuesReturnsErrorWhenNoAliasesDecoded confirms the other side: with
// no per-alias outcome at all, the error must surface so the caller retries
// everything individually.
func TestCreateIssuesReturnsErrorWhenNoAliasesDecoded(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{
			TeamID:           "team-1",
			UnsubscribeActor: false,
			States:           config.StateConfig{Todo: "Todo"},
		},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if _, err := io.ReadAll(req.Body); err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				return jsonResponse(t, `{"errors":[{"message":"Query validation failed"}]}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{"todo": "state-todo"},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	desired := []model.DesiredIssue{{Fingerprint: "f0", Title: "a", State: model.StateTodo}}
	if _, err := client.CreateIssues(context.Background(), desired); err == nil {
		t.Fatal("CreateIssues() error = nil, want an error when no alias outcome is known")
	}
}

// TestPostCommentsKeepsPartialDataOnGraphQLError mirrors the CreateIssues
// partial-data test for comments: aliases that posted must not be retried, or
// the issue ends up with the same change comment twice.
func TestPostCommentsKeepsPartialDataOnGraphQLError(t *testing.T) {
	client := &Client{
		cfg:          config.LinearConfig{TeamID: "team-1"},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if _, err := io.ReadAll(req.Body); err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				// Comments 0 and 2 posted; comment 1 errored.
				return jsonResponse(t, `{
					"data":{
						"commentCreate0":{"success":true,"comment":{"id":"c0"}},
						"commentCreate2":{"success":true,"comment":{"id":"c2"}}
					},
					"errors":[{"message":"Entity not found: Issue"}]
				}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	// Each update needs a diff, or buildChangeComment returns "" and the update
	// is never given a comment alias.
	updates := make([]model.IssueUpdate, 0, 3)
	for i := range 3 {
		updates = append(updates, model.IssueUpdate{
			Existing: model.ExistingIssue{ID: fmt.Sprintf("i%d", i), Identifier: fmt.Sprintf("SEC-%d", i), StateName: "Todo"},
			Desired:  model.DesiredIssue{Fingerprint: fmt.Sprintf("f%d", i), State: model.StateDone},
			Diff:     &model.IssueDiff{StateChanged: true, StateFrom: "Todo", StateTo: "Done"},
		})
	}

	failed, err := client.PostComments(context.Background(), updates)
	if err != nil {
		t.Fatalf("PostComments() error = %v, want nil: partial data must not be discarded, "+
			"otherwise the two posted comments are duplicated on retry", err)
	}
	if !slices.Equal(failed, []int{1}) {
		t.Fatalf("failed indices = %#v, want [1]", failed)
	}
}

// TestExistingFingerprintsRejectsNonAdvancingCursor pins the pagination guard. A
// server that keeps reporting hasNextPage with an unchanged cursor would loop
// forever, and the run context may carry no deadline to break out of it.
func TestExistingFingerprintsRejectsNonAdvancingCursor(t *testing.T) {
	requests := 0
	client := &Client{
		cfg:          config.LinearConfig{TeamID: "team-1"},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if requests > 10 {
					t.Fatal("ExistingFingerprints kept paginating on an unchanged cursor")
				}
				if _, err := io.ReadAll(req.Body); err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				// Always the same cursor, always claiming another page.
				return jsonResponse(t, `{"data":{"issues":{
					"nodes":[],
					"pageInfo":{"hasNextPage":true,"endCursor":"stuck"}
				}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	_, err := client.ExistingFingerprints(context.Background(), []string{"snyk-dast:t:1"})
	if err == nil {
		t.Fatal("ExistingFingerprints() error = nil, want an error when the cursor does not advance")
	}
	if !strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("error = %v, want it to name the non-advancing cursor", err)
	}
}

// TestExistingFingerprintsReportsOnlyLiveMatches confirms the lookup returns the
// requested fingerprints that exist and ignores anything else Linear returns.
func TestExistingFingerprintsReportsOnlyLiveMatches(t *testing.T) {
	var capturedQuery string
	client := &Client{
		cfg:          config.LinearConfig{TeamID: "team-1"},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query string `json:"query"`
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				capturedQuery = payload.Query
				return jsonResponse(t, `{"data":{"issues":{
					"nodes":[
						{"id":"i1","identifier":"SEC-1","description":"<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:t:1\n-->"},
						{"id":"i9","identifier":"SEC-9","description":"<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:t:999\n-->"}
					],
					"pageInfo":{"hasNextPage":false,"endCursor":null}
				}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	found, err := client.ExistingFingerprints(context.Background(), []string{"snyk-dast:t:1", "snyk-dast:t:2"})
	if err != nil {
		t.Fatalf("ExistingFingerprints() error = %v", err)
	}
	if _, ok := found["snyk-dast:t:1"]; !ok {
		t.Fatalf("found = %#v, want it to contain the live fingerprint", found)
	}
	if _, ok := found["snyk-dast:t:2"]; ok {
		t.Fatal("found contains a fingerprint with no live issue")
	}
	if len(found) != 1 {
		t.Fatalf("found = %#v, want exactly the one requested fingerprint that exists", found)
	}
	// Archived issues must not count as evidence a create landed, or the
	// replacement ticket for a reopened archived finding would be suppressed.
	if !strings.Contains(capturedQuery, "includeArchived: false") {
		t.Fatalf("fingerprint lookup must be explicit about excluding archived issues, got:\n%s", capturedQuery)
	}
}

// existingFingerprintsFilter runs ExistingFingerprints against a stub that
// captures the filter variable, so a test can assert the query's shape.
func existingFingerprintsFilter(t *testing.T, managedLabel string) map[string]any {
	t.Helper()
	var captured map[string]any

	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			Labels: config.LabelConfig{Managed: managedLabel},
		},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Variables map[string]any `json:"variables"`
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				if filter, ok := payload.Variables["filter"].(map[string]any); ok {
					captured = filter
				}
				return jsonResponse(t, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	if _, err := client.ExistingFingerprints(context.Background(), []string{"snyk-dast:project-a:100"}); err != nil {
		t.Fatalf("ExistingFingerprints() error = %v", err)
	}
	if captured == nil {
		t.Fatal("ExistingFingerprints() did not send a filter variable")
	}
	return captured
}

// TestExistingFingerprintsScopesQueryToManagedTickets pins the reconciliation
// query to the sync's own tickets. The lookup runs inside the retry path for
// an ambiguous batch create -- a moment when Linear has just failed a request
// -- so a filter that does not bound the result set turns one failure on a
// shared team into paging through every issue in the team.
func TestExistingFingerprintsScopesQueryToManagedTickets(t *testing.T) {
	filter := existingFingerprintsFilter(t, "snyk-dast-automation")

	raw, err := json.Marshal(filter)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	// The old shape carried an Or of `description: { contains }` clauses, which
	// Linear ignores; the fingerprint check belongs on the decoded description,
	// not in the query predicate.
	if strings.Contains(string(raw), "description") {
		t.Fatalf("filter must not constrain description; it does not bound the result set: %s", raw)
	}

	team, ok := filter["team"].(map[string]any)
	if !ok {
		t.Fatalf("filter must constrain team: %#v", filter)
	}
	teamID, ok := team["id"].(map[string]any)
	if !ok {
		t.Fatalf("team filter must constrain id: %#v", team)
	}
	if got := teamID["eq"]; got != "team-1" {
		t.Fatalf("team.id.eq = %#v, want the configured team", got)
	}

	clauses := identityArm(t, filter)
	if len(clauses) != 2 {
		t.Fatalf("identity clauses = %d, want 2 (title prefix, managed label)", len(clauses))
	}

	var sawTitle, sawLabel bool
	for _, rawClause := range clauses {
		clause, ok := rawClause.(map[string]any)
		if !ok {
			t.Fatalf("identity clause is not an object: %#v", rawClause)
		}
		if title, ok := clause["title"].(map[string]any); ok {
			if got := title["startsWith"]; got != titlePrefix {
				t.Fatalf("title.startsWith = %#v, want %q", got, titlePrefix)
			}
			sawTitle = true
			continue
		}
		if labels, ok := clause["labels"].(map[string]any); ok {
			some, ok := labels["some"].(map[string]any)
			if !ok {
				t.Fatalf("labels filter must use `some`, got: %#v", labels)
			}
			name, ok := some["name"].(map[string]any)
			if !ok {
				t.Fatalf("labels.some must constrain name, got: %#v", some)
			}
			if got := name["eqIgnoreCase"]; got != "snyk-dast-automation" {
				t.Fatalf("labels.some.name.eqIgnoreCase = %#v, want the managed label", got)
			}
			sawLabel = true
			continue
		}
		t.Fatalf("identity clause is neither a title nor a labels predicate: %#v", clause)
	}
	if !sawTitle {
		t.Fatal("identity arm lost the title-prefix clause; a retitled ticket is the only thing the label clause misses")
	}
	if !sawLabel {
		t.Fatal("identity arm is missing the managed-label clause")
	}
}

// TestExistingFingerprintsOmitsLabelArmWhenLabelManagementDisabled mirrors
// TestLoadIssuesOmitsLabelClauseWhenLabelManagementDisabled: with
// LINEAR_MANAGED_LABEL=off there is no label to match, and an arm matching
// nothing would narrow the reconciliation query rather than widen it.
func TestExistingFingerprintsOmitsLabelArmWhenLabelManagementDisabled(t *testing.T) {
	filter := existingFingerprintsFilter(t, "")

	clauses := identityArm(t, filter)
	if len(clauses) != 1 {
		t.Fatalf("identity clauses = %d, want 1 (title prefix only)", len(clauses))
	}
	clause, ok := clauses[0].(map[string]any)
	if !ok {
		t.Fatalf("identity clause is not an object: %#v", clauses[0])
	}
	if _, hasLabels := clause["labels"]; hasLabels {
		t.Fatalf("identity clause must not constrain labels with label management off: %#v", clause)
	}
	if _, hasTitle := clause["title"]; !hasTitle {
		t.Fatalf("identity clause must keep the title prefix: %#v", clause)
	}
}

// TestLoadIssuesAndExistingFingerprintsShareIdentityScope pins the shared
// scope: if one of the two queries drifts, a shared team excludes the other
// sync's tickets in one read and not the other.
func TestLoadIssuesAndExistingFingerprintsShareIdentityScope(t *testing.T) {
	snapshot := identityArm(t, loadIssuesFilter(t, "snyk-dast-automation"))
	reconciliation := identityArm(t, existingFingerprintsFilter(t, "snyk-dast-automation"))

	if !reflect.DeepEqual(snapshot, reconciliation) {
		t.Fatalf("identity scopes differ:\nsnapshot:      %#v\nreconciliation: %#v", snapshot, reconciliation)
	}
}

// e2eSnykDAST is the Snyk DAST side of the end-to-end Run test: two open
// findings for one target, so the run plans exactly one create batch.
type e2eSnykDAST struct{}

func (e2eSnykDAST) LoadSnapshot(context.Context) (model.SnykDASTSnapshot, error) {
	return model.SnykDASTSnapshot{
		Findings: []model.Finding{
			{
				Fingerprint:       "snyk-dast:project-a:100",
				SnykDASTFindingID: "SNYK-100",
				TargetID:          "target-project-a",
				TargetName:        "Example App",
				TargetHost:        "example.test",
				IssueTitle:        "Reflected XSS",
				Severity:          "high",
				Status:            model.FindingOpen,
				IssueURL:          "https://example.test/findings/100",
				CreatedAt:         time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC),
			},
			{
				Fingerprint:       "snyk-dast:project-a:101",
				SnykDASTFindingID: "SNYK-101",
				TargetID:          "target-project-a",
				TargetName:        "Example App",
				TargetHost:        "example.test",
				IssueTitle:        "SQL Injection",
				Severity:          "high",
				Status:            model.FindingOpen,
				IssueURL:          "https://example.test/findings/101",
				CreatedAt:         time.Date(2026, time.October, 1, 9, 30, 0, 0, time.UTC),
			},
		},
		TargetIDs: map[string]struct{}{"target-project-a": {}},
	}, nil
}

// hasTitlePrefixArm reports whether the decoded IssueFilter carries the
// title-prefix identity arm inside its AND/OR structure.
func hasTitlePrefixArm(filterValue any) bool {
	filter, ok := filterValue.(map[string]any)
	if !ok {
		return false
	}
	ands, ok := filter["and"].([]any)
	if !ok {
		return false
	}
	for _, rawArm := range ands {
		arm, ok := rawArm.(map[string]any)
		if !ok {
			continue
		}
		ors, ok := arm["or"].([]any)
		if !ok {
			continue
		}
		for _, rawClause := range ors {
			clause, ok := rawClause.(map[string]any)
			if !ok {
				continue
			}
			if title, ok := clause["title"].(map[string]any); ok {
				if startsWith, _ := title["startsWith"].(string); startsWith == titlePrefix {
					return true
				}
			}
		}
	}
	return false
}

// TestRunReconcilesAmbiguousBatchCreateThroughRealClientScope drives the
// ambiguous-create reconciliation end to end through sync.Service.Run with the
// real linear.Client. The first issueCreateBatch dies with a 502 (ambiguous:
// the mutation may or may not have landed), and the issuesByFingerprint
// reconciliation is only answered when its filter carries the title-prefix
// identity arm -- an unscoped query, like the `description contains` shape it
// replaces, gets an error, which would fail the whole batch instead of
// reconciling it.
func TestRunReconcilesAmbiguousBatchCreateThroughRealClientScope(t *testing.T) {
	var (
		mu                 sync.Mutex
		ambiguousLeft      = 1
		ambiguousSeen      int
		reconcileCalls     int
		createDescriptions []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "unmarshal: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(payload.Query, "query resolveTeam"):
			_, _ = io.WriteString(w, `{"data":{"teams":{"nodes":[{"id":"team-1","key":"team-1","name":"Team A"}]}}}`)
		case strings.Contains(payload.Query, "query teamStates"):
			_, _ = io.WriteString(w, `{"data":{"team":{"states":{"nodes":[{"id":"state-todo","name":"Todo","type":"started"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`)
		case strings.Contains(payload.Query, "query existingIssues"):
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		case strings.Contains(payload.Query, "query managedIssueLabels"):
			_, _ = io.WriteString(w, `{"data":{"issueLabels":{"nodes":[{"id":"label-managed","name":"snyk-dast-automation","team":{"id":"team-1"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		case strings.Contains(payload.Query, "query issuesByFingerprint"):
			mu.Lock()
			reconcileCalls++
			mu.Unlock()
			if !hasTitlePrefixArm(payload.Variables["filter"]) {
				http.Error(w, "issuesByFingerprint: query is not scoped to managed tickets", http.StatusBadRequest)
				return
			}
			// One of the batch's two creates landed before the response was
			// lost: fingerprint 100 exists live, 101 does not.
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"i-100","identifier":"SEC-100","description":"body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:project-a:100\n-->"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		case strings.Contains(payload.Query, "mutation issueCreateBatch"):
			mu.Lock()
			if ambiguousLeft > 0 {
				ambiguousLeft--
				ambiguousSeen++
				mu.Unlock()
				http.Error(w, "batch create transport failure", http.StatusBadGateway)
				return
			}
			// The retry: record every input, answer success for each alias.
			for i := 0; ; i++ {
				input, ok := payload.Variables[fmt.Sprintf("input%d", i)].(map[string]any)
				if !ok {
					break
				}
				description, _ := input["description"].(string)
				createDescriptions = append(createDescriptions, description)
			}
			mu.Unlock()
			_, _ = io.WriteString(w, `{"data":{"issueCreate0":{"success":true,"issue":{"id":"i-101","identifier":"SEC-101"}}}}`)
		default:
			http.Error(w, "unhandled operation: "+payload.Query, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := New(config.LinearConfig{
		TeamID: "team-1",
		APIKey: "test-key",
		Labels: config.LabelConfig{Managed: "snyk-dast-automation"},
		States: config.StateConfig{Todo: "Todo"},
		Due: config.DueDateConfig{
			CriticalDays: 15,
			HighDays:     30,
			MediumDays:   45,
			LowDays:      90,
		},
	}, 1, logger)
	// New hardcodes the real Linear endpoint; point the client at the stub, as
	// the other real-client tests above do.
	client.gql = gqlclient.New(server.URL, &http.Client{})

	cfg := config.Config{
		Linear: config.LinearConfig{
			TeamID: "team-1",
			Labels: config.LabelConfig{Managed: "snyk-dast-automation"},
			States: config.StateConfig{Todo: "Todo"},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
	service := linSync.New(cfg, logger, e2eSnykDAST{}, client, nil)

	// The fixed server returns immediately; the deadline only bounds a
	// regression that would wait out a backoff.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := service.Run(ctx)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0: reconciliation and the retry must both succeed", result.FailedOps)
	}

	mu.Lock()
	defer mu.Unlock()
	if ambiguousSeen != 1 {
		t.Fatalf("ambiguous 502 responses served = %d, want exactly the first batch attempt", ambiguousSeen)
	}
	if reconcileCalls != 1 {
		t.Fatalf("issuesByFingerprint calls = %d, want exactly one reconciliation query", reconcileCalls)
	}
	if len(createDescriptions) != 1 {
		t.Fatalf("successful creates = %d, want exactly one retry (for the unlanded fingerprint)", len(createDescriptions))
	}
	if !strings.Contains(createDescriptions[0], "snyk-dast:project-a:101") {
		t.Fatalf("retried create is not for fingerprint %q, got description:\n%s", "snyk-dast:project-a:101", createDescriptions[0])
	}
	if strings.Contains(createDescriptions[0], "snyk-dast:project-a:100") {
		t.Fatalf("the landed fingerprint snyk-dast:project-a:100 was retried; got description:\n%s", createDescriptions[0])
	}
}

// protectedTestLabels is a representative protected set: control labels owned by
// another actor (a triage/dispatch bot) that this sync must never add or remove.
var protectedTestLabels = []string{"df:dispatch", "df:dispatch-complete"}

func protectedLabelClient() *Client {
	return &Client{
		cfg: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed:   "snyk-dast-automation",
				Protected: protectedTestLabels,
			},
		},
		managedLabelIDs: map[string]string{
			"snyk-dast-automation": "label-managed",
			"df:dispatch":          "label-dispatch",
		},
	}
}

// TestDesiredLabelIDsPreservesProtectedLabelAddedMidRun covers the race the
// protected set exists for: another actor labels a ticket after this run's
// opening snapshot, then an update echoes the whole label set back and deletes
// the label. The live read is the authority for protected labels, so it must
// win over the snapshot.
func TestDesiredLabelIDsPreservesProtectedLabelAddedMidRun(t *testing.T) {
	client := protectedLabelClient()

	existing := model.ExistingIssue{
		ManagedLabels: []string{"snyk-dast-automation"},
		Labels: []model.IssueLabel{
			{ID: "label-unrelated", Name: "customer-visible"},
			{ID: "label-managed", Name: "snyk-dast-automation"},
		},
	}
	// The live read sees a protected label the snapshot never had.
	live := []model.IssueLabel{
		{ID: "label-unrelated", Name: "customer-visible"},
		{ID: "label-managed", Name: "snyk-dast-automation"},
		{ID: "label-dispatch", Name: "df:dispatch"},
	}
	desired := model.DesiredIssue{ManagedLabels: []string{"snyk-dast-automation"}}

	labelIDs, err := client.desiredLabelIDs(existing, desired, live)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if !containsString(labelIDs, "label-dispatch") {
		t.Fatalf("labelIDs = %#v, want the protected df:dispatch label preserved", labelIDs)
	}
	if !containsString(labelIDs, "label-unrelated") {
		t.Fatalf("labelIDs = %#v, want unrelated labels still preserved", labelIDs)
	}
	if !containsString(labelIDs, "label-managed") {
		t.Fatalf("labelIDs = %#v, want the managed label still asserted", labelIDs)
	}
}

// TestDesiredLabelIDsDropsProtectedLabelRemovedMidRun is the other direction:
// protection must not resurrect a label the owning actor has just removed, so an
// update must never re-add one the snapshot still carries.
func TestDesiredLabelIDsDropsProtectedLabelRemovedMidRun(t *testing.T) {
	client := protectedLabelClient()

	existing := model.ExistingIssue{
		Labels: []model.IssueLabel{
			{ID: "label-unrelated", Name: "customer-visible"},
			{ID: "label-dispatch", Name: "df:dispatch"},
		},
	}
	// The owning actor removed df:dispatch and added the terminal gate.
	live := []model.IssueLabel{
		{ID: "label-unrelated", Name: "customer-visible"},
		{ID: "label-complete", Name: "df:dispatch-complete"},
	}

	labelIDs, err := client.desiredLabelIDs(existing, model.DesiredIssue{}, live)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if containsString(labelIDs, "label-dispatch") {
		t.Fatalf("labelIDs = %#v, want the removed protected label left off", labelIDs)
	}
	if !containsString(labelIDs, "label-complete") {
		t.Fatalf("labelIDs = %#v, want the live protected label preserved", labelIDs)
	}
}

// TestDesiredLabelIDsNeverAssertsProtectedLabelFromManagedSet bounds the blast
// radius of a misconfiguration: even if a protected label somehow lands in the
// managed set, an update must not stamp another actor's control label onto a
// ticket that does not carry it.
func TestDesiredLabelIDsNeverAssertsProtectedLabelFromManagedSet(t *testing.T) {
	client := protectedLabelClient()

	desired := model.DesiredIssue{ManagedLabels: []string{"snyk-dast-automation", "df:dispatch"}}

	labelIDs, err := client.desiredLabelIDs(model.ExistingIssue{}, desired, nil)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if containsString(labelIDs, "label-dispatch") {
		t.Fatalf("labelIDs = %#v, want no protected label asserted from the managed set", labelIDs)
	}
	if !containsString(labelIDs, "label-managed") {
		t.Fatalf("labelIDs = %#v, want the non-protected managed label present", labelIDs)
	}
}

// TestUpdateIssuesRefusesUpdateWhenLiveLabelsUnreadable pins the fail-closed
// choice. An issue absent from the live read is indistinguishable from one with
// no protected label, so the update must not proceed — guessing wrong deletes
// another actor's control label. The caller retries issues individually, so only
// the unreadable one is reported failed.
func TestUpdateIssuesRefusesUpdateWhenLiveLabelsUnreadable(t *testing.T) {
	var sawUpdateMutation bool

	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			States: config.StateConfig{Todo: "Todo"},
			Labels: config.LabelConfig{Protected: protectedTestLabels},
		},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if strings.Contains(string(body), "issueUpdate") {
					sawUpdateMutation = true
					return jsonResponse(t, `{"data":{"issueUpdate0":{"success":true}}}`), nil
				}
				// The live label read returns nothing for the requested issue.
				return jsonResponse(t, `{"data":{"issues":{"nodes":[]}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{"todo": "state-todo"},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	updates := []model.IssueUpdate{{
		Existing: model.ExistingIssue{ID: "issue-1", Identifier: "SEC-1"},
		Desired:  model.DesiredIssue{State: model.StateTodo},
	}}

	err := client.UpdateIssues(context.Background(), updates)
	if err == nil {
		t.Fatal("UpdateIssues() error = nil, want a refusal when live labels cannot be read")
	}
	if !strings.Contains(err.Error(), "SEC-1") {
		t.Fatalf("UpdateIssues() error = %v, want it to name the unreadable issue", err)
	}
	if sawUpdateMutation {
		t.Fatal("UpdateIssues() sent an update mutation despite being unable to preserve protected labels")
	}
}

// TestUpdateIssuesSkipsLiveLabelReadWhenNoProtectedLabels keeps the extra query
// off the hot path for anyone running without protected labels configured.
func TestUpdateIssuesSkipsLiveLabelReadWhenNoProtectedLabels(t *testing.T) {
	var sawLabelRead bool

	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			States: config.StateConfig{Todo: "Todo"},
		},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if strings.Contains(string(body), "issueLabelsByID") {
					sawLabelRead = true
				}
				return jsonResponse(t, `{"data":{"issueUpdate0":{"success":true}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{"todo": "state-todo"},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	updates := []model.IssueUpdate{{
		Existing: model.ExistingIssue{ID: "issue-1", Identifier: "SEC-1"},
		Desired:  model.DesiredIssue{State: model.StateTodo},
	}}

	if err := client.UpdateIssues(context.Background(), updates); err != nil {
		t.Fatalf("UpdateIssues() error = %v", err)
	}
	if sawLabelRead {
		t.Fatal("UpdateIssues() read live labels with no protected labels configured")
	}
}

// TestFetchLabelsByIssueIDFiltersByIssueID pins the live read's shape: it must
// filter by the exact issue IDs of the batch and include archived issues, since
// an archived ticket can still be the target of an update.
func TestFetchLabelsByIssueIDFiltersByIssueID(t *testing.T) {
	var capturedQuery string
	var capturedFilter map[string]any

	client := &Client{
		cfg:          config.LinearConfig{TeamID: "team-1"},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				capturedQuery = payload.Query
				if filter, ok := payload.Variables["filter"].(map[string]any); ok {
					capturedFilter = filter
				}
				return jsonResponse(t, `{"data":{"issues":{"nodes":[
					{"id":"issue-1","labels":{"nodes":[{"id":"label-dispatch","name":"df:dispatch"}]}}
				]}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	labels, err := client.fetchLabelsByIssueID(context.Background(), []string{"issue-1", "issue-2"})
	if err != nil {
		t.Fatalf("fetchLabelsByIssueID() error = %v", err)
	}

	if !strings.Contains(capturedQuery, "includeArchived: true") {
		t.Fatalf("live label query must pass includeArchived: true, got:\n%s", capturedQuery)
	}
	id, ok := capturedFilter["id"].(map[string]any)
	if !ok {
		t.Fatalf("filter must carry an id comparator, got: %#v", capturedFilter)
	}
	in, ok := id["in"].([]any)
	if !ok || len(in) != 2 {
		t.Fatalf("filter id.in = %#v, want both batch issue IDs", id["in"])
	}

	if len(labels["issue-1"]) != 1 || labels["issue-1"][0].Name != "df:dispatch" {
		t.Fatalf("labels[issue-1] = %#v, want the df:dispatch label", labels["issue-1"])
	}
	// issue-2 is absent from the response, and must stay absent from the map so
	// UpdateIssues can tell "unreadable" from "no protected labels".
	if _, ok := labels["issue-2"]; ok {
		t.Fatalf("labels = %#v, want issue-2 absent rather than an empty entry", labels)
	}
}

// TestUpdateIssuesRefusesUpdateWhenLabelPageTruncated covers the pagination edge
// of the live label read. A protected label past the page boundary would be
// absent from the read, and the update would then delete it — the exact failure
// the read exists to prevent. So a truncated page must fail the issue closed, not
// be treated as a complete label set.
func TestUpdateIssuesRefusesUpdateWhenLabelPageTruncated(t *testing.T) {
	var sawUpdateMutation bool

	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			States: config.StateConfig{Todo: "Todo"},
			Labels: config.LabelConfig{Protected: protectedTestLabels},
		},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if strings.Contains(string(body), "issueUpdate") {
					sawUpdateMutation = true
					return jsonResponse(t, `{"data":{"issueUpdate0":{"success":true}}}`), nil
				}
				// The issue's labels do not fit one page, so the protected label
				// may be on a page this read never saw.
				return jsonResponse(t, `{"data":{"issues":{"nodes":[
					{"id":"issue-1","labels":{
						"nodes":[{"id":"label-ordinary","name":"customer-visible"}],
						"pageInfo":{"hasNextPage":true}
					}}
				]}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{"todo": "state-todo"},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	updates := []model.IssueUpdate{{
		Existing: model.ExistingIssue{ID: "issue-1", Identifier: "SEC-1"},
		Desired:  model.DesiredIssue{State: model.StateTodo},
	}}

	err := client.UpdateIssues(context.Background(), updates)
	if err == nil {
		t.Fatal("UpdateIssues() error = nil, want a refusal when the live label page is truncated")
	}
	if !strings.Contains(err.Error(), "SEC-1") {
		t.Fatalf("UpdateIssues() error = %v, want it to name the issue", err)
	}
	if sawUpdateMutation {
		t.Fatal("UpdateIssues() sent an update mutation from a truncated label page; a protected label past the page boundary would be deleted")
	}
}

// TestFetchLabelsByIssueIDRequestsLabelPageInfo pins the query shape: without
// pageInfo on the nested labels connection there is no way to tell a complete
// label set from a truncated one.
func TestFetchLabelsByIssueIDRequestsLabelPageInfo(t *testing.T) {
	var capturedQuery string

	client := &Client{
		cfg:          config.LinearConfig{TeamID: "team-1"},
		resolvedTeam: "team-1",
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query string `json:"query"`
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				capturedQuery = payload.Query
				return jsonResponse(t, `{"data":{"issues":{"nodes":[]}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	if _, err := client.fetchLabelsByIssueID(context.Background(), []string{"issue-1"}); err != nil {
		t.Fatalf("fetchLabelsByIssueID() error = %v", err)
	}
	if !strings.Contains(capturedQuery, "hasNextPage") {
		t.Fatalf("live label query must request the labels pageInfo, got:\n%s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, fmt.Sprintf("labels(first: %d)", maxLiveLabelPage)) {
		t.Fatalf("live label query must request %d labels per issue, got:\n%s", maxLiveLabelPage, capturedQuery)
	}
}

// TestCreateIssuesSendsOneMutationWhenRateLimitBudgetExhausted is the
// end-to-end regression test for the duplicate-ticket bug: Linear sends
// `X-RateLimit-Requests-Remaining: 0` on every response, including the 200 to
// the mutation that consumed the last unit of the window. A transport that
// treated that header as "rate limited" would discard the successful response
// and re-send the same mutation, so one batch of ten issues would create up to
// fifty tickets. The transport must return the successful response and only
// delay the *next* request.
func TestCreateIssuesSendsOneMutationWhenRateLimitBudgetExhausted(t *testing.T) {
	// Both counters are written by the httptest handler goroutine while the
	// test goroutine reads them, so they are atomic (this test runs under
	// -race).
	var createBatches atomic.Int32
	var unsubscribes atomic.Int32
	var otherRequests atomic.Int32
	handlerReturned := make(chan struct{}, 8)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			select {
			case handlerReturned <- struct{}{}:
			default:
			}
		}()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
			return
		}
		query := string(body)

		// Linear sends the budget header on every response, successful or
		// not. The reset instant is already in the past so a "0 remaining"
		// header cannot park the next request on a real multi-second wait.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Requests-Remaining", "0")
		w.Header().Set("X-RateLimit-Requests-Reset",
			strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10))

		switch {
		case strings.Contains(query, "issueCreateBatch"):
			createBatches.Add(1)
			_, _ = io.WriteString(w, `{"data":{"issueCreate0":{"success":true,"issue":{"id":"issue-1","identifier":"SNYK-100"}}}}`)
		case strings.Contains(query, "issueUnsubscribe"):
			unsubscribes.Add(1)
			_, _ = io.WriteString(w, `{"data":{"issueUnsubscribe0":{"success":true}}}`)
		default:
			// Any other operation gets a valid (empty-data) success body so
			// the test never fails for an unrelated reason, but it is counted
			// so an unexpected round trip is visible.
			otherRequests.Add(1)
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := New(config.LinearConfig{
		TeamID:           "team-1",
		APIKey:           "test-key",
		UnsubscribeActor: true,
		States:           config.StateConfig{Todo: "Todo"},
	}, 1, logger)
	// New hardcodes the real Linear endpoint; point the client at the stub,
	// exactly as the CreateIssues tests above do. The AdaptiveTransport and
	// HeaderTransport built by New stay in place, which is the point here.
	client.gql = gqlclient.New(server.URL, &http.Client{})

	// Pre-seed the client so the batch needs no team/state/label round trips.
	client.mu.Lock()
	client.resolvedTeam = "team-1"
	client.statesByName = map[string]string{"todo": "state-todo"}
	client.statesByType = map[string]string{}
	client.managedLabelIDs = map[string]string{}
	client.mu.Unlock()

	desired := []model.DesiredIssue{
		{
			Fingerprint: "snyk-dast:project-a:100",
			Title:       "t",
			Description: "",
			State:       model.StateTodo,
			Priority:    0,
		},
	}

	// The fixed client returns immediately; the deadline only bounds a
	// regression that would wait on the rate-limit window.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	failed, err := client.CreateIssues(ctx, desired)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("CreateIssues() error = %v, want nil", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %#v, want no failed indices", failed)
	}
	// The real fix returns at once instead of sleeping out a backoff.
	if elapsed > 2*time.Second {
		t.Fatalf("CreateIssues() took %v, want it to return immediately without re-sending", elapsed)
	}

	// Let any (buggy) re-send reach the server before counting: the handler
	// acknowledges every request it serves, so this is a bounded wait, not a
	// policy sleep.
	waitForRequests := time.NewTimer(500 * time.Millisecond)
	defer waitForRequests.Stop()
	deadline := time.After(500 * time.Millisecond)
	for createBatches.Load() == 0 {
		select {
		case <-handlerReturned:
		case <-deadline:
			t.Fatalf("server saw no issueCreateBatch request, want exactly 1")
		}
	}
	select {
	case <-handlerReturned:
	case <-waitForRequests.C:
	}

	gotCreate := createBatches.Load()
	gotUnsubscribe := unsubscribes.Load()
	gotOther := otherRequests.Load()
	t.Logf("recorded requests: issueCreateBatch=%d issueUnsubscribe=%d other=%d", gotCreate, gotUnsubscribe, gotOther)

	if gotCreate != 1 {
		t.Fatalf("issueCreateBatch requests = %d, want exactly 1: a successful mutation must not be re-sent "+
			"when X-RateLimit-Requests-Remaining is 0 (a re-send duplicates every ticket in the batch)",
			gotCreate)
	}
	if gotUnsubscribe > 1 {
		t.Fatalf("issueUnsubscribe requests = %d, want at most 1", gotUnsubscribe)
	}
	if gotOther != 0 {
		t.Fatalf("other requests = %d, want 0: the batch must need no extra lookups", gotOther)
	}
	if gotCreate >= 2 || gotUnsubscribe >= 2 {
		t.Fatalf("server saw %d issueCreateBatch and %d issueUnsubscribe requests, want never 2+ per operation",
			gotCreate, gotUnsubscribe)
	}
}

// TestDesiredLabelIDsManagedLabelEvictsExclusiveGroupSibling guards against
// "labelIds not exclusive child labels": when the sync asserts a managed label
// that belongs to a Linear exclusive group, and the ticket already carries a
// different child of that same group, the carried-forward sibling must be
// dropped so the update names only one child of the group. The managed label
// wins.
func TestDesiredLabelIDsManagedLabelEvictsExclusiveGroupSibling(t *testing.T) {
	client := &Client{
		managedLabelIDs: map[string]string{
			"status-confirmed": "label-confirmed",
		},
		managedLabelGroupIDs: map[string]string{
			"status-confirmed": "group-status",
		},
	}

	// The ticket already carries status-new, another child of the exclusive
	// "status" group, applied by a human or a prior run. It is not in the
	// sync's managed set for this finding, so it would otherwise be carried
	// forward alongside the asserted status-confirmed and Linear would reject
	// the update.
	existing := model.ExistingIssue{
		Labels: []model.IssueLabel{
			{ID: "label-unrelated", Name: "customer-visible"},
			{ID: "label-new", Name: "status-new", GroupID: "group-status"},
		},
	}
	desired := model.DesiredIssue{
		ManagedLabels: []string{"status-confirmed"},
	}

	labelIDs, err := client.desiredLabelIDs(existing, desired, nil)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if containsString(labelIDs, "label-new") {
		t.Fatalf("labelIDs = %#v, want carried-forward group sibling 'label-new' evicted", labelIDs)
	}
	if !containsString(labelIDs, "label-confirmed") {
		t.Fatalf("labelIDs = %#v, want asserted managed group child 'label-confirmed' present", labelIDs)
	}
	if !containsString(labelIDs, "label-unrelated") {
		t.Fatalf("labelIDs = %#v, want ungrouped label preserved", labelIDs)
	}
	if len(labelIDs) != 2 {
		t.Fatalf("labelIDs len = %d, want 2 (at most one child per exclusive group)", len(labelIDs))
	}
}

// TestDesiredLabelIDsDropsSecondCarriedForwardExclusiveSibling covers two
// children of the same exclusive group present on the ticket with neither in
// the sync's managed set — for example two labels applied before the group was
// made exclusive. Echoing both back fails the update, so the set keeps the
// first read and drops the rest.
func TestDesiredLabelIDsDropsSecondCarriedForwardExclusiveSibling(t *testing.T) {
	client := &Client{}

	existing := model.ExistingIssue{
		Labels: []model.IssueLabel{
			{ID: "label-sev-high", Name: "sev:high", GroupID: "group-sev"},
			{ID: "label-sev-crit", Name: "sev:critical", GroupID: "group-sev"},
			{ID: "label-plain", Name: "backend"},
		},
	}

	labelIDs, err := client.desiredLabelIDs(existing, model.DesiredIssue{}, nil)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if !containsString(labelIDs, "label-sev-high") {
		t.Fatalf("labelIDs = %#v, want first exclusive-group child kept", labelIDs)
	}
	if containsString(labelIDs, "label-sev-crit") {
		t.Fatalf("labelIDs = %#v, want second exclusive-group child dropped", labelIDs)
	}
	if !containsString(labelIDs, "label-plain") {
		t.Fatalf("labelIDs = %#v, want ungrouped label preserved", labelIDs)
	}
	if len(labelIDs) != 2 {
		t.Fatalf("labelIDs len = %d, want 2", len(labelIDs))
	}
}

// TestDesiredLabelIDsKeepsChildrenOfDistinctGroups is a regression guard: the
// exclusive-group collapse must only fold labels that share a group. Children
// of different groups, and ungrouped labels, are all preserved.
func TestDesiredLabelIDsKeepsChildrenOfDistinctGroups(t *testing.T) {
	client := &Client{}

	existing := model.ExistingIssue{
		Labels: []model.IssueLabel{
			{ID: "label-tool", Name: "tool-a", GroupID: "group-tool"},
			{ID: "label-origin", Name: "origin-b", GroupID: "group-origin"},
			{ID: "label-plain", Name: "security"},
		},
	}

	labelIDs, err := client.desiredLabelIDs(existing, model.DesiredIssue{}, nil)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if len(labelIDs) != 3 {
		t.Fatalf("labelIDs = %#v, want all three distinct-group/ungrouped labels kept", labelIDs)
	}
}
