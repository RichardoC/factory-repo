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
	"slices"
	"strconv"
	"strings"
	syncstd "sync"
	"testing"
	"time"

	gqlclient "git.sr.ht/~emersion/gqlclient"

	"github.com/tesslio/snyk-linear-sync/internal/config"
	"github.com/tesslio/snyk-linear-sync/internal/model"
	linSync "github.com/tesslio/snyk-linear-sync/internal/sync"
)

func TestDesiredLabelIDsReplacesPreviousManagedLabel(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed: "snyk-automation",
			},
		},
		managedLabelIDs: map[string]string{
			"snyk-automation": "label-new",
			"snyk-code":       "label-code",
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
		ManagedLabels: []string{"snyk-automation", "snyk-code"},
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
	if !containsString(labelIDs, "label-code") {
		t.Fatalf("labelIDs = %#v, want tool label present", labelIDs)
	}
	if containsString(labelIDs, "label-old") {
		t.Fatalf("labelIDs = %#v, want old managed label removed", labelIDs)
	}
}

func TestDesiredLabelIDsRemovesManagedLabelWhenDisabled(t *testing.T) {
	client := &Client{}
	existing := model.ExistingIssue{
		ManagedLabels: []string{"snyk-automation", "snyk-code"},
		Labels: []model.IssueLabel{
			{ID: "label-unrelated", Name: "customer-visible"},
			{ID: "label-managed", Name: "snyk-automation"},
			{ID: "label-tool", Name: "snyk-code"},
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
				if strings.Contains(payload.Query, "query issueLabelsByID") {
					return jsonResponse(t, `{"data":{"issues":{"nodes":[{"id":"issue-1","labels":{"nodes":[{"id":"label-1","name":"snyk-automation"}]}}]}}}`), nil
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
			ManagedLabels: []string{"snyk-automation"},
			Labels:        []model.IssueLabel{{ID: "label-1", Name: "snyk-automation"}},
		},
		Desired: model.DesiredIssue{
			Fingerprint:   "snyk:proj-1:issue-1",
			Title:         "Snyk: title",
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

// TestLoadSnapshotArchiveFilterMatchesNotArchivedIssues guards against a
// regression where the "not archived" OR clauses in loadIssues' filter used
// AutoArchivedAt.Null: false instead of true. Per Linear's
// NullableDateComparator, "null: false" matches non-null values (i.e.
// archived issues) while "null: true" matches null values (i.e. NOT
// archived issues). With the inverted boolean, the first two OR clauses
// matched only archived tickets -- the opposite of their intent -- so the
// snapshot silently dropped every non-archived managed ticket outside the
// lookback window's Gte clauses, and the sync created duplicate tickets for
// issues that already had an open Linear ticket.
func TestLoadSnapshotArchiveFilterMatchesNotArchivedIssues(t *testing.T) {
	var capturedFilter map[string]any

	client := &Client{
		cfg: config.LinearConfig{
			TeamID:              "11111111-1111-1111-1111-111111111111",
			ArchiveLookbackDays: 21,
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

				switch {
				case strings.Contains(payload.Query, "query teamStates"):
					return jsonResponse(t, `{"data":{"team":{"states":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`), nil
				case strings.Contains(payload.Query, "query existingIssues"):
					filter, ok := payload.Variables["filter"].(map[string]any)
					if !ok {
						t.Fatalf("filter variable missing or wrong type: %#v", payload.Variables["filter"])
					}
					capturedFilter = filter
					return jsonResponse(t, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`), nil
				default:
					t.Fatalf("unexpected GraphQL query: %s", payload.Query)
					return nil, nil
				}
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName: map[string]string{},
		statesByType: map[string]string{},
	}

	if _, err := client.LoadSnapshot(t.Context()); err != nil {
		t.Fatalf("LoadSnapshot() error = %v", err)
	}
	if capturedFilter == nil {
		t.Fatal("no filter captured for the existingIssues query")
	}

	orClauses := archiveWindowArm(t, capturedFilter)

	var nullTrueCount, gteCount int
	for _, raw := range orClauses {
		clause, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("OR clause has wrong type: %#v", raw)
		}
		autoArchivedAt, ok := clause["autoArchivedAt"].(map[string]any)
		if !ok {
			t.Fatalf("OR clause missing autoArchivedAt: %#v", clause)
		}
		if nullVal, has := autoArchivedAt["null"]; has {
			if nullVal != true {
				t.Fatalf("autoArchivedAt.null = %#v, want true (matches NOT-archived issues); "+
					"null:false would match only archived issues, hiding open managed tickets", nullVal)
			}
			nullTrueCount++
			continue
		}
		if gte, has := autoArchivedAt["gte"]; has {
			gteStr, ok := gte.(string)
			if !ok || gteStr == "" {
				t.Fatalf("autoArchivedAt.gte = %#v, want a non-empty cutoff timestamp", gte)
			}
			gteCount++
			continue
		}
		t.Fatalf("OR clause autoArchivedAt has neither null nor gte: %#v", autoArchivedAt)
	}

	if nullTrueCount != 1 {
		t.Fatalf("clauses with autoArchivedAt.null=true = %d, want 1", nullTrueCount)
	}
	if gteCount != 1 {
		t.Fatalf("clauses with autoArchivedAt.gte = %d, want 1", gteCount)
	}
}

// andArms returns the filter's top-level AND arms. The filter is deliberately
// shaped as team AND (archive window) AND (identity) rather than leaving an `or`
// as a sibling of other fields, so the grouping does not depend on how Linear
// interprets a mixed filter object.
func andArms(t *testing.T, filter map[string]any) []any {
	t.Helper()
	arms, ok := filter["and"].([]any)
	if !ok {
		t.Fatalf("filter must carry an AND arm list, got: %#v", filter)
	}
	if len(arms) != 2 {
		t.Fatalf("AND arms = %d, want 2 (archive window, identity)", len(arms))
	}
	return arms
}

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
// autoArchivedAt, so tests do not depend on arm ordering.
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

// identityArm returns the OR clauses of whichever AND arm selects this sync's
// own tickets.
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

// loadIssuesFilter runs LoadSnapshot against a stub that captures the filter
// variable of the existingIssues query.
func loadIssuesFilter(t *testing.T, managedLabel string) map[string]any {
	t.Helper()
	var captured map[string]any

	client := &Client{
		cfg: config.LinearConfig{
			TeamID:              "11111111-1111-1111-1111-111111111111",
			ArchiveLookbackDays: 21,
			Labels:              config.LabelConfig{Managed: managedLabel},
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
				switch {
				case strings.Contains(payload.Query, "query teamStates"):
					return jsonResponse(t, `{"data":{"team":{"states":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`), nil
				case strings.Contains(payload.Query, "query existingIssues"):
					if filter, ok := payload.Variables["filter"].(map[string]any); ok {
						captured = filter
					}
					return jsonResponse(t, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`), nil
				default:
					t.Fatalf("unexpected GraphQL query: %s", payload.Query)
					return nil, nil
				}
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName: map[string]string{},
		statesByType: map[string]string{},
	}

	if _, err := client.LoadSnapshot(t.Context()); err != nil {
		t.Fatalf("LoadSnapshot() error = %v", err)
	}
	if captured == nil {
		t.Fatal("no filter captured for the existingIssues query")
	}
	return captured
}

// TestLoadIssuesScopesSnapshotToManagedLabel is the regression guard for the
// snapshot spanning a shared Linear team. The sibling snyk-dast-linear-sync,
// which used this identical filter shape against this same team, loaded 59,866
// issues for 7 findings because the identity arm filtered on
// `description: { contains: <metadata header> }` and that predicate did not
// bound the result set. The identity arm must use exact predicates instead: the
// managed title prefix and the managed label name.
func TestLoadIssuesScopesSnapshotToManagedLabel(t *testing.T) {
	filter := loadIssuesFilter(t, "snyk-automation")

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
			if got := name["eqIgnoreCase"]; got != "snyk-automation" {
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
// LINEAR_MANAGED_LABEL=off working: an arm matching a label that is never
// applied would narrow the snapshot, hiding managed tickets and minting
// duplicates.
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

func TestExtractFingerprintPrefersMetadataBlock(t *testing.T) {
	description := "## Example\n\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\nmanaged_labels: snyk-automation,snyk-code\n-->"

	got := extractFingerprint(description)

	if got != "snyk:project-a:issue-1" {
		t.Fatalf("extractFingerprint() = %q, want %q", got, "snyk:project-a:issue-1")
	}
}

// TestExtractFingerprintCanonicalizesLinearMangledMarkdown reproduces the
// stored form observed via the live Linear API: the sync wrote a fingerprint
// containing "__main__.py" and Linear's editor re-serialized it inside the
// metadata comment as "**main**.py". Extraction must return the canonical
// form or the ticket can never be matched again (hourly close-and-recreate
// flapping).
func TestExtractFingerprintCanonicalizesLinearMangledMarkdown(t *testing.T) {
	description := "## Path Traversal [LOW]\n\nFile: [sim/**main**.py (line 92:17-21)](<https://github.com/example/repo/blob/abc/sim/__main__.py#L92>)\n\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1:sim/**main**.py\nmanaged_labels: snyk-automation,snyk-code\n-->"

	got := extractFingerprint(description)

	if want := "snyk:project-a:issue-1:sim/__main__.py"; got != want {
		t.Fatalf("extractFingerprint() = %q, want %q", got, want)
	}
}

// TestExtractAwaitingFix verifies the awaiting_fix marker parsing: present
// inside the metadata block reports true, an absent line (tickets written
// before the marker existed) reports false, and a line in user-written body
// text outside the block is ignored.
func TestExtractAwaitingFix(t *testing.T) {
	if got := extractAwaitingFix("body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\nawaiting_fix: yes\n-->"); !got {
		t.Fatalf("extractAwaitingFix(marker) = false, want true")
	}
	if got := extractAwaitingFix("body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->"); got {
		t.Fatalf("extractAwaitingFix(legacy) = true, want false (no marker line)")
	}
	if got := extractAwaitingFix("awaiting_fix: yes\n\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->"); got {
		t.Fatalf("extractAwaitingFix(outside block) = true, want false (spoofed line ignored)")
	}
	// Linear escapes underscores in the stored key (awaiting\_fix); the
	// canonicalization used for fingerprints must undo that.
	if got := extractAwaitingFix("<!-- snyk-linear-sync\nawaiting\\_fix: yes\n-->"); !got {
		t.Fatalf("extractAwaitingFix(escaped key) = false, want true")
	}
}

func TestExtractManagedLabelsSupportsLegacyAndNewMetadata(t *testing.T) {
	if got := extractManagedLabels("<!-- snyk-linear-sync\nmanaged_label: snyk-automation\n-->"); !slices.Equal(got, []string{"snyk-automation"}) {
		t.Fatalf("extractManagedLabels(legacy) = %#v", got)
	}
	if got := extractManagedLabels("<!-- snyk-linear-sync\nmanaged_labels: snyk-automation,snyk-code\n-->"); !slices.Equal(got, []string{"snyk-automation", "snyk-code"}) {
		t.Fatalf("extractManagedLabels(new) = %#v", got)
	}
}

// TestExtractFingerprintIgnoresLinesOutsideMetadataBlock verifies that a
// "Fingerprint:" line appearing in user-written body text is not extracted as
// the managed fingerprint. Only the fingerprint inside the line-anchored
// metadata block counts. This mirrors the line-boundary hardening applied to
// upsertManagedMetadata so user text cannot spoof deduplication.
func TestExtractFingerprintIgnoresLinesOutsideMetadataBlock(t *testing.T) {
	description := "Notes\nFingerprint: fake-from-user-text\n\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\nmanaged_labels: snyk-automation\n-->"

	if got := extractFingerprint(description); got != "snyk:project-a:issue-1" {
		t.Fatalf("extractFingerprint() = %q, want the metadata-block fingerprint", got)
	}
}

// TestExtractFingerprintIgnoresInlineMarker verifies that an inline marker
// (mid-sentence) is not treated as a metadata block, so no fingerprint is
// extracted from a description that has no real line-anchored block.
func TestExtractFingerprintIgnoresInlineMarker(t *testing.T) {
	description := "See <!-- snyk-linear-sync notes --> for context\nFingerprint: not-a-real-block"

	if got := extractFingerprint(description); got != "" {
		t.Fatalf("extractFingerprint() = %q, want empty (no line-anchored block)", got)
	}
	if got := extractManagedLabels(description); got != nil {
		t.Fatalf("extractManagedLabels() = %#v, want nil (no line-anchored block)", got)
	}
}

// TestExtractManagedLabelsIgnoresLinesOutsideMetadataBlock verifies that a
// "managed_labels:" line in user body text does not override the real
// metadata block value.
func TestExtractManagedLabelsIgnoresLinesOutsideMetadataBlock(t *testing.T) {
	description := "managed_labels: fake-from-user\n\n<!-- snyk-linear-sync\nmanaged_labels: snyk-automation,snyk-code\n-->"

	if got := extractManagedLabels(description); !slices.Equal(got, []string{"snyk-automation", "snyk-code"}) {
		t.Fatalf("extractManagedLabels() = %#v, want the metadata-block labels", got)
	}
}

// TestFindMetadataBlockStartReturnsLastOccurrence guards against a
// regression where findMetadataBlockStart returned the FIRST line-anchored
// "<!-- snyk-linear-sync" marker instead of the LAST. Ticket descriptions
// can embed free-form Snyk-controlled prose (issue description/remediation
// text) above the real metadata block, since the sync always appends its
// managed block last. If that prose happens to contain its own line-anchored
// marker (e.g. quoted from elsewhere), the first-occurrence behavior would
// hijack fingerprint/label extraction and break ticket matching, causing
// duplicate ticket creation.
func TestFindMetadataBlockStartReturnsLastOccurrence(t *testing.T) {
	cases := []struct {
		name        string
		description string
		wantIdx     int
	}{
		{
			name:        "single block unchanged",
			description: "Some text\n<!-- snyk-linear-sync\nfingerprint: test\n-->",
			wantIdx:     10, // after "Some text\n"
		},
		{
			name:        "fake block before real block: last wins",
			description: "<!-- snyk-linear-sync\nfingerprint: fake\n-->\nSome text\n<!-- snyk-linear-sync\nfingerprint: real\n-->",
			wantIdx:     54, // index of the second marker
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findMetadataBlockStart(tc.description)
			if got != tc.wantIdx {
				t.Fatalf("findMetadataBlockStart() = %d, want %d", got, tc.wantIdx)
			}
		})
	}
}

// TestExtractMetadataUsesLastBlockWhenFakeBlockPrecedesReal verifies that
// when a fake, line-anchored "<!-- snyk-linear-sync ... -->" block appears
// before the real one (e.g. embedded in Snyk-controlled prose above the
// sync-managed block), extractFingerprint and extractManagedLabels return
// the values from the LAST (real) block, not the first (fake) one.
func TestExtractMetadataUsesLastBlockWhenFakeBlockPrecedesReal(t *testing.T) {
	description := "<!-- snyk-linear-sync\n" +
		"fingerprint: fake-from-embedded-prose\n" +
		"managed_labels: fake-label\n" +
		"-->\n\n" +
		"## Remediation\n\nSome Snyk-controlled prose describing the finding.\n\n" +
		"<!-- snyk-linear-sync\n" +
		"fingerprint: snyk:project-a:issue-1\n" +
		"managed_labels: snyk-automation,snyk-code\n" +
		"-->"

	if got := extractFingerprint(description); got != "snyk:project-a:issue-1" {
		t.Fatalf("extractFingerprint() = %q, want the last (real) block's fingerprint", got)
	}
	if got := extractManagedLabels(description); !slices.Equal(got, []string{"snyk-automation", "snyk-code"}) {
		t.Fatalf("extractManagedLabels() = %#v, want the last (real) block's labels", got)
	}
}

// TestExtractMetadataUsesLastBlockWhenFakeBlockInsideFencedCode verifies
// that a fake metadata block embedded inside a fenced code block (still
// line-anchored, since fences don't change line-boundary semantics) does
// not win over the real, sync-appended block that follows it.
func TestExtractMetadataUsesLastBlockWhenFakeBlockInsideFencedCode(t *testing.T) {
	description := "```\n" +
		"<!-- snyk-linear-sync\n" +
		"fingerprint: fake-in-code-block\n" +
		"-->\n" +
		"```\n\n" +
		"<!-- snyk-linear-sync\n" +
		"fingerprint: snyk:project-a:issue-2\n" +
		"-->"

	if got := extractFingerprint(description); got != "snyk:project-a:issue-2" {
		t.Fatalf("extractFingerprint() = %q, want the last (real) block's fingerprint", got)
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
				if strings.Contains(payload.Query, "query issueLabelsByID") {
					return jsonResponse(t, `{"data":{"issues":{"nodes":[{"id":"issue-1","labels":{"nodes":[{"id":"label-1","name":"snyk-automation"}]}}]}}}`), nil
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
			"snyk-automation": "label-1",
		},
	}

	err := client.UpdateIssues(t.Context(), []model.IssueUpdate{{
		Existing: model.ExistingIssue{
			ID:            "issue-1",
			Identifier:    "SNYK-1",
			ManagedLabels: []string{"snyk-automation"},
			Labels:        []model.IssueLabel{{ID: "label-1", Name: "snyk-automation"}},
		},
		Desired: model.DesiredIssue{
			Fingerprint:   "snyk:proj-1:issue-1",
			Title:         "Snyk: updated title",
			Description:   "updated body",
			State:         model.StateTodo,
			ManagedLabels: []string{"snyk-automation"},
			Priority:      2,
		},
	}})
	if err != nil {
		t.Fatalf("UpdateIssues() error = %v", err)
	}
}

// TestDesiredLabelIDsPreservesManuallyAddedLabels verifies that labels added manually
// (not tracked in managed_labels metadata) are preserved when an issue is updated.
func TestDesiredLabelIDsPreservesManuallyAddedLabels(t *testing.T) {
	client := &Client{
		managedLabelIDs: map[string]string{
			"snyk-automation": "label-managed",
		},
	}

	existing := model.ExistingIssue{
		ManagedLabels: []string{"snyk-automation"},
		Labels: []model.IssueLabel{
			{ID: "label-managed", Name: "snyk-automation"},
			{ID: "label-manual-1", Name: "needs-review"},
			{ID: "label-manual-2", Name: "team-platform"},
		},
	}
	desired := model.DesiredIssue{
		ManagedLabels: []string{"snyk-automation"},
	}

	labelIDs, err := client.desiredLabelIDs(existing, desired, nil)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if !containsString(labelIDs, "label-manual-1") {
		t.Fatalf("labelIDs = %#v, want manually-added label 'needs-review' preserved", labelIDs)
	}
	if !containsString(labelIDs, "label-manual-2") {
		t.Fatalf("labelIDs = %#v, want manually-added label 'team-platform' preserved", labelIDs)
	}
	if !containsString(labelIDs, "label-managed") {
		t.Fatalf("labelIDs = %#v, want managed label preserved", labelIDs)
	}
	if len(labelIDs) != 3 {
		t.Fatalf("labelIDs len = %d, want 3", len(labelIDs))
	}
}

// TestDesiredLabelIDsManagedLabelEvictsExclusiveGroupSibling guards the fix for
// "labelIds not exclusive child labels": when the sync asserts a managed label
// that belongs to a Linear exclusive group, and the ticket already carries a
// different child of that same group, the carried-forward sibling must be
// dropped so the update names only one child of the group. The managed label
// wins.
func TestDesiredLabelIDsManagedLabelEvictsExclusiveGroupSibling(t *testing.T) {
	client := &Client{
		managedLabelIDs: map[string]string{
			"snyk-open-source": "label-oss",
		},
		managedLabelGroupIDs: map[string]string{
			"snyk-open-source": "group-tool",
		},
	}

	// The ticket already carries snyk-code, another child of the exclusive
	// "tool" group, applied by a human or a prior run. It is not in the sync's
	// managed set for this finding, so it would otherwise be carried forward
	// alongside the asserted snyk-open-source and Linear would reject the update.
	existing := model.ExistingIssue{
		Labels: []model.IssueLabel{
			{ID: "label-unrelated", Name: "customer-visible"},
			{ID: "label-code", Name: "snyk-code", GroupID: "group-tool"},
		},
	}
	desired := model.DesiredIssue{
		ManagedLabels: []string{"snyk-open-source"},
	}

	labelIDs, err := client.desiredLabelIDs(existing, desired, nil)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if containsString(labelIDs, "label-code") {
		t.Fatalf("labelIDs = %#v, want carried-forward group sibling 'snyk-code' evicted", labelIDs)
	}
	if !containsString(labelIDs, "label-oss") {
		t.Fatalf("labelIDs = %#v, want asserted managed group child 'snyk-open-source' present", labelIDs)
	}
	if !containsString(labelIDs, "label-unrelated") {
		t.Fatalf("labelIDs = %#v, want ungrouped label preserved", labelIDs)
	}
	if len(labelIDs) != 2 {
		t.Fatalf("labelIDs len = %d, want 2 (at most one child per exclusive group)", len(labelIDs))
	}
}

// TestDesiredLabelIDsDropsSecondCarriedForwardExclusiveSibling covers two
// children of the same exclusive group present on the ticket with neither in the
// sync's managed set — for example two labels applied before the group was made
// exclusive. Echoing both back fails the update, so the set keeps the first read
// and drops the rest.
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
// exclusive-group collapse must only fold labels that share a group. Children of
// different groups, and ungrouped labels, are all preserved.
func TestDesiredLabelIDsKeepsChildrenOfDistinctGroups(t *testing.T) {
	client := &Client{}

	existing := model.ExistingIssue{
		Labels: []model.IssueLabel{
			{ID: "label-tool", Name: "snyk-open-source", GroupID: "group-tool"},
			{ID: "label-origin", Name: "snyk-github", GroupID: "group-origin"},
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

func TestCreateIssuesRemovesActorAfterCreateWhenLinearAutoSubscribesThem(t *testing.T) {
	var requests []struct {
		Query     string
		Variables map[string]any
	}

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
				requests = append(requests, struct {
					Query     string
					Variables map[string]any
				}{
					Query:     payload.Query,
					Variables: payload.Variables,
				})

				switch {
				case strings.Contains(payload.Query, "mutation issueCreateBatch"):
					input0 := payload.Variables["input0"].(map[string]any)
					if subscriberIDs := input0["subscriberIds"]; !slices.Equal(anyStrings(subscriberIDs), []string{}) {
						t.Fatalf("create subscriberIds = %#v, want empty list", subscriberIDs)
					}
					return jsonResponse(t, `{"data":{"issueCreate0":{"success":true,"issue":{"id":"issue-1","identifier":"ENG-1"}}}}`), nil
				case strings.Contains(payload.Query, "mutation issueUnsubscribeBatch"):
					if got := payload.Variables["id0"]; got != "issue-1" {
						t.Fatalf("issueUnsubscribe id0 = %#v, want issue-1", got)
					}
					return jsonResponse(t, `{"data":{"issueUnsubscribe0":{"success":true}}}`), nil
				default:
					t.Fatalf("unexpected GraphQL query: %s", payload.Query)
					return nil, nil
				}
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam: "team-1",
		statesByName: map[string]string{"todo": "state-1"},
		statesByType: map[string]string{"unstarted": "state-1"},
	}

	failed, err := client.CreateIssues(t.Context(), []model.DesiredIssue{{
		Fingerprint: "snyk:project-1:issue-1",
		Title:       "Snyk: example",
		Description: "body",
		State:       model.StateTodo,
		Priority:    3,
	}})
	if err != nil {
		t.Fatalf("CreateIssues() error = %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("CreateIssues() failed indices = %v, want none", failed)
	}

	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func anyStrings(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			continue
		}
		out = append(out, s)
	}
	return out
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
				if strings.Contains(payload.Query, "query issueLabelsByID") {
					return jsonResponse(t, `{"data":{"issues":{"nodes":[{"id":"issue-1","labels":{"nodes":[{"id":"label-1","name":"snyk-automation"}]}}]}}}`), nil
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
			"snyk-automation": "label-1",
		},
	}

	err := client.UpdateIssues(t.Context(), []model.IssueUpdate{{
		Existing: model.ExistingIssue{
			ID:            "issue-1",
			Identifier:    "SEC-1",
			StateID:       "state-todo",
			StateName:     "Todo",
			ManagedLabels: []string{"snyk-automation"},
			Labels:        []model.IssueLabel{{ID: "label-1", Name: "snyk-automation"}},
		},
		Desired: model.DesiredIssue{
			Fingerprint:   "snyk:proj-1:issue-1",
			Title:         "updated title",
			Description:   "updated body",
			State:         model.StateTodo,
			PreserveState: true,
			ManagedLabels: []string{"snyk-automation"},
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
			ManagedLabels: []string{"snyk-automation"},
			Labels:        []model.IssueLabel{{ID: "l1", Name: "snyk-automation"}},
		},
		Desired: model.DesiredIssue{
			Title:         "new title",
			Description:   "new body",
			DueDate:       "2026-05-01",
			State:         model.StateBacklog,
			StateReason:   "Snyk reports this issue as ignored until a fix is available",
			DueDateReason: "high severity SLA: 30 days from issue creation",
			Priority:      1,
			ManagedLabels: []string{"snyk-automation", "snyk-code"},
			LabelReasons:  map[string]string{"snyk-code": "Snyk issue type is code"},
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
			LabelsAdded:        []string{"snyk-code"},
		},
	}

	comment := buildChangeComment(update)

	if comment == "" {
		t.Fatal("expected non-empty comment")
	}
	if !strings.Contains(comment, "**snyk-linear-sync**") {
		t.Fatalf("comment missing header: %s", comment)
	}
	// State comment explains why, not just what
	if !strings.Contains(comment, "Moved to **backlog** — Snyk reports this issue as ignored until a fix is available") {
		t.Fatalf("comment missing state change with reason: %s", comment)
	}
	// Due date comment explains the SLA basis
	if !strings.Contains(comment, "Due date set to **2026-05-01** — high severity SLA: 30 days from issue creation") {
		t.Fatalf("comment missing due date with reason: %s", comment)
	}
	// Description explains the driver
	if !strings.Contains(comment, "Description updated — Snyk finding data changed") {
		t.Fatalf("comment missing description reason: %s", comment)
	}
	// Title explains the driver
	if !strings.Contains(comment, "Title updated — Snyk finding data changed") {
		t.Fatalf("comment missing title reason: %s", comment)
	}
	// Priority uses human-readable name and explains the driver
	if !strings.Contains(comment, "Priority set to **Urgent** — Snyk severity changed") {
		t.Fatalf("comment missing priority with reason: %s", comment)
	}
	// Added label includes reason from LabelReasons
	if !strings.Contains(comment, "Added **snyk-code** — Snyk issue type is code") {
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

func TestBuildChangeCommentDueDateClearedWithReason(t *testing.T) {
	update := model.IssueUpdate{
		Existing: model.ExistingIssue{
			DueDate:   "2026-04-01",
			StateName: "Todo",
		},
		Desired: model.DesiredIssue{
			DueDate:       "",
			DueDateReason: "awaiting upstream fix, SLA paused",
			State:         model.StateBacklog,
		},
		Diff: &model.IssueDiff{
			DueDateChanged: true,
			DueDateFrom:    "2026-04-01",
			DueDateTo:      "",
		},
	}

	comment := buildChangeComment(update)
	if !strings.Contains(comment, "Due date cleared — awaiting upstream fix, SLA paused") {
		t.Fatalf("comment missing cleared due date with reason: %s", comment)
	}
}

func TestBuildChangeCommentRemovedLabels(t *testing.T) {
	update := model.IssueUpdate{
		Existing: model.ExistingIssue{
			ManagedLabels: []string{"snyk-automation", "triage-dependency"},
			Labels: []model.IssueLabel{
				{ID: "l1", Name: "snyk-automation"},
				{ID: "l2", Name: "triage-dependency"},
			},
			StateName: "Backlog",
		},
		Desired: model.DesiredIssue{
			State:         model.StateTodo,
			ManagedLabels: []string{"snyk-automation"},
		},
		Diff: &model.IssueDiff{
			StateChanged:     true,
			StateFrom:        "Backlog",
			StateTo:          "todo",
			LabelsRemoved:    []string{"triage-dependency"},
			LabelsNeedUpdate: true,
		},
	}

	comment := buildChangeComment(update)
	if !strings.Contains(comment, "Removed **triage-dependency** — no longer applicable") {
		t.Fatalf("comment missing removed label: %s", comment)
	}
}

func TestBuildChangeCommentStateChangeWithoutReason(t *testing.T) {
	update := model.IssueUpdate{
		Existing: model.ExistingIssue{
			StateName: "Todo",
		},
		Desired: model.DesiredIssue{
			State:       model.StateDone,
			StateReason: "", // no reason provided
		},
		Diff: &model.IssueDiff{
			StateChanged: true,
			StateFrom:    "Todo",
			StateTo:      "done",
		},
	}

	comment := buildChangeComment(update)
	if !strings.Contains(comment, "Moved to **done**") {
		t.Fatalf("comment missing state change: %s", comment)
	}
	// Should NOT contain an em-dash reason when reason is empty
	if strings.Contains(comment, "Moved to **done** —") {
		t.Fatalf("comment should not have reason suffix when empty: %s", comment)
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

// TestCreateIssuesPartialAliasFailureReturnsOnlyFailedIndices verifies that
// when one aliased issueCreate in a batched mutation reports success: false
// while its siblings succeed, CreateIssues reports only the failed index
// instead of an error for the whole batch. Treating a partial failure as a
// whole-batch failure made the caller recreate the already-created siblings,
// producing duplicate tickets.
func TestCreateIssuesPartialAliasFailureReturnsOnlyFailedIndices(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			States: config.StateConfig{Todo: "Todo"},
		},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return jsonResponse(t, `{"data":{
					"issueCreate0":{"success":true,"issue":{"id":"issue-1","identifier":"ENG-1"}},
					"issueCreate1":{"success":false,"issue":{"id":"","identifier":""}}
				}}`), nil
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam: "team-1",
		statesByName: map[string]string{"todo": "state-1"},
		statesByType: map[string]string{"unstarted": "state-1"},
	}

	failed, err := client.CreateIssues(t.Context(), []model.DesiredIssue{
		{
			Fingerprint: "snyk:project-1:issue-1",
			Title:       "Snyk: first",
			Description: "body",
			State:       model.StateTodo,
			Priority:    3,
		},
		{
			Fingerprint: "snyk:project-1:issue-2",
			Title:       "Snyk: second",
			Description: "body",
			State:       model.StateTodo,
			Priority:    3,
		},
	})
	if err != nil {
		t.Fatalf("CreateIssues() error = %v, want nil (partial failure is not a batch error)", err)
	}
	if len(failed) != 1 || failed[0] != 1 {
		t.Fatalf("CreateIssues() failed indices = %v, want [1]", failed)
	}
}

// TestCreateIssuesUnsubscribeFailureDoesNotFailCreates verifies that a
// failure in the best-effort post-create actor-unsubscribe step is not
// reported as a create failure. The issues were already created; returning
// an error here made the caller recreate them, producing duplicate tickets.
func TestCreateIssuesUnsubscribeFailureDoesNotFailCreates(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{
			TeamID:           "team-1",
			UnsubscribeActor: true,
			States:           config.StateConfig{Todo: "Todo"},
		},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll() error = %v", err)
				}
				payload := string(body)
				switch {
				case strings.Contains(payload, "issueCreateBatch"):
					return jsonResponse(t, `{"data":{"issueCreate0":{"success":true,"issue":{"id":"issue-1","identifier":"ENG-1"}}}}`), nil
				case strings.Contains(payload, "issueUnsubscribeBatch"):
					return jsonResponse(t, `{"errors":[{"message":"unsubscribe exploded"}]}`), nil
				default:
					t.Fatalf("unexpected GraphQL query: %s", payload)
					return nil, nil
				}
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam: "team-1",
		statesByName: map[string]string{"todo": "state-1"},
		statesByType: map[string]string{"unstarted": "state-1"},
	}

	failed, err := client.CreateIssues(t.Context(), []model.DesiredIssue{{
		Fingerprint: "snyk:project-1:issue-1",
		Title:       "Snyk: example",
		Description: "body",
		State:       model.StateTodo,
		Priority:    3,
	}})
	if err != nil {
		t.Fatalf("CreateIssues() error = %v, want nil (unsubscribe is best-effort)", err)
	}
	if len(failed) != 0 {
		t.Fatalf("CreateIssues() failed indices = %v, want none", failed)
	}
}

// TestPostCommentsPartialAliasFailureReturnsOnlyFailedIndices verifies the
// PostComments equivalent of the CreateIssues partial-failure contract: only
// the update whose commentCreate alias failed is reported, mapped back to
// its index in the updates slice (accounting for updates that produced no
// comment at all), so the caller does not re-post comments that already
// landed — re-posting spams duplicate notifications to subscribers.
func TestPostCommentsPartialAliasFailureReturnsOnlyFailedIndices(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{TeamID: "team-1"},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return jsonResponse(t, `{"data":{
					"commentCreate0":{"success":true,"comment":{"id":"comment-1"}},
					"commentCreate1":{"success":false,"comment":{"id":""}}
				}}`), nil
			}),
		}),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	updates := []model.IssueUpdate{
		{
			// No diff: produces no comment and must never appear in the
			// failed list, and must not shift the index mapping.
			Existing: model.ExistingIssue{ID: "existing-0", Identifier: "ENG-10"},
			Desired:  model.DesiredIssue{Fingerprint: "snyk:project-1:issue-10"},
		},
		{
			Existing: model.ExistingIssue{ID: "existing-1", Identifier: "ENG-11"},
			Desired:  model.DesiredIssue{Fingerprint: "snyk:project-1:issue-11"},
			Diff:     &model.IssueDiff{TitleChanged: true},
		},
		{
			Existing: model.ExistingIssue{ID: "existing-2", Identifier: "ENG-12"},
			Desired:  model.DesiredIssue{Fingerprint: "snyk:project-1:issue-12"},
			Diff:     &model.IssueDiff{TitleChanged: true},
		},
	}

	failed, err := client.PostComments(t.Context(), updates)
	if err != nil {
		t.Fatalf("PostComments() error = %v, want nil (partial failure is not a batch error)", err)
	}
	if len(failed) != 1 || failed[0] != 2 {
		t.Fatalf("PostComments() failed indices = %v, want [2] (the second comment maps to updates[2])", failed)
	}
}

// protectedTestLabels is the production default set: the three kikimora Dark
// Factory control labels this sync must never write on an existing ticket.
var protectedTestLabels = []string{
	"df:kikimora-snyk",
	"df:kikimora-snyk-complete",
	"df:kikimora-snyk-invalid",
}

func protectedLabelClient() *Client {
	return &Client{
		cfg: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed:   "snyk-automation",
				Protected: protectedTestLabels,
			},
		},
		managedLabelIDs: map[string]string{
			"snyk-automation":  "label-automation",
			"df:kikimora-snyk": "label-kiki",
		},
	}
}

// TestDesiredLabelIDsPreservesProtectedLabelAddedSinceSnapshot is the
// SNYK-54304 regression. kikimora added df:kikimora-snyk after this run took
// its board snapshot, so the label is absent from existing.Labels. Because
// issueUpdate replaces the whole label set, echoing the snapshot back deleted
// the label. The live read is the authority for protected labels, so it must
// survive.
func TestDesiredLabelIDsPreservesProtectedLabelAddedSinceSnapshot(t *testing.T) {
	client := protectedLabelClient()

	existing := model.ExistingIssue{
		ManagedLabels: []string{"snyk-automation"},
		Labels: []model.IssueLabel{
			{ID: "label-automation", Name: "snyk-automation"},
			{ID: "label-security", Name: "Security-Discovery"},
		},
	}
	desired := model.DesiredIssue{ManagedLabels: []string{"snyk-automation"}}
	live := []model.IssueLabel{
		{ID: "label-automation", Name: "snyk-automation"},
		{ID: "label-security", Name: "Security-Discovery"},
		{ID: "label-kiki", Name: "df:kikimora-snyk"},
	}

	labelIDs, err := client.desiredLabelIDs(existing, desired, live)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if !containsString(labelIDs, "label-kiki") {
		t.Fatalf("labelIDs = %#v, want the protected df:kikimora-snyk label preserved", labelIDs)
	}
	if !containsString(labelIDs, "label-security") {
		t.Fatalf("labelIDs = %#v, want the unrelated label preserved", labelIDs)
	}
	if !containsString(labelIDs, "label-automation") {
		t.Fatalf("labelIDs = %#v, want the managed label present", labelIDs)
	}
}

// TestDesiredLabelIDsDoesNotReAddProtectedLabelRemovedSinceSnapshot is the
// other direction: kikimora's DONE handling removes df:kikimora-snyk (swapping
// it for -complete). The snapshot still shows the old label, and re-asserting
// it would undo kikimora's swap. Protection means "never write", not "always
// keep".
func TestDesiredLabelIDsDoesNotReAddProtectedLabelRemovedSinceSnapshot(t *testing.T) {
	client := protectedLabelClient()

	existing := model.ExistingIssue{
		ManagedLabels: []string{"snyk-automation"},
		Labels: []model.IssueLabel{
			{ID: "label-automation", Name: "snyk-automation"},
			{ID: "label-kiki", Name: "df:kikimora-snyk"},
		},
	}
	desired := model.DesiredIssue{ManagedLabels: []string{"snyk-automation"}}
	live := []model.IssueLabel{
		{ID: "label-automation", Name: "snyk-automation"},
		{ID: "label-complete", Name: "df:kikimora-snyk-complete"},
	}

	labelIDs, err := client.desiredLabelIDs(existing, desired, live)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if containsString(labelIDs, "label-kiki") {
		t.Fatalf("labelIDs = %#v, want the removed protected label left off", labelIDs)
	}
	if !containsString(labelIDs, "label-complete") {
		t.Fatalf("labelIDs = %#v, want the live protected label preserved", labelIDs)
	}
}

// TestDesiredLabelIDsNeverAssertsProtectedLabelFromManagedSet guards the
// blast radius of a misconfiguration: even if a protected label somehow lands
// in the managed set, an update must not stamp it onto a ticket that does not
// carry it. Adding df:kikimora-snyk-complete to a live finding would terminally
// gate it and silently stop it ever being assessed.
func TestDesiredLabelIDsNeverAssertsProtectedLabelFromManagedSet(t *testing.T) {
	client := protectedLabelClient()

	existing := model.ExistingIssue{Labels: []model.IssueLabel{}}
	desired := model.DesiredIssue{
		ManagedLabels: []string{"snyk-automation", "df:kikimora-snyk"},
	}

	labelIDs, err := client.desiredLabelIDs(existing, desired, nil)
	if err != nil {
		t.Fatalf("desiredLabelIDs() error = %v", err)
	}
	if containsString(labelIDs, "label-kiki") {
		t.Fatalf("labelIDs = %#v, want no protected label asserted from the managed set", labelIDs)
	}
	if !containsString(labelIDs, "label-automation") {
		t.Fatalf("labelIDs = %#v, want the managed label present", labelIDs)
	}
}

// TestCreateLabelIDsIncludesCreateOnlyLabels covers the one place a protected
// label is legitimately written: issue creation. df:kikimora-snyk is stamped at
// birth so kikimora's watcher can enrol the finding without doing a write of
// its own.
func TestCreateLabelIDsIncludesCreateOnlyLabels(t *testing.T) {
	client := protectedLabelClient()

	labelIDs := client.createLabelIDs(model.DesiredIssue{
		ManagedLabels:    []string{"snyk-automation"},
		CreateOnlyLabels: []string{"df:kikimora-snyk"},
	})

	if !containsString(labelIDs, "label-kiki") {
		t.Fatalf("labelIDs = %#v, want the create-only label applied on create", labelIDs)
	}
	if !containsString(labelIDs, "label-automation") {
		t.Fatalf("labelIDs = %#v, want the managed label applied on create", labelIDs)
	}
}

// TestCreateLabelNamesKeepsCreateOnlyLabelsWhenManagementOff pins the sentence
// README and PROJECT.md state next to the LINEAR_MANAGED_LABEL=off description:
// turning label management off empties the managed set but not the create-only
// set, because LINEAR_CREATE_ONLY_LABELS is configured separately. Those labels
// therefore still resolve on create, and must still exist in Linear.
func TestCreateLabelNamesKeepsCreateOnlyLabelsWhenManagementOff(t *testing.T) {
	// The feature under test is createLabelNames, which takes no LabelConfig; a
	// desired issue built with an empty managed set is exactly what a run with
	// management off produces.
	desired := model.DesiredIssue{
		ManagedLabels:    nil,
		CreateOnlyLabels: []string{"df:kikimora-snyk"},
	}

	got := createLabelNames(desired)
	if !slices.Equal(got, []string{"df:kikimora-snyk"}) {
		t.Fatalf("createLabelNames() = %#v, want only the create-only label", got)
	}
	if names := managedLabelsFromDesiredIssues([]model.DesiredIssue{desired}); !slices.Contains(names, "df:kikimora-snyk") {
		t.Fatalf("managedLabelsFromDesiredIssues() = %#v, want the create-only label to still be resolved", names)
	}
}

// TestUpdateIssuesFailsWhenLiveLabelsUnreadable pins the fail-closed path. An
// issue missing from the live label read is indistinguishable from one carrying
// no protected label, so the update must not proceed — guessing wrong deletes a
// kikimora control label. The caller retries the batch item by item.
func TestUpdateIssuesFailsWhenLiveLabelsUnreadable(t *testing.T) {
	mutated := false
	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			States: config.StateConfig{Todo: "Todo"},
			Labels: config.LabelConfig{Protected: protectedTestLabels},
		},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query string `json:"query"`
				}
				body, _ := io.ReadAll(req.Body)
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				if strings.Contains(payload.Query, "query issueLabelsByID") {
					// The issue is absent from the response.
					return jsonResponse(t, `{"data":{"issues":{"nodes":[]}}}`), nil
				}
				if strings.Contains(payload.Query, "mutation issueUpdateBatch") {
					mutated = true
					return jsonResponse(t, `{"data":{"issueUpdate0":{"success":true}}}`), nil
				}
				t.Fatalf("unexpected GraphQL query: %s", payload.Query)
				return nil, nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam:    "team-1",
		statesByName:    map[string]string{"todo": "state-1"},
		statesByType:    map[string]string{"unstarted": "state-1"},
		managedLabelIDs: map[string]string{},
	}

	err := client.UpdateIssues(t.Context(), []model.IssueUpdate{{
		Existing: model.ExistingIssue{ID: "issue-1", Identifier: "SNYK-1"},
		Desired: model.DesiredIssue{
			Fingerprint: "snyk:proj-1:issue-1",
			Title:       "Snyk: title",
			Description: "body",
			State:       model.StateTodo,
		},
	}})
	if err == nil {
		t.Fatal("UpdateIssues() error = nil, want a failure when live labels cannot be read")
	}
	if mutated {
		t.Fatal("UpdateIssues() sent an update mutation despite being unable to preserve protected labels")
	}
}

// TestUpdateIssuesRefusesUpdateWhenLabelPageTruncated covers the pagination
// edge of the live label read. The labels connection can truncate just like
// any other connection, and a truncated page is not a complete label set: a
// label past the page boundary would be absent from the read, and the update
// would then delete it — the exact failure the read exists to prevent. So a
// hasNextPage issue must be treated as unreadable and fail the batch, not be
// updated from a partial set. The other batch item must not be corrupted
// either: nothing is written while the refusal stands, and the caller's
// per-item fallback decides what lands.
func TestUpdateIssuesRefusesUpdateWhenLabelPageTruncated(t *testing.T) {
	mutated := false
	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			States: config.StateConfig{Todo: "Todo"},
			Labels: config.LabelConfig{Protected: []string{"control:protected"}},
		},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query string `json:"query"`
				}
				body, _ := io.ReadAll(req.Body)
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				if strings.Contains(payload.Query, "query issueLabelsByID") {
					// issue-1's labels do not fit one page, so a label the
					// sync does not own may be on a page this read never saw;
					// issue-2's page is complete.
					return jsonResponse(t, `{"data":{"issues":{"nodes":[
						{"id":"issue-1","labels":{
							"nodes":[{"id":"label-ordinary","name":"customer-visible"}],
							"pageInfo":{"hasNextPage":true}
						}},
						{"id":"issue-2","labels":{
							"nodes":[{"id":"label-protected","name":"control:protected"}]
						}}
					]}}}`), nil
				}
				if strings.Contains(payload.Query, "mutation issueUpdateBatch") {
					mutated = true
					return jsonResponse(t, `{"data":{"issueUpdate0":{"success":true}}}`), nil
				}
				t.Fatalf("unexpected GraphQL query: %s", payload.Query)
				return nil, nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam:    "team-1",
		statesByName:    map[string]string{"todo": "state-1"},
		statesByType:    map[string]string{"unstarted": "state-1"},
		managedLabelIDs: map[string]string{},
	}

	updates := []model.IssueUpdate{
		{
			Existing: model.ExistingIssue{ID: "issue-1", Identifier: "SNYK-1"},
			Desired: model.DesiredIssue{
				Fingerprint: "snyk:proj-1:issue-1",
				Title:       "Snyk: title",
				Description: "body",
				State:       model.StateTodo,
			},
		},
		{
			Existing: model.ExistingIssue{ID: "issue-2", Identifier: "SNYK-2"},
			Desired: model.DesiredIssue{
				Fingerprint: "snyk:proj-1:issue-2",
				Title:       "Snyk: other title",
				Description: "other body",
				State:       model.StateTodo,
			},
		},
	}

	err := client.UpdateIssues(t.Context(), updates)
	if err == nil {
		t.Fatal("UpdateIssues() error = nil, want a refusal when the live label page is truncated")
	}
	if !strings.Contains(err.Error(), "SNYK-1") {
		t.Fatalf("UpdateIssues() error = %v, want it to name the truncated issue", err)
	}
	if mutated {
		t.Fatal("UpdateIssues() sent an update mutation from a truncated label page; a label past the page boundary would be deleted")
	}
}

// TestFetchLabelsByIssueIDRequestsLabelPageInfo pins the query shape: without
// pageInfo on the nested labels connection there is no way to tell a complete
// label set from a truncated one.
func TestFetchLabelsByIssueIDRequestsLabelPageInfo(t *testing.T) {
	var capturedQuery string
	client := &Client{
		cfg: config.LinearConfig{TeamID: "team-1"},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query string `json:"query"`
				}
				body, _ := io.ReadAll(req.Body)
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				capturedQuery = payload.Query
				return jsonResponse(t, `{"data":{"issues":{"nodes":[]}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam:    "team-1",
		statesByName:    map[string]string{},
		statesByType:    map[string]string{},
		managedLabelIDs: map[string]string{},
	}

	if _, err := client.fetchLabelsByIssueID(t.Context(), []string{"issue-1"}); err != nil {
		t.Fatalf("fetchLabelsByIssueID() error = %v", err)
	}
	if !strings.Contains(capturedQuery, "hasNextPage") {
		t.Fatalf("live label query must request the labels pageInfo, got:\n%s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, fmt.Sprintf("labels(first: %d)", maxLiveLabelPage)) {
		t.Fatalf("live label query must request %d labels per issue, got:\n%s", maxLiveLabelPage, capturedQuery)
	}
}

// TestLoadSnapshotRefusesSnapshotWhenLabelPageTruncated covers the pagination
// edge of the snapshot's labels connection. A ticket carrying more labels than
// one page returns a truncated set, and the snapshot feeds both the cache hash
// and full-set rebuilds: a managed label past the page boundary — one removed
// by hand, say — would read as absent before and after, so the cache fast path
// would suppress re-asserting it indefinitely, while a rebuild from the partial
// snapshot would delete the labels it never saw. So a truncating snapshot must
// fail closed, not be trusted as complete.
func TestLoadSnapshotRefusesSnapshotWhenLabelPageTruncated(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "11111111-1111-1111-1111-111111111111",
		},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query string `json:"query"`
				}
				body, _ := io.ReadAll(req.Body)
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				if strings.Contains(payload.Query, "query teamStates") {
					return jsonResponse(t, `{"data":{"team":{"states":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`), nil
				}
				if strings.Contains(payload.Query, "query existingIssues") {
					// The ticket's labels do not fit one page, so the
					// snapshot would be a partial label set.
					return jsonResponse(t, `{"data":{"issues":{"nodes":[
						{"id":"issue-1","identifier":"SNYK-1","title":"Snyk: title","state":{"id":"state-1","name":"Todo"},"labels":{
							"nodes":[{"id":"label-1","name":"customer-visible"}],
							"pageInfo":{"hasNextPage":true}
						}}
					],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`), nil
				}
				t.Fatalf("unexpected GraphQL query: %s", payload.Query)
				return nil, nil
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName: map[string]string{},
		statesByType: map[string]string{},
	}

	_, err := client.LoadSnapshot(t.Context())
	if err == nil {
		t.Fatal("LoadSnapshot() error = nil, want a refusal when the snapshot's label page is truncated")
	}
	if !strings.Contains(err.Error(), "SNYK-1") {
		t.Fatalf("LoadSnapshot() error = %v, want it to name the truncated issue", err)
	}
}

// TestLoadIssueByIdentifierRefusesLookupWhenLabelPageTruncated covers the same
// pagination edge in the single-issue lookup. Diagnostic and reconciliation
// paths hash the labels this lookup returns, so a truncated page would read a
// hand-removed managed label as absent both before and after; the lookup must
// fail closed rather than return a partial set.
func TestLoadIssueByIdentifierRefusesLookupWhenLabelPageTruncated(t *testing.T) {
	client := &Client{
		cfg: config.LinearConfig{TeamID: "team-1"},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query string `json:"query"`
				}
				body, _ := io.ReadAll(req.Body)
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				if !strings.Contains(payload.Query, "query issueByIdentifier") {
					t.Fatalf("unexpected GraphQL query: %s", payload.Query)
				}
				// The ticket's labels do not fit one page, so the lookup
				// would return a partial label set.
				return jsonResponse(t, `{"data":{"issues":{"nodes":[
					{"id":"issue-1","identifier":"SNYK-1","title":"Snyk: title","state":{"id":"state-1","name":"Todo"},"labels":{
						"nodes":[{"id":"label-1","name":"customer-visible"}],
						"pageInfo":{"hasNextPage":true}
					}}
				]}}}`), nil
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		statesByName: map[string]string{},
		statesByType: map[string]string{},
	}

	_, err := client.LoadIssueByIdentifier(t.Context(), "SNYK-1")
	if err == nil {
		t.Fatal("LoadIssueByIdentifier() error = nil, want a refusal when the label page is truncated")
	}
	if !strings.Contains(err.Error(), "SNYK-1") {
		t.Fatalf("LoadIssueByIdentifier() error = %v, want it to name the truncated issue", err)
	}
}

// TestSnapshotAndLookupQueriesRequestLabelPageInfo pins the query shape of the
// snapshot and single-issue lookups: without pageInfo on the nested labels
// connection there is no way to tell a complete label set from a truncated
// one, and a partial set would be trusted as complete.
func TestSnapshotAndLookupQueriesRequestLabelPageInfo(t *testing.T) {
	fetch := func(t *testing.T, run func(c *Client) error) string {
		t.Helper()
		var capturedQuery string
		client := &Client{
			cfg: config.LinearConfig{TeamID: "11111111-1111-1111-1111-111111111111"},
			gql: gqlclient.New("http://linear.test/graphql", &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					var payload struct {
						Query string `json:"query"`
					}
					body, _ := io.ReadAll(req.Body)
					if err := json.Unmarshal(body, &payload); err != nil {
						t.Fatalf("json.Unmarshal() error = %v", err)
					}
					if !strings.Contains(payload.Query, "query teamStates") {
						capturedQuery = payload.Query
					}
					// One ticket whose labels fit one page: the shape test
					// only pins the query text, and the lookup needs a node
					// to reach the return path.
					return jsonResponse(t, `{"data":{"issues":{"nodes":[
						{"id":"issue-1","identifier":"SNYK-1","title":"Snyk: title","state":{"id":"state-1","name":"Todo"},"labels":{
							"nodes":[],
							"pageInfo":{"hasNextPage":false}
						}}
					]}}}`), nil
				}),
			}),
			log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
			statesByName: map[string]string{},
			statesByType: map[string]string{},
		}
		if err := run(client); err != nil {
			t.Fatalf("query errored: %v", err)
		}
		if capturedQuery == "" {
			t.Fatal("no issue query captured")
		}
		return capturedQuery
	}

	snapshotQuery := fetch(t, func(c *Client) error {
		_, err := c.LoadSnapshot(t.Context())
		return err
	})
	lookupQuery := fetch(t, func(c *Client) error {
		_, err := c.LoadIssueByIdentifier(t.Context(), "SNYK-1")
		return err
	})

	// The snapshot query also carries the issues-level pageInfo, so it must
	// request hasNextPage twice: once for the labels connection and once for
	// the issues connection. The lookup has no issues-level pageInfo.
	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"snapshot", snapshotQuery, 2},
		{"lookup", lookupQuery, 1},
	} {
		if got := strings.Count(tc.query, "hasNextPage"); got != tc.want {
			t.Fatalf("%s query requests hasNextPage %d times, want %d (the labels connection must request pageInfo):\n%s", tc.name, got, tc.want, tc.query)
		}
	}
	if !strings.Contains(lookupQuery, fmt.Sprintf("labels(first: %d)", maxLiveLabelPage)) {
		t.Fatalf("lookup query must request %d labels per issue, got:\n%s", maxLiveLabelPage, lookupQuery)
	}
}

// TestUpdateIssuesAlwaysReadsLiveLabels pins that the live label read runs for
// every update batch, not only when protected labels are configured. The read
// is what makes the live set — rather than the run's opening snapshot — the
// authority for which labels the ticket carries, so an unmanaged label added by
// a person or another automation since the snapshot is not clobbered.
func TestUpdateIssuesAlwaysReadsLiveLabels(t *testing.T) {
	labelRead := false
	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			States: config.StateConfig{Todo: "Todo"},
			// No protected labels configured.
		},
		gql: gqlclient.New("http://linear.test/graphql", &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload struct {
					Query string `json:"query"`
				}
				body, _ := io.ReadAll(req.Body)
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("json.Unmarshal() error = %v", err)
				}
				if strings.Contains(payload.Query, "query issueLabelsByID") {
					labelRead = true
					return jsonResponse(t, `{"data":{"issues":{"nodes":[{"id":"issue-1","labels":{"nodes":[]}}]}}}`), nil
				}
				return jsonResponse(t, `{"data":{"issueUpdate0":{"success":true}}}`), nil
			}),
		}),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam:    "team-1",
		statesByName:    map[string]string{"todo": "state-1"},
		statesByType:    map[string]string{"unstarted": "state-1"},
		managedLabelIDs: map[string]string{},
	}

	err := client.UpdateIssues(t.Context(), []model.IssueUpdate{{
		Existing: model.ExistingIssue{ID: "issue-1", Identifier: "SNYK-1"},
		Desired: model.DesiredIssue{
			Fingerprint: "snyk:proj-1:issue-1",
			Title:       "Snyk: title",
			Description: "body",
			State:       model.StateTodo,
		},
	}})
	if err != nil {
		t.Fatalf("UpdateIssues() error = %v", err)
	}
	if !labelRead {
		t.Fatal("UpdateIssues() did not read live labels; an unmanaged label added since the snapshot would be clobbered")
	}
}

// TestUpdateIssuesPreservesUnmanagedLabelAddedSinceSnapshot is the EXP-7834
// regression (observed on SNYK-59893). A person added the unmanaged
// Security-Scale label after this run took its board snapshot, so the label is
// absent from existing.Labels. Because issueUpdate replaces the whole label set,
// echoing the snapshot back deleted it. The live read taken immediately before
// the write is the authority, so the label must survive the update.
func TestUpdateIssuesPreservesUnmanagedLabelAddedSinceSnapshot(t *testing.T) {
	var capturedInput map[string]any
	client := &Client{
		cfg: config.LinearConfig{
			TeamID: "team-1",
			States: config.StateConfig{Todo: "Todo"},
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
				if strings.Contains(payload.Query, "query issueLabelsByID") {
					// Live read: the ticket now carries Security-Scale, which the
					// opening snapshot below never saw.
					return jsonResponse(t, `{"data":{"issues":{"nodes":[{"id":"issue-1","labels":{"nodes":[{"id":"label-automation","name":"snyk-automation"},{"id":"label-scale","name":"Security-Scale"}]}}]}}}`), nil
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
		managedLabelIDs: map[string]string{
			"snyk-automation": "label-automation",
		},
	}

	err := client.UpdateIssues(t.Context(), []model.IssueUpdate{{
		Existing: model.ExistingIssue{
			ID:            "issue-1",
			Identifier:    "SNYK-59893",
			ManagedLabels: []string{"snyk-automation"},
			// Opening snapshot: no Security-Scale yet.
			Labels: []model.IssueLabel{{ID: "label-automation", Name: "snyk-automation"}},
		},
		Desired: model.DesiredIssue{
			Fingerprint:   "snyk:proj-1:issue-1",
			Title:         "Snyk: updated title",
			Description:   "updated body",
			State:         model.StateTodo,
			ManagedLabels: []string{"snyk-automation"},
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
		t.Fatalf("update mutation must include labelIds, got: %#v", capturedInput)
	}
	arr, ok := rawLabelIds.([]any)
	if !ok {
		t.Fatalf("labelIds must be an array, got: %#v", rawLabelIds)
	}
	ids := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			ids = append(ids, s)
		}
	}
	if !containsString(ids, "label-scale") {
		t.Fatalf("labelIds = %#v, want the unmanaged Security-Scale label (label-scale) preserved", ids)
	}
	if !containsString(ids, "label-automation") {
		t.Fatalf("labelIds = %#v, want the managed snyk-automation label present", ids)
	}
}

func TestExtractIdentityAndClosedReason(t *testing.T) {
	for _, tc := range []struct {
		name        string
		description string
	}{
		{
			name:        "as written",
			description: "body\n\n<!-- snyk-linear-sync\nfingerprint: snyk:p:i:pkg@1\nidentity: 0123456789abcdef0123456789abcdef\nmanaged_labels: snyk-automation\nclosed_reason: project-missing\n-->",
		},
		{
			name:        "escaped by Linear markdown normalization",
			description: "body\n\n<!-- snyk-linear-sync\nfingerprint: snyk:p:i:pkg@1\nidentity: 0123456789abcdef0123456789abcdef\nmanaged\\_labels: snyk-automation\nclosed\\_reason: project\\-missing\n-->",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractIdentity(tc.description); got != "0123456789abcdef0123456789abcdef" {
				t.Fatalf("extractIdentity() = %q", got)
			}
			if got := extractClosedReason(tc.description); got != model.ClosedReasonProjectMissing {
				t.Fatalf("extractClosedReason() = %q", got)
			}
		})
	}
	legacy := "<!-- snyk-linear-sync\nfingerprint: snyk:p:i\n-->"
	if extractIdentity(legacy) != "" || extractClosedReason(legacy) != "" {
		t.Fatalf("legacy block must yield no identity or closed_reason")
	}
	// Lines outside the metadata block are never read.
	outside := "identity: spoofed\nclosed_reason: project-missing\n<!-- snyk-linear-sync\nfingerprint: snyk:p:i\n-->"
	if extractIdentity(outside) != "" || extractClosedReason(outside) != "" {
		t.Fatalf("metadata lines outside the block must be ignored")
	}
}

func TestLinearIssueToModelLoadsTimestamps(t *testing.T) {
	created := "2026-08-01T10:00:00Z"
	completed := "2026-09-01T10:00:00Z"
	canceled := "2026-09-05T10:00:00Z"
	node := linearIssueNode{ID: "id", CreatedAt: &created, CompletedAt: &completed}
	issue := linearIssueToModel(node)
	if issue.CreatedAt == nil || issue.CreatedAt.Format(time.RFC3339) != created {
		t.Fatalf("CreatedAt = %v, want %s", issue.CreatedAt, created)
	}
	if issue.ClosedAt == nil || issue.ClosedAt.Format(time.RFC3339) != completed {
		t.Fatalf("ClosedAt = %v, want completedAt %s", issue.ClosedAt, completed)
	}

	// Done then Cancelled: the later timestamp is the current closure.
	node.CanceledAt = &canceled
	if issue := linearIssueToModel(node); issue.ClosedAt == nil || issue.ClosedAt.Format(time.RFC3339) != canceled {
		t.Fatalf("ClosedAt = %v, want canceledAt %s", issue.ClosedAt, canceled)
	}

	bad := "not-a-time"
	if issue := linearIssueToModel(linearIssueNode{CompletedAt: &bad}); issue.ClosedAt != nil || issue.CreatedAt != nil {
		t.Fatalf("unparsable/missing timestamps must be nil, got closed=%v created=%v", issue.ClosedAt, issue.CreatedAt)
	}
}

func TestBuildChangeCommentSkipsMetadataOnlyDescriptionChange(t *testing.T) {
	update := model.IssueUpdate{
		Diff: &model.IssueDiff{DescriptionChanged: true, MetadataOnlyDescriptionChange: true},
	}
	if comment := buildChangeComment(update); comment != "" {
		t.Fatalf("expected no comment for a metadata-only change, got: %s", comment)
	}
	update.Diff.StateChanged = true
	update.Diff.StateTo = "todo"
	update.Desired.StateReason = "Snyk still reports this finding as open; reopened instead of creating a duplicate"
	comment := buildChangeComment(update)
	if strings.Contains(comment, "Description updated") || !strings.Contains(comment, "reopened instead of creating a duplicate") {
		t.Fatalf("unexpected comment: %s", comment)
	}
}

// historyClient returns a client whose transport answers issueStateHistory
// queries from pages, keyed by issue ID and then by the request's "after"
// cursor ("" for the first page). An issue missing from pages gets a GraphQL
// error.
func historyClient(t *testing.T, pages map[string]map[string]string, requests *[]map[string]any) *Client {
	t.Helper()
	return &Client{
		cfg: config.LinearConfig{TeamID: "team-1"},
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
				if !strings.Contains(payload.Query, "query issueStateHistory") {
					t.Fatalf("unexpected GraphQL query: %s", payload.Query)
				}
				*requests = append(*requests, payload.Variables)
				id, _ := payload.Variables["id"].(string)
				after, _ := payload.Variables["after"].(string)
				page, ok := pages[id][after]
				if !ok {
					return jsonResponse(t, `{"errors":[{"message":"Entity not found"}]}`), nil
				}
				return jsonResponse(t, page), nil
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam: "team-1",
		statesByName: map[string]string{"triage": "s-triage", "todo": "s-todo", "in progress": "s-progress", "done": "s-done", "canceled": "s-canceled"},
		statesByType: map[string]string{"triage": "s-triage", "unstarted": "s-todo", "started": "s-progress", "completed": "s-done", "canceled": "s-canceled"},
		statesByID: map[string]workflowState{
			"s-triage":   {name: "Triage", stateType: "triage"},
			"s-todo":     {name: "Todo", stateType: "unstarted"},
			"s-progress": {name: "In Progress", stateType: "started"},
			"s-done":     {name: "Done", stateType: "completed"},
			"s-canceled": {name: "Canceled", stateType: "canceled"},
		},
	}
}

func historyPage(hasNext bool, cursor string, nodes ...string) string {
	return fmt.Sprintf(`{"data":{"issue":{"history":{"nodes":[%s],"pageInfo":{"hasNextPage":%t,"endCursor":%q}}}}}`, strings.Join(nodes, ","), hasNext, cursor)
}

func historyNode(at, from, to string) string {
	field := func(v string) string {
		if v == "" {
			return "null"
		}
		return strconv.Quote(v)
	}
	return fmt.Sprintf(`{"createdAt":%q,"fromStateId":%s,"toStateId":%s}`, at, field(from), field(to))
}

func TestLastOpenStatesReturnsStateBeforeClosure(t *testing.T) {
	pages := map[string]map[string]string{
		// Triaged to Todo, then cancelled by the sync when its project was
		// deactivated. Linear lists history newest first; a description
		// update carries no state change.
		"triaged": {"": historyPage(false, "",
			historyNode("2026-10-08T13:01:52.014Z", "s-todo", "s-canceled"),
			historyNode("2026-10-07T09:00:00Z", "", ""),
			historyNode("2026-10-05T05:22:51.308Z", "s-triage", "s-todo"),
		)},
		// Created in Triage and cancelled straight away: Linear records no
		// entry for the initial state, only the transition out of it.
		"untriaged": {"": historyPage(false, "",
			historyNode("2026-10-08T13:01:52Z", "s-triage", "s-canceled"),
		)},
		// In progress, fixed (Done), then moved to Canceled; the last open
		// state is still In Progress. Spread over two pages, oldest first.
		"paged": {
			"": historyPage(true, "cursor-1",
				historyNode("2026-09-01T00:00:00Z", "s-triage", "s-todo"),
				historyNode("2026-09-02T00:00:00Z", "s-todo", "s-progress"),
			),
			"cursor-1": historyPage(false, "",
				historyNode("2026-09-03T00:00:00Z", "s-progress", "s-done"),
				historyNode("2026-10-08T00:00:00Z", "s-done", "s-canceled"),
			),
		},
		// Moved in from another team whose states this team cannot use, and
		// closed straight away.
		"foreign": {"": historyPage(false, "",
			historyNode("2026-09-01T00:00:00Z", "other-team-todo", "s-canceled"),
		)},
		"no-changes": {"": historyPage(false, "", historyNode("2026-09-01T00:00:00Z", "", ""))},
	}
	var requests []map[string]any
	client := historyClient(t, pages, &requests)

	got, err := client.LastOpenStates(t.Context(), []string{"triaged", "untriaged", "paged", "foreign", "no-changes"})
	if err != nil {
		t.Fatalf("LastOpenStates() error = %v", err)
	}
	want := map[string]model.WorkflowState{
		"triaged":   {ID: "s-todo", Name: "Todo"},
		"untriaged": {ID: "s-triage", Name: "Triage"},
		"paged":     {ID: "s-progress", Name: "In Progress"},
	}
	if len(got) != len(want) {
		t.Fatalf("LastOpenStates() = %+v, want %+v", got, want)
	}
	for id, state := range want {
		if got[id] != state {
			t.Fatalf("LastOpenStates()[%s] = %+v, want %+v", id, got[id], state)
		}
	}
	if len(requests) != 6 || requests[3]["id"] != "paged" || requests[3]["after"] != "cursor-1" {
		t.Fatalf("requests = %v, want one per issue plus a second page for paged", requests)
	}
}

func TestLastOpenStatesKeepsReadableIssuesWhenOneFails(t *testing.T) {
	pages := map[string]map[string]string{
		"ok": {"": historyPage(false, "", historyNode("2026-10-08T00:00:00Z", "s-todo", "s-canceled"))},
	}
	var requests []map[string]any
	client := historyClient(t, pages, &requests)

	got, err := client.LastOpenStates(t.Context(), []string{"missing", "ok"})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v, want one naming the unreadable issue", err)
	}
	if len(got) != 1 || got["ok"] != (model.WorkflowState{ID: "s-todo", Name: "Todo"}) {
		t.Fatalf("LastOpenStates() = %+v, want the readable issue", got)
	}
}

func TestUpdateIssuesMovesReopenToRestoredState(t *testing.T) {
	var input0 map[string]any
	client := &Client{
		cfg: config.LinearConfig{TeamID: "team-1", States: config.StateConfig{Todo: "Triage"}},
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
				switch {
				case strings.Contains(payload.Query, "query issueLabelsByID"):
					return jsonResponse(t, `{"data":{"issues":{"nodes":[{"id":"issue-1","labels":{"nodes":[]}}]}}}`), nil
				case strings.Contains(payload.Query, "mutation issueUpdateBatch"):
					input0, _ = payload.Variables["input0"].(map[string]any)
					return jsonResponse(t, `{"data":{"issueUpdate0":{"success":true}}}`), nil
				}
				t.Fatalf("unexpected GraphQL query: %s", payload.Query)
				return nil, nil
			}),
		}),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolvedTeam: "team-1",
		statesByName: map[string]string{"triage": "s-triage", "todo": "s-todo"},
		statesByType: map[string]string{"triage": "s-triage", "unstarted": "s-todo"},
	}

	err := client.UpdateIssues(t.Context(), []model.IssueUpdate{{
		Existing: model.ExistingIssue{ID: "issue-1", Identifier: "SEC-1", StateName: "Canceled"},
		Desired: model.DesiredIssue{
			Fingerprint:  "snyk:proj-1:issue-1",
			State:        model.StateTodo,
			Reopen:       true,
			RestoreState: model.WorkflowState{ID: "s-todo", Name: "Todo"},
		},
	}})
	if err != nil {
		t.Fatalf("UpdateIssues() error = %v", err)
	}
	if input0["stateId"] != "s-todo" {
		t.Fatalf("stateId = %v, want the restored state s-todo rather than the configured Triage", input0["stateId"])
	}
}

// e2eSnykFindings is the Snyk side of the end-to-end ambiguous-create tests:
// two open findings for one project, so the run plans exactly one create
// batch of two.
type e2eSnykFindings struct{}

func (e2eSnykFindings) LoadSnapshot(context.Context) (model.SnykSnapshot, error) {
	return model.SnykSnapshot{
		Findings: []model.Finding{
			{
				Fingerprint: "snyk:project-a:issue-1",
				SnykIssueID: "issue-1",
				ProjectID:   "project-a",
				ProjectName: "Project A",
				IssueTitle:  "Outdated package",
				Severity:    "high",
				Status:      model.FindingOpen,
				IssueURL:    "https://example.test/issue-1",
				CreatedAt:   time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC),
			},
			{
				Fingerprint: "snyk:project-a:issue-2",
				SnykIssueID: "issue-2",
				ProjectID:   "project-a",
				ProjectName: "Project A",
				IssueTitle:  "Vulnerable dependency",
				Severity:    "high",
				Status:      model.FindingOpen,
				IssueURL:    "https://example.test/issue-2",
				CreatedAt:   time.Date(2026, time.October, 1, 9, 30, 0, 0, time.UTC),
			},
		},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}, nil
}

// hasTitlePrefixArm reports whether the decoded IssueFilter carries the
// title-prefix identity arm inside its AND/OR structure.
func hasTitlePrefixArm(filterValue any) bool {
	return hasIdentityArm(filterValue, func(clause map[string]any) bool {
		title, ok := clause["title"].(map[string]any)
		if !ok {
			return false
		}
		startsWith, _ := title["startsWith"].(string)
		return startsWith == titlePrefix
	})
}

// hasManagedLabelArm reports whether the decoded IssueFilter carries the
// managed-label identity arm. Together with hasTitlePrefixArm it pins the
// reconciliation query to the same identity scope as the snapshot.
func hasManagedLabelArm(filterValue any, managedLabel string) bool {
	return hasIdentityArm(filterValue, func(clause map[string]any) bool {
		labels, ok := clause["labels"].(map[string]any)
		if !ok {
			return false
		}
		some, ok := labels["some"].(map[string]any)
		if !ok {
			return false
		}
		name, ok := some["name"].(map[string]any)
		if !ok {
			return false
		}
		eqIgnoreCase, _ := name["eqIgnoreCase"].(string)
		return eqIgnoreCase == managedLabel
	})
}

// hasIdentityArm walks the AND/OR structure Linear filters serialize to and
// reports whether any OR clause satisfies match.
func hasIdentityArm(filterValue any, match func(map[string]any) bool) bool {
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
			if ok && match(clause) {
				return true
			}
		}
	}
	return false
}

// newAmbiguousCreateServer stubs the Linear GraphQL API for the
// ambiguous-batch-create tests. The two-issue create batch fails once with a
// 502 (ambiguous: the mutation may or may not have landed); after that the
// issuesByFingerprint reconciliation is answered with one live fingerprint --
// but only when its filter carries the full identity scope. An unscoped
// query, like the `description contains` shape that must never be copied
// from the #199 dast trap, gets an error, which would fail the whole batch
// instead of reconciling it. When lookupFails is true the reconciliation
// query itself errors instead, modelling the lookup failing too.
func newAmbiguousCreateServer(t *testing.T, managedLabel string, lookupFails bool) (serverURL string, state *ambiguousCreateState) {
	t.Helper()
	state = &ambiguousCreateState{managedLabel: managedLabel, ambiguousLeft: 1}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			_, _ = io.WriteString(w, `{"data":{"team":{"states":{"nodes":[{"id":"state-todo","name":"Todo","type":"unstarted"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`)
		case strings.Contains(payload.Query, "query existingIssues"):
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		case strings.Contains(payload.Query, "query managedIssueLabels"):
			_, _ = io.WriteString(w, `{"data":{"issueLabels":{"nodes":[{"id":"label-managed","name":"`+state.managedLabel+`","team":{"id":"team-1"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		case strings.Contains(payload.Query, "query issuesByFingerprint"):
			state.mu.Lock()
			state.reconcileCalls++
			state.mu.Unlock()
			if lookupFails {
				http.Error(w, "issuesByFingerprint: lookup failed too", http.StatusInternalServerError)
				return
			}
			if !hasTitlePrefixArm(payload.Variables["filter"]) || !hasManagedLabelArm(payload.Variables["filter"], state.managedLabel) {
				http.Error(w, "issuesByFingerprint: query is not scoped to managed tickets", http.StatusBadRequest)
				return
			}
			// One of the batch's two creates landed before the response was
			// lost: fingerprint issue-1 exists live, issue-2 does not.
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"i-1","identifier":"SEC-1","description":"body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		case strings.Contains(payload.Query, "mutation issueCreateBatch"):
			state.mu.Lock()
			if state.ambiguousLeft > 0 {
				state.ambiguousLeft--
				state.ambiguousSeen++
				state.mu.Unlock()
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
				state.createDescriptions = append(state.createDescriptions, description)
			}
			state.mu.Unlock()
			_, _ = io.WriteString(w, `{"data":{"issueCreate0":{"success":true,"issue":{"id":"i-2","identifier":"SEC-2"}}}}`)
		default:
			http.Error(w, "unhandled operation: "+payload.Query, http.StatusBadRequest)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL, state
}

// ambiguousCreateState records what the stubbed Linear API saw.
type ambiguousCreateState struct {
	managedLabel string

	mu                 syncstd.Mutex
	ambiguousLeft      int
	ambiguousSeen      int
	reconcileCalls     int
	createDescriptions []string
}

// runAmbiguousCreate drives sync.Service.Run end to end with the real
// linear.Client pointed at the stub server.
func runAmbiguousCreate(t *testing.T, serverURL string) (linSync.RunResult, error) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := New(config.LinearConfig{
		TeamID: "team-1",
		APIKey: "test-key",
		Labels: config.LabelConfig{Managed: "snyk-automation"},
		States: config.StateConfig{
			Todo:      "Todo",
			Backlog:   "Backlog",
			Done:      "Done",
			Cancelled: "Cancelled",
		},
		Due: config.DueDateConfig{
			CriticalDays: 15,
			HighDays:     30,
			MediumDays:   45,
			LowDays:      90,
		},
	}, 1, logger)
	// New hardcodes the real Linear endpoint; point the client at the stub,
	// as the other real-client tests in this file do.
	client.gql = gqlclient.New(serverURL, &http.Client{})

	cfg := config.Config{
		Linear: config.LinearConfig{
			TeamID: "team-1",
			Labels: config.LabelConfig{Managed: "snyk-automation"},
			States: config.StateConfig{
				Todo:      "Todo",
				Backlog:   "Backlog",
				Done:      "Done",
				Cancelled: "Cancelled",
			},
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
	service := linSync.New(cfg, logger, e2eSnykFindings{}, client, nil)

	// The stub server returns immediately; the deadline only bounds a
	// regression that would wait out a backoff.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return service.Run(ctx)
}

// TestCreateBatchAmbiguousFailureReconcilesFirst covers the lost-response
// case: the batch create dies with a transport error after Linear committed
// one of its two creates, so retrying every item would duplicate that
// ticket. The run must reconcile first through the fingerprint query and
// re-create only the fingerprint that did not land -- exactly one per-item
// create, no duplicate.
func TestCreateBatchAmbiguousFailureReconcilesFirst(t *testing.T) {
	serverURL, state := newAmbiguousCreateServer(t, "snyk-automation", false)

	result, err := runAmbiguousCreate(t, serverURL)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0: reconciliation and the retry must both succeed", result.FailedOps)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.ambiguousSeen != 1 {
		t.Fatalf("ambiguous 502 responses served = %d, want exactly the first batch attempt", state.ambiguousSeen)
	}
	if state.reconcileCalls != 1 {
		t.Fatalf("issuesByFingerprint calls = %d, want exactly one reconciliation query", state.reconcileCalls)
	}
	if len(state.createDescriptions) != 1 {
		t.Fatalf("successful creates = %d, want exactly one retry (for the un-landed fingerprint), descriptions: %#v", len(state.createDescriptions), state.createDescriptions)
	}
	if !strings.Contains(state.createDescriptions[0], "snyk:project-a:issue-2") {
		t.Fatalf("retried create is not for fingerprint %q, got description:\n%s", "snyk:project-a:issue-2", state.createDescriptions[0])
	}
	if strings.Contains(state.createDescriptions[0], "snyk:project-a:issue-1") {
		t.Fatalf("the landed fingerprint snyk:project-a:issue-1 was retried; got description:\n%s", state.createDescriptions[0])
	}
}

// TestCreateBatchAmbiguousFailureLookupFailsLeavesBatchForNextRun covers the
// other branch: when the reconciliation query itself fails there is no safe
// way to tell what landed, so nothing is re-sent and the whole batch is
// counted in FailedOps. A missing ticket is recoverable on the next run; a
// duplicate is only undone later by the duplicate-cancellation pass.
func TestCreateBatchAmbiguousFailureLookupFailsLeavesBatchForNextRun(t *testing.T) {
	serverURL, state := newAmbiguousCreateServer(t, "snyk-automation", true)

	result, err := runAmbiguousCreate(t, serverURL)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.ambiguousSeen != 1 {
		t.Fatalf("ambiguous 502 responses served = %d, want exactly the first batch attempt", state.ambiguousSeen)
	}
	if state.reconcileCalls != 1 {
		t.Fatalf("issuesByFingerprint calls = %d, want 1", state.reconcileCalls)
	}
	if len(state.createDescriptions) != 0 {
		t.Fatalf("successful creates = %d, want 0: an unreconcilable ambiguous failure must not be retried, descriptions: %#v", len(state.createDescriptions), state.createDescriptions)
	}
	if result.FailedOps != 2 {
		t.Fatalf("FailedOps = %d, want 2 (the whole unreconciled batch)", result.FailedOps)
	}
}

// typedSnykFindings is e2eSnykFindings with an issue type on each finding, so
// the tool-label fallback (LINEAR_TOOL_LABEL_DEFAULT) has something to fire on.
type typedSnykFindings struct{}

func (typedSnykFindings) LoadSnapshot(context.Context) (model.SnykSnapshot, error) {
	snapshot, _ := e2eSnykFindings{}.LoadSnapshot(context.Background())
	for i := range snapshot.Findings {
		snapshot.Findings[i].IssueType = "code"
	}
	return snapshot, nil
}

// TestRunWithLabelManagementOffToleratesMissingManagedLabel is the issue #333
// end-to-end guard, using the real Linear client against a stubbed API whose
// workspace has no snyk-automation (or triage-dependency) label at all. With
// label management on, the label lookup finds nothing and the run fails; with
// LINEAR_MANAGED_LABEL=off the whole managed set is off, so no lookup happens,
// the create carries no labelIds, and the run succeeds.
func TestRunWithLabelManagementOffToleratesMissingManagedLabel(t *testing.T) {
	labelLookups := 0
	var createInputs []map[string]any

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			_, _ = io.WriteString(w, `{"data":{"team":{"states":{"nodes":[{"id":"state-todo","name":"Todo","type":"unstarted"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`)
		case strings.Contains(payload.Query, "query existingIssues"):
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		case strings.Contains(payload.Query, "query managedIssueLabels"):
			labelLookups++
			// The workspace has no such label: an empty result is what makes
			// resolveManagedLabel fail with "was not found".
			_, _ = io.WriteString(w, `{"data":{"issueLabels":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		case strings.Contains(payload.Query, "mutation issueCreateBatch"):
			results := make(map[string]any)
			for i := 0; ; i++ {
				input, ok := payload.Variables[fmt.Sprintf("input%d", i)].(map[string]any)
				if !ok {
					break
				}
				createInputs = append(createInputs, input)
				results[fmt.Sprintf("issueCreate%d", i)] = map[string]any{
					"success": true,
					"issue":   map[string]any{"id": fmt.Sprintf("i-%d", i+1), "identifier": fmt.Sprintf("SEC-%d", i+1)},
				}
			}
			resp, err := json.Marshal(map[string]any{"data": results})
			if err != nil {
				t.Errorf("marshal create response: %v", err)
				return
			}
			_, _ = w.Write(resp)
		default:
			http.Error(w, "unhandled operation: "+payload.Query, http.StatusBadRequest)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The defaults LINEAR_MANAGED_LABEL=off leaves behind: Managed empty, the
	// derived fallbacks still non-empty.
	labelCfg := config.LabelConfig{
		Managed:     "",
		ToolDefault: "snyk-automation",
		AwaitingFix: "triage-dependency",
	}
	states := config.StateConfig{Todo: "Todo", Backlog: "Backlog", Done: "Done", Cancelled: "Cancelled"}
	due := config.DueDateConfig{CriticalDays: 15, HighDays: 30, MediumDays: 45, LowDays: 90}

	client := New(config.LinearConfig{
		TeamID: "team-1",
		APIKey: "test-key",
		Labels: labelCfg,
		States: states,
		Due:    due,
	}, 1, logger)
	client.gql = gqlclient.New(server.URL, &http.Client{})

	cfg := config.Config{
		Linear: config.LinearConfig{TeamID: "team-1", Labels: labelCfg, States: states, Due: due},
		Sync:   config.SyncConfig{Workers: 1},
	}
	// The findings carry an issue type so the tool-label fallback would fire on
	// its own with label management on: without the off gate the create would
	// try to resolve snyk-automation and fail against this label-less workspace.
	service := linSync.New(cfg, logger, typedSnykFindings{}, client, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := service.Run(ctx)
	if err != nil {
		t.Fatalf("Run() error = %v, want success with label management off", err)
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0", result.FailedOps)
	}
	if labelLookups != 0 {
		t.Fatalf("managed label lookups = %d, want 0 when label management is off", labelLookups)
	}
	if len(createInputs) != 2 {
		t.Fatalf("creates = %d, want 2", len(createInputs))
	}
	for i, input := range createInputs {
		if labels, ok := input["labelIds"]; ok {
			t.Fatalf("create %d sent labelIds = %#v, want none when label management is off", i, labels)
		}
		description, _ := input["description"].(string)
		if strings.Contains(description, "managed_labels:") {
			t.Fatalf("create %d metadata block records managed labels:\n%s", i, description)
		}
	}
}
