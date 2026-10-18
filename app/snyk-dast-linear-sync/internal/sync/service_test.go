package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tesslio/snyk-dast-linear-sync/internal/cache"
	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
)

type fakeSnykDAST struct {
	snapshot model.SnykDASTSnapshot
}

func (f fakeSnykDAST) LoadSnapshot(context.Context) (model.SnykDASTSnapshot, error) {
	return f.snapshot, nil
}

type fakeLinear struct {
	snapshot []model.ExistingIssue
	created  []model.DesiredIssue
	updated  []model.DesiredIssue
	updates  []model.IssueUpdate
	comments []model.IssueUpdate

	// createFailIdx/commentFailIdx simulate a partial batch failure: the first
	// batch call reports these indices as failed, subsequent calls succeed.
	createFailIdx  []int
	commentFailIdx []int
	createCalls    []int
	// createAlwaysFailIdx makes every create call report a per-entry failure,
	// including single-entry retries.
	createAlwaysFailIdx bool

	// createBatchErr makes the first (batch) create call fail with an error,
	// simulating an ambiguous transport failure. landedFingerprints is what
	// ExistingFingerprints then reports as already present in Linear, and
	// existingErr makes that reconciliation query itself fail.
	createBatchErr     error
	landedFingerprints []string
	existingErr        error
	existingCalls      int

	// commentBatchErr makes the batch PostComments call fail with an error,
	// simulating an ambiguous outcome. commentCalls records the size of every
	// PostComments call so a test can assert no retry happened.
	commentBatchErr error
	commentCalls    []int
}

type fakeCache struct {
	snapshot cache.Snapshot
	saved    cache.Snapshot
}

func (f *fakeLinear) LoadSnapshot(context.Context) ([]model.ExistingIssue, error) {
	return f.snapshot, nil
}

func (f *fakeLinear) ExistingFingerprints(_ context.Context, fingerprints []string) (map[string]struct{}, error) {
	f.existingCalls++
	if f.existingErr != nil {
		return nil, f.existingErr
	}
	wanted := map[string]struct{}{}
	for _, fingerprint := range fingerprints {
		wanted[fingerprint] = struct{}{}
	}
	found := map[string]struct{}{}
	for _, fingerprint := range f.landedFingerprints {
		if _, ok := wanted[fingerprint]; ok {
			found[fingerprint] = struct{}{}
		}
	}
	return found, nil
}

func (f *fakeLinear) CreateIssues(_ context.Context, desired []model.DesiredIssue) ([]int, error) {
	if f.createBatchErr != nil && len(desired) > 1 {
		// Only the multi-entry batch call fails; single-entry retries succeed.
		err := f.createBatchErr
		f.createBatchErr = nil
		return nil, err
	}
	f.created = append(f.created, desired...)
	if f.createAlwaysFailIdx {
		// Every call, retries included, reports a per-entry failure with a nil
		// error — the case that used to be silently dropped.
		failed := make([]int, len(desired))
		for i := range desired {
			failed[i] = i
		}
		return failed, nil
	}
	if f.createFailIdx != nil {
		f.createCalls = append(f.createCalls, len(desired))
		failed := f.createFailIdx
		// Only the first batch call reports failures, so a retry can succeed.
		f.createFailIdx = nil
		return failed, nil
	}
	return nil, nil
}

func (f *fakeLinear) UpdateIssues(_ context.Context, updates []model.IssueUpdate) error {
	for _, update := range updates {
		f.updated = append(f.updated, update.Desired)
		f.updates = append(f.updates, update)
	}
	return nil
}

func (f *fakeLinear) PostComments(_ context.Context, updates []model.IssueUpdate) ([]int, error) {
	f.commentCalls = append(f.commentCalls, len(updates))
	if f.commentBatchErr != nil {
		err := f.commentBatchErr
		f.commentBatchErr = nil
		return nil, err
	}
	f.comments = append(f.comments, updates...)
	if f.commentFailIdx != nil {
		failed := f.commentFailIdx
		f.commentFailIdx = nil
		return failed, nil
	}
	return nil, nil
}

func (f *fakeCache) Load(context.Context) (cache.Snapshot, error) {
	return f.snapshot, nil
}

func (f *fakeCache) Save(_ context.Context, snapshot cache.Snapshot) error {
	f.saved = snapshot
	return nil
}

func TestRunPlansCreateUpdateAndResolve(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{
			Workers: 1,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					TargetHost:        "example.com",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					IssueURL:          "https://plus.probely.app/targets/target-a/findings/1",
					CreatedAt:         time.Date(2026, time.August, 1, 14, 0, 0, 0, time.UTC),
				},
				{
					Fingerprint:       "snyk-dast:target-b:finding-2",
					SnykDASTFindingID: "su2d3k-2",
					TargetID:          "target-b",
					TargetName:        "Other App",
					IssueTitle:        "SQL Injection",
					Severity:          "low",
					Status:            model.FindingIgnored,
					IssueURL:          "https://plus.probely.app/targets/target-b/findings/2",
					CreatedAt:         time.Date(2026, time.August, 1, 9, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{
				"target-a": {},
				"target-b": {},
				"target-z": {},
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
			},
			{
				ID:          "existing-2",
				Identifier:  "SEC-2",
				Title:       "old resolved issue",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-z:finding-9",
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1", result.PlannedCreates)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1", len(linear.created))
	}
	if len(linear.updated) != 2 {
		t.Fatalf("updated = %d, want 2", len(linear.updated))
	}
	if linear.created[0].DueDate != "2026-10-30" {
		t.Fatalf("created due date = %q, want %q", linear.created[0].DueDate, "2026-10-30")
	}
	if !containsDesiredState(linear.updated, model.StateDone) {
		t.Fatalf("updated states = %#v, want one %q", desiredStates(linear.updated), model.StateDone)
	}
}

func TestRunSkipsCachedUnchangedIssue(t *testing.T) {
	cfg := config.Config{
		Cache: config.CacheConfig{},
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{
			Workers: 1,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					TargetHost:        "example.com",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					IssueURL:          "https://plus.probely.app/targets/target-a/findings/1",
					IssueAPIURL:       "https://api.probely.com/findings/1/",
					CreatedAt:         time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{
				"target-a": {},
			},
		},
	}
	desired := desiredIssue(cfg, snykdast.snapshot.Findings[0])
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       desired.Title,
		Description: desired.Description,
		DueDate:     desired.DueDate,
		StateName:   "Todo",
		Fingerprint: desired.Fingerprint,
		Priority:    desired.Priority,
	}
	cacheStore := &fakeCache{
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			SnykDASTHashes: map[string]string{
				desired.Fingerprint: desiredIssueHash(desired),
			},
			LinearHashes: map[string]string{
				desired.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{existing},
	}

	service := New(cfg, logger, snykdast, linear, cacheStore)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

// TestRunCancelsIgnoredFindingEvenIfCached verifies that a finding which is
// ignored in Snyk DAST (desired state Cancelled) is moved to Cancelled even
// when its ticket was manually parked in "Todo" and the cache claims nothing
// changed.
func TestRunCancelsIgnoredFindingEvenIfCached(t *testing.T) {
	cfg := config.Config{
		Cache: config.CacheConfig{},
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "Information Disclosure",
					Severity:          "low",
					Status:            model.FindingIgnored,
					CreatedAt:         time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	desired := desiredIssue(cfg, snykdast.snapshot.Findings[0])
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       desired.Title,
		Description: desired.Description,
		DueDate:     desired.DueDate,
		StateName:   "Todo",
		Fingerprint: desired.Fingerprint,
		Priority:    desired.Priority,
	}
	cacheStore := &fakeCache{
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			SnykDASTHashes: map[string]string{
				desired.Fingerprint: desiredIssueHash(desired),
			},
			LinearHashes: map[string]string{
				desired.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}

	service := New(cfg, logger, snykdast, linear, cacheStore)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateCancelled {
		t.Fatalf("updated state = %q, want %q", linear.updated[0].State, model.StateCancelled)
	}
}

// TestRunCacheStillSkipsNonTerminalStateDivergence locks in the narrow scope
// of the terminal-transition cache guard: an open finding whose ticket
// diverges only in a non-terminal state must still be cache-suppressed when
// its hashes are unchanged.
func TestRunCacheStillSkipsNonTerminalStateDivergence(t *testing.T) {
	cfg := config.Config{
		Cache: config.CacheConfig{},
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen, // desired Todo — non-terminal
					CreatedAt:         time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	desired := desiredIssue(cfg, snykdast.snapshot.Findings[0]) // State == Todo
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       desired.Title,
		Description: desired.Description,
		DueDate:     desired.DueDate,
		StateName:   "Triage",
		Fingerprint: desired.Fingerprint,
		Priority:    desired.Priority,
	}
	cacheStore := &fakeCache{
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			SnykDASTHashes: map[string]string{
				desired.Fingerprint: desiredIssueHash(desired),
			},
			LinearHashes: map[string]string{
				desired.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}

	service := New(cfg, logger, snykdast, linear, cacheStore)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (non-terminal divergence stays cached)", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

func TestRunCancelsMissingIssueWhenTargetDeleted(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			TargetIDs: map[string]struct{}{
				"target-a": {},
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "missing target issue",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-z:finding-9",
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateCancelled {
		t.Fatalf("resolved state = %q, want %q", linear.updated[0].State, model.StateCancelled)
	}
}

func TestRunCancelsMissingIssueWhenTargetDeletedEvenIfCached(t *testing.T) {
	cfg := config.Config{
		Cache: config.CacheConfig{},
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			TargetIDs: map[string]struct{}{
				"target-a": {},
			},
		},
	}
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       "missing target issue",
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: "snyk-dast:target-z:finding-9",
	}
	cacheStore := &fakeCache{
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			LinearHashes: map[string]string{
				existing.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{existing},
	}

	service := New(cfg, logger, snykdast, linear, cacheStore)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateCancelled {
		t.Fatalf("resolved state = %q, want %q", linear.updated[0].State, model.StateCancelled)
	}
}

func TestNeedsUpdateUsesCaseInsensitiveLabels(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-04-01",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-04-01",
		State:       model.StateTodo,
		Priority:    2,
	}

	if needsUpdate(existing, desired) {
		t.Fatal("needsUpdate() = true, want false")
	}
}

func containsDesiredState(desired []model.DesiredIssue, state model.IssueState) bool {
	for _, issue := range desired {
		if issue.State == state {
			return true
		}
	}
	return false
}

func desiredStates(desired []model.DesiredIssue) []model.IssueState {
	out := make([]model.IssueState, 0, len(desired))
	for _, issue := range desired {
		out = append(out, issue.State)
	}
	return out
}

func TestDesiredIssueDueDateUsesCreatedAt(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetName:        "Example App",
		Severity:          "critical",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.August, 11, 23, 30, 0, 0, time.FixedZone("minus0500", -5*60*60)),
	}

	desired := desiredIssue(cfg, finding)

	if desired.DueDate != "2026-08-27" {
		t.Fatalf("desired due date = %q, want %q", desired.DueDate, "2026-08-27")
	}
}

func TestDesiredIssueDueDateLeavesPastSLAAsIs(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetName:        "Example App",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	if desired.DueDate != "2026-01-31" {
		t.Fatalf("desired due date = %q, want %q (raw SLA date for past issues)", desired.DueDate, "2026-01-31")
	}
}

func TestDesiredIssueDueDateUsesAcceptanceExpiryWhenSnoozed(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				HighDays: 30,
			},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetName:        "Example App",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		IgnoreExpiresAt:   time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	// IgnoreExpiresAt wins over CreatedAt: June 1 + 30 days = July 1.
	if desired.DueDate != "2026-07-01" {
		t.Fatalf("desired due date = %q, want %q (acceptance expiry + high offset)", desired.DueDate, "2026-07-01")
	}
}

func TestDesiredIssueRendersDASTContext(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed:           "snyk-dast-automation",
				TargetType:        map[string]string{"api": "snyk-dast-api"},
				TargetTypeDefault: "snyk-dast-web",
			},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example API",
		TargetType:        "api",
		TargetURL:         "https://api.example.com",
		TargetHost:        "api.example.com",
		IssueTitle:        "Broken Access Control",
		Severity:          "high",
		Status:            model.FindingOpen,
		IssueURL:          "https://plus.probely.app/targets/target-a/findings/1",
		IssueAPIURL:       "https://api.probely.com/findings/1/",
		FindingURL:        "https://api.example.com/users/42",
		Method:            "get",
		Path:              "users/42",
		Parameter:         "id",
		InsertionPoint:    "parameter",
		CWE:               "CWE-284",
		CWEName:           "Improper Access Control",
		CVSS:              7.5,
	}

	desired := desiredIssue(cfg, finding)

	if desired.Title != "Snyk DAST: [high] api.example.com: Broken Access Control at GET users/42" {
		t.Fatalf("title = %q", desired.Title)
	}
	if !strings.Contains(desired.Description, "## Broken Access Control [HIGH]") {
		t.Fatalf("description missing heading: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "URL: [GET users/42](https://api.example.com/users/42)") {
		t.Fatalf("description missing vulnerable URL line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Method: `GET`") {
		t.Fatalf("description missing method line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Path: `users/42`") {
		t.Fatalf("description missing path line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Parameter: `id` (Parameter)") {
		t.Fatalf("description missing parameter line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "CWE: `CWE-284` (Improper Access Control)") {
		t.Fatalf("description missing CWE line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "CVSS: `7.5`") {
		t.Fatalf("description missing CVSS line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Snyk DAST: [Open finding](https://plus.probely.app/targets/target-a/findings/1)") {
		t.Fatalf("description missing Snyk DAST link: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "API: [Finding details](https://api.probely.com/findings/1/)") {
		t.Fatalf("description missing API link: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Status: `open`") {
		t.Fatalf("description missing status line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "managed_labels: snyk-dast-api,snyk-dast-automation") {
		t.Fatalf("description missing managed labels metadata: %s", desired.Description)
	}
	// NormalizeManagedLabelNames sorts labels alphabetically, so snyk-dast-api
	// precedes snyk-dast-automation regardless of insertion order.
	if len(desired.ManagedLabels) != 2 || desired.ManagedLabels[0] != "snyk-dast-api" || desired.ManagedLabels[1] != "snyk-dast-automation" {
		t.Fatalf("ManagedLabels = %#v, want [snyk-dast-api snyk-dast-automation]", desired.ManagedLabels)
	}
}

func TestDesiredIssueRendersSnykCodeCorrelation(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed: "snyk-dast-automation",
			},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example API",
		TargetHost:        "api.example.com",
		IssueTitle:        "SQL Injection",
		Severity:          "high",
		Status:            model.FindingOpen,
		Method:            "get",
		Path:              "users/42",
		CorrelationMarkdown: []string{
			"**Snyk Code:** [SQL Injection in `src/db/query.go:42`](https://app.snyk.io/org/example/project/abc#issue-SNYK-GO-1234)",
			"Repository: `owner/repo` — confirmed correlation",
		},
	}

	desired := desiredIssue(cfg, finding)

	if !strings.Contains(desired.Description, "### Snyk Code correlation") {
		t.Fatalf("description missing Snyk Code correlation heading: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Linked Snyk Code source vulnerability responsible for this runtime finding.") {
		t.Fatalf("description missing correlation subtitle: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "SQL Injection in `src/db/query.go:42`") {
		t.Fatalf("description missing correlation markdown block: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Repository: `owner/repo`") {
		t.Fatalf("description missing second correlation block: %s", desired.Description)
	}
}

func TestDesiredIssueOmitsCorrelationSectionWhenEmpty(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{Managed: "snyk-dast-automation"},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetName:        "Example App",
		IssueTitle:        "XSS",
		Severity:          "medium",
		Status:            model.FindingOpen,
	}

	desired := desiredIssue(cfg, finding)

	if strings.Contains(desired.Description, "Snyk Code correlation") {
		t.Fatalf("description should not contain correlation section when empty: %s", desired.Description)
	}
}

func TestDesiredIssueUsesTargetTypeDefaultLabel(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed:           "snyk-dast-automation",
				TargetTypeDefault: "snyk-dast-web",
			},
		},
	}
	finding := model.Finding{
		Fingerprint: "snyk-dast:target-a:finding-1",
		TargetType:  "single",
		IssueTitle:  "XSS",
		Severity:    "medium",
		Status:      model.FindingOpen,
	}

	desired := desiredIssue(cfg, finding)

	found := false
	for _, label := range desired.ManagedLabels {
		if label == "snyk-dast-web" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ManagedLabels = %#v, want snyk-dast-web fallback", desired.ManagedLabels)
	}
}

func TestIssueTitleUsesTargetNameWhenNoHost(t *testing.T) {
	finding := model.Finding{
		IssueTitle: "TLS Misconfiguration",
		Severity:   "medium",
		TargetName: "Marketing Site",
		Path:       "login",
		Method:     "post",
	}

	title := issueTitle(finding)

	if title != "Snyk DAST: [medium] Marketing Site: TLS Misconfiguration at POST login" {
		t.Fatalf("title = %q", title)
	}
}

func TestUpsertManagedMetadataRemovesVisibleFingerprintFooter(t *testing.T) {
	description := "Status: `open`\n\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->\nFingerprint: snyk-dast:target-a:finding-1"

	got := upsertManagedMetadata(description, "snyk-dast:target-a:finding-1", []string{"snyk-dast-automation", "snyk-dast-api"})

	if strings.Contains(got, "Fingerprint: snyk-dast:target-a:finding-1") {
		t.Fatalf("upsertManagedMetadata() left visible fingerprint footer: %s", got)
	}
	if !strings.Contains(got, "managed_labels: snyk-dast-api,snyk-dast-automation") {
		t.Fatalf("upsertManagedMetadata() missing managed labels metadata: %s", got)
	}
}

// TestUpsertManagedMetadataIgnoresInlineMarker verifies that the metadata
// marker appearing mid-sentence in user text does not corrupt the description.
func TestUpsertManagedMetadataIgnoresInlineMarker(t *testing.T) {
	description := "See <!-- snyk-dast-linear-sync notes --> for details\n\nStatus: `open`"

	got := upsertManagedMetadata(description, "snyk-dast:target-a:finding-1", []string{"snyk-dast-automation"})

	if !strings.Contains(got, "See <!-- snyk-dast-linear-sync notes -->") {
		t.Fatalf("upsertManagedMetadata() corrupted inline marker: %s", got)
	}
	if !strings.Contains(got, "<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1") {
		t.Fatalf("upsertManagedMetadata() missing proper metadata block: %s", got)
	}
}

func TestFindMetadataBlockStartAnchoredToLineBoundary(t *testing.T) {
	cases := []struct {
		name        string
		description string
		wantIdx     int
	}{
		{
			name:        "marker at start of description",
			description: "<!-- snyk-dast-linear-sync\nfingerprint: test\n-->",
			wantIdx:     0,
		},
		{
			name:        "marker at start of line",
			description: "Some text\n<!-- snyk-dast-linear-sync\nfingerprint: test\n-->",
			wantIdx:     10,
		},
		{
			name:        "marker mid-sentence should not match",
			description: "See <!-- snyk-dast-linear-sync notes --> for details",
			wantIdx:     -1,
		},
		{
			name:        "no marker at all",
			description: "Just a normal description",
			wantIdx:     -1,
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

func TestNeedsUpdateDetectsManagedLabelChange(t *testing.T) {
	existing := model.ExistingIssue{
		Title:         "title",
		Description:   "description",
		DueDate:       "2026-04-01",
		StateName:     "Todo",
		ManagedLabels: []string{"old-label"},
		Labels: []model.IssueLabel{
			{ID: "label-1", Name: "old-label"},
		},
		Priority: 2,
	}
	desired := model.DesiredIssue{
		Title:         "title",
		Description:   "description",
		DueDate:       "2026-04-01",
		State:         model.StateTodo,
		ManagedLabels: []string{"new-label"},
		Priority:      2,
	}

	if !needsUpdate(existing, desired) {
		t.Fatal("needsUpdate() = false, want true")
	}
}

func TestManagedLabelsUsesConfiguredCheckTypeMapping(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed:   "snyk-dast-automation",
		CheckType: map[string]string{"passive": "snyk-dast-passive", "active": "snyk-dast-active"},
	}, model.Finding{CheckType: "passive"})

	found := false
	for _, l := range labels {
		if l == "snyk-dast-passive" {
			found = true
		}
	}
	if !found {
		t.Fatalf("managedLabels() = %#v, want snyk-dast-passive for passive check", labels)
	}
}

func TestManagedLabelsUsesConfiguredTargetTypeMapping(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed:    "snyk-dast-automation",
		TargetType: map[string]string{"api": "snyk-dast-api"},
	}, model.Finding{TargetType: "api"})

	// Labels are normalized (sorted) alphabetically.
	if len(labels) != 2 || labels[0] != "snyk-dast-api" || labels[1] != "snyk-dast-automation" {
		t.Fatalf("managedLabels() = %#v, want [snyk-dast-api snyk-dast-automation]", labels)
	}
}

func TestManagedLabelsOmitsTargetTypeLabelWhenNoMapping(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed: "snyk-dast-automation",
	}, model.Finding{TargetType: "single"})

	if len(labels) != 1 || labels[0] != "snyk-dast-automation" {
		t.Fatalf("managedLabels() = %#v, want [snyk-dast-automation]", labels)
	}
}

func TestNeedsUpdateClearsDueDateWhenDesiredIsEmpty(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-07-01",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "",
		State:       model.StateTodo,
		Priority:    2,
	}

	if !needsUpdate(existing, desired) {
		t.Fatal("needsUpdate() = false, want true (must clear stale due date)")
	}
}

func TestNeedsUpdateSkipsDueDateWhenBothEmpty(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "",
		State:       model.StateTodo,
		Priority:    2,
	}

	if needsUpdate(existing, desired) {
		t.Fatal("needsUpdate() = true, want false (both due dates empty)")
	}
}

func TestNeedsUpdateIncludesDueDate(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-04-01",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-04-15",
		State:       model.StateTodo,
		Priority:    2,
	}

	if !needsUpdate(existing, desired) {
		t.Fatal("needsUpdate() = false, want true")
	}
}

func TestIdentifierNum(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"PROB-1", 1},
		{"SEC-42", 42},
		{"SNYK-11596", 11596},
		{"nodash", 0},
		{"", 0},
		{"SNYK-abc", 0},
	}
	for _, tc := range cases {
		if got := identifierNum(tc.input); got != tc.want {
			t.Errorf("identifierNum(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestRunCancelsDuplicateFingerprintKeepsLowerIdentifier(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "CVE-2026-1234",
					Severity:          "high",
					Status:            model.FindingOpen,
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-20",
				Title:       "Snyk DAST: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    2,
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-10",
				Title:       "Snyk DAST: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    2,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Conflicts != 1 {
		t.Fatalf("Conflicts = %d, want 1", result.Conflicts)
	}
	if result.CancelledDuplicates != 1 {
		t.Fatalf("CancelledDuplicates = %d, want 1", result.CancelledDuplicates)
	}

	cancelledIDs := cancelledIdentifiers(linear.updates)
	if len(cancelledIDs) != 1 || cancelledIDs[0] != "SNYK-20" {
		t.Fatalf("cancelled identifiers = %v, want [SNYK-20]", cancelledIDs)
	}
}

func TestRunDuplicateCancellationIsIdempotentWhenAlreadyCancelled(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "CVE-2026-1234",
					Severity:          "high",
					Status:            model.FindingOpen,
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-20",
				Title:       "Snyk DAST: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Cancelled",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    2,
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-10",
				Title:       "Snyk DAST: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    2,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Conflicts != 1 {
		t.Fatalf("Conflicts = %d, want 1", result.Conflicts)
	}
	if result.CancelledDuplicates != 0 {
		t.Fatalf("CancelledDuplicates = %d, want 0 (already cancelled)", result.CancelledDuplicates)
	}
}

func TestRunCancelsDuplicateAndStillSyncsCanonical(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "CVE-2026-1234",
					Severity:          "high",
					Status:            model.FindingOpen,
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-20",
				Title:       "stale title",
				Description: "d\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-10",
				Title:       "stale title",
				Description: "d\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.CancelledDuplicates != 1 {
		t.Fatalf("CancelledDuplicates = %d, want 1", result.CancelledDuplicates)
	}
	if !containsStr(updatedIdentifiers(linear.updates), "SNYK-10") {
		t.Fatalf("SNYK-10 (canonical) was not updated; updates: %v", updatedIdentifiers(linear.updates))
	}
	if !containsStr(cancelledIdentifiers(linear.updates), "SNYK-20") {
		t.Fatalf("SNYK-20 (duplicate) was not cancelled; cancelled: %v", cancelledIdentifiers(linear.updates))
	}
}

// minimalCfg returns the smallest valid Config needed to run the service in tests.
func minimalCfg() config.Config {
	return config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
}

func cancelledIdentifiers(updates []model.IssueUpdate) []string {
	var out []string
	for _, u := range updates {
		if u.Desired.State == model.StateCancelled {
			out = append(out, u.Existing.Identifier)
		}
	}
	return out
}

func updatedIdentifiers(updates []model.IssueUpdate) []string {
	out := make([]string, 0, len(updates))
	for _, u := range updates {
		out = append(out, u.Existing.Identifier)
	}
	return out
}

func containsStr(slice []string, s string) bool {
	return slices.Contains(slice, s)
}

func TestRunRespectsManualBacklogMove(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example App",
		IssueTitle:        "XSS",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{finding},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Backlog",
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (Backlog override should prevent state-only update)", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

func TestRunDoesNotOverrideBacklogForFixedFindings(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example App",
		IssueTitle:        "XSS",
		Severity:          "high",
		Status:            model.FindingFixed,
		CreatedAt:         time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{finding},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Backlog",
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (fixed finding should move to Done)", result.PlannedUpdates)
	}
	if linear.updated[0].State != model.StateDone {
		t.Fatalf("updated state = %q, want %q", linear.updated[0].State, model.StateDone)
	}
}

func TestRunPreservesNonTerminalStateWhenUserMovesToTodo(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example App",
		IssueTitle:        "XSS",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{finding},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}

	desired := desiredIssue(cfg, finding)
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Todo", // not the configured open state "Triage"
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (Todo state should be preserved)", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

func TestRunDoesNotPreserveTerminalStates(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example App",
		IssueTitle:        "XSS",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{finding},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}

	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Done", // terminal state — should NOT be preserved
				Fingerprint: finding.Fingerprint,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates == 0 {
		t.Fatal("PlannedUpdates = 0, want > 0 (Done state should NOT be preserved for open finding)")
	}
}

func TestRunPostsChangeCommentsOnUpdate(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.CommentsEnabled = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "Updated title",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}

	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    3,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if len(linear.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(linear.comments))
	}
}

func TestRunSkipsCommentsForResolve(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "existing-1",
				Identifier:    "SEC-1",
				Title:         "resolved issue",
				Description:   "old description\n\u003c!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-z:finding-9\n--\u003e",
				StateName:     "Todo",
				Fingerprint:   "snyk-dast:target-z:finding-9",
				ManagedLabels: []string{"snyk-dast-automation"},
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.comments) != 0 {
		t.Fatalf("comments = %d, want 0 (no comments for resolve)", len(linear.comments))
	}
}

func TestComputeDiffDetectsAllChanges(t *testing.T) {
	existing := model.ExistingIssue{
		ID:            "issue-1",
		Identifier:    "SEC-1",
		Title:         "old title",
		Description:   "old description",
		DueDate:       "2026-04-01",
		StateName:     "Todo",
		Priority:      3,
		ManagedLabels: []string{"snyk-dast-automation"},
		Labels:        []model.IssueLabel{{ID: "label-1", Name: "snyk-dast-automation"}},
	}
	desired := model.DesiredIssue{
		Title:         "new title",
		Description:   "new description",
		DueDate:       "2026-05-01",
		State:         model.StateBacklog,
		Priority:      1,
		ManagedLabels: []string{"snyk-dast-automation", "snyk-dast-api"},
	}

	diff := ComputeDiff(existing, desired)

	if !diff.TitleChanged || !diff.DescriptionChanged || !diff.DueDateChanged || !diff.StateChanged || !diff.PriorityChanged {
		t.Fatalf("expected all field changes, got: %+v", diff)
	}
	if len(diff.LabelsAdded) != 1 || diff.LabelsAdded[0] != "snyk-dast-api" {
		t.Fatalf("labels added = %v, want [snyk-dast-api]", diff.LabelsAdded)
	}
	if !diff.LabelsNeedUpdate {
		t.Fatal("expected LabelsNeedUpdate when labels are added")
	}
}

func TestComputeDiffNoStateChangeWhenPreserveState(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "desc",
		DueDate:     "2026-04-01",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:         "title",
		Description:   "desc",
		DueDate:       "2026-04-01",
		State:         model.StateBacklog,
		PreserveState: true,
		ManagedLabels: []string{"snyk-dast-automation"},
		Priority:      2,
	}

	diff := ComputeDiff(existing, desired)

	if diff.StateChanged {
		t.Fatal("expected no state change when PreserveState=true")
	}
}

func TestIsConfiguredBacklogState(t *testing.T) {
	cases := []struct {
		existing   string
		configured string
		want       bool
	}{
		{"Backlog", "Backlog", true},
		{"backlog", "Backlog", true},
		{"Todo", "Backlog", false},
		{"", "Backlog", false},
	}
	for _, tc := range cases {
		got := isConfiguredBacklogState(tc.existing, tc.configured)
		if got != tc.want {
			t.Errorf("isConfiguredBacklogState(%q, %q) = %v, want %v", tc.existing, tc.configured, got, tc.want)
		}
	}
}

// TestRunSnoozedFindingStaysOpenWithExtendedDueDate verifies that a finding
// with a time-limited acceptance (snoozed) is kept open in Todo with a due
// date calculated from the acceptance expiry, not cancelled.
func TestRunSnoozedFindingStaysOpenWithExtendedDueDate(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	acceptanceExpiry := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "CVE-2026-1234",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
					IgnoreExpiresAt:   acceptanceExpiry,
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "Snyk DAST: [high] CVE-2026-1234",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    2,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	updated := linear.updated[0]
	if updated.State != model.StateTodo {
		t.Fatalf("updated state = %q, want %q (snoozed stays open)", updated.State, model.StateTodo)
	}
	// Due date should be calculated from IgnoreExpiresAt (2026-06-01) + 30 days = 2026-07-01
	if updated.DueDate != "2026-07-01" {
		t.Fatalf("updated due date = %q, want %q", updated.DueDate, "2026-07-01")
	}
}

// baseTestConfig returns the minimal config the archive/batch tests need.
func baseTestConfig() config.Config {
	return config.Config{
		Linear: config.LinearConfig{
			ArchiveLookbackDays: 35,
			States: config.StateConfig{
				Todo:      "Triage",
				Backlog:   "Backlog",
				Done:      "Done",
				Cancelled: "Canceled",
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
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunDoesNotRecreateArchivedClosedIssue is the regression test for the
// duplicate-minting loop: Snyk DAST keeps fixed findings in its API forever, so
// once Linear auto-archives the ticket the sync must still recognise it rather
// than create a fresh copy on every run.
func TestRunDoesNotRecreateArchivedClosedIssue(t *testing.T) {
	archivedAt := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingFixed,
					CreatedAt:         time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "Snyk DAST: [high] Reflected XSS",
				StateName:   "Done",
				Fingerprint: "snyk-dast:target-a:finding-1",
				ArchivedAt:  &archivedAt,
			},
		},
	}

	result, err := New(baseTestConfig(), discardLogger(), snykdast, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d (%#v), want 0: an archived closed ticket must not be recreated", len(linear.created), linear.created)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0: archived issues cannot be mutated", len(linear.updated))
	}
	if result.PlannedResolves != 0 {
		t.Fatalf("PlannedResolves = %d, want 0", result.PlannedResolves)
	}
}

// TestRunReplacesArchivedIssueWhenFindingReopens covers the other half: an
// archived ticket cannot be reopened via the API, so a regressed finding needs a
// replacement ticket rather than a silently-dropped update.
func TestRunReplacesArchivedIssueWhenFindingReopens(t *testing.T) {
	archivedAt := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "Snyk DAST: [high] Reflected XSS",
				StateName:   "Done",
				Fingerprint: "snyk-dast:target-a:finding-1",
				ArchivedAt:  &archivedAt,
			},
		},
	}

	if _, err := New(baseTestConfig(), discardLogger(), snykdast, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1 replacement ticket", len(linear.created))
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0: archived issues cannot be mutated", len(linear.updated))
	}
}

// TestRunRetriesOnlyFailedCreates is the regression test for the partial-batch
// duplicate: when one entry in a batch fails, only that entry may be retried.
func TestRunRetriesOnlyFailedCreates(t *testing.T) {
	var findings []model.Finding
	for i := 1; i <= 3; i++ {
		findings = append(findings, model.Finding{
			Fingerprint:       fmt.Sprintf("snyk-dast:target-a:finding-%d", i),
			SnykDASTFindingID: fmt.Sprintf("su2d3k-%d", i),
			TargetID:          "target-a",
			IssueTitle:        "Reflected XSS",
			Severity:          "high",
			Status:            model.FindingOpen,
			CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  findings,
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	// The first batch call reports index 1 as failed; the retry must carry that
	// one entry only, not all three.
	linear := &fakeLinear{createFailIdx: []int{1}}

	if _, err := New(baseTestConfig(), discardLogger(), snykdast, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// created accumulates every entry passed to CreateIssues: 3 from the batch
	// plus exactly 1 from the retry.
	if len(linear.created) != 4 {
		t.Fatalf("created = %d, want 4 (3 in batch + 1 retried); a whole-batch retry would give 6", len(linear.created))
	}
	if len(linear.createCalls) != 1 || linear.createCalls[0] != 3 {
		t.Fatalf("createCalls = %#v, want a single 3-entry batch", linear.createCalls)
	}
}

func TestPreferCanonicalDuplicatePrefersLiveTicket(t *testing.T) {
	states := config.StateConfig{Todo: "Triage", Done: "Done", Cancelled: "Canceled"}
	archivedAt := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)

	older := model.ExistingIssue{Identifier: "SEC-1", StateName: "Canceled"}
	newer := model.ExistingIssue{Identifier: "SEC-9", StateName: "Triage"}
	canonical, duplicate := preferCanonicalDuplicate(older, newer, states)
	if canonical.Identifier != "SEC-9" || duplicate.Identifier != "SEC-1" {
		t.Fatalf("canonical = %q, duplicate = %q; want the live ticket SEC-9 as canonical", canonical.Identifier, duplicate.Identifier)
	}

	archived := model.ExistingIssue{Identifier: "SEC-1", StateName: "Done", ArchivedAt: &archivedAt}
	live := model.ExistingIssue{Identifier: "SEC-9", StateName: "Triage"}
	canonical, _ = preferCanonicalDuplicate(archived, live, states)
	if canonical.Identifier != "SEC-9" {
		t.Fatalf("canonical = %q, want SEC-9: an archived copy cannot be canonical", canonical.Identifier)
	}

	// Same class: the older (lower) identifier wins.
	a := model.ExistingIssue{Identifier: "SEC-4", StateName: "Triage"}
	b := model.ExistingIssue{Identifier: "SEC-2", StateName: "Triage"}
	canonical, duplicate = preferCanonicalDuplicate(a, b, states)
	if canonical.Identifier != "SEC-2" || duplicate.Identifier != "SEC-4" {
		t.Fatalf("canonical = %q, duplicate = %q; want SEC-2 canonical", canonical.Identifier, duplicate.Identifier)
	}
}

// TestRunCountsRetryFailuresReportedWithoutError guards the accounting hole
// CodeRabbit flagged: the batch client reports per-entry failures through a
// failed-index return with a nil error, so a retry loop that inspects only the
// error value counts a failed create as done and the run reports success for an
// issue that was never created.
func TestRunCountsRetryFailuresReportedWithoutError(t *testing.T) {
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{createAlwaysFailIdx: true}

	result, err := New(baseTestConfig(), discardLogger(), snykdast, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FailedOps != 1 {
		t.Fatalf("FailedOps = %d, want 1: a create that failed without an error must still be counted", result.FailedOps)
	}
}

// threeOpenFindings returns a snapshot with three open findings, for the
// ambiguous-batch tests.
func threeOpenFindings() fakeSnykDAST {
	var findings []model.Finding
	for i := 1; i <= 3; i++ {
		findingID := fmt.Sprintf("su2d3k-%d", i)
		findings = append(findings, model.Finding{
			// Derive the fingerprint the same way the production path does, so the
			// fixture keys match what the service looks up.
			Fingerprint:       model.Fingerprint("target-a", findingID),
			SnykDASTFindingID: findingID,
			TargetID:          "target-a",
			IssueTitle:        "Reflected XSS",
			Severity:          "high",
			Status:            model.FindingOpen,
			CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	return fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  findings,
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
}

// TestRunReconcilesAmbiguousBatchCreateBeforeRetry covers the lost-response case:
// a transport failure does not prove Linear rejected the mutation, so the entries
// that actually landed must not be created a second time.
func TestRunReconcilesAmbiguousBatchCreateBeforeRetry(t *testing.T) {
	linear := &fakeLinear{
		createBatchErr: errors.New("connection reset by peer"),
		// Two of the three creates were committed before the response was lost.
		landedFingerprints: []string{
			model.Fingerprint("target-a", "su2d3k-1"),
			model.Fingerprint("target-a", "su2d3k-3"),
		},
	}

	result, err := New(baseTestConfig(), discardLogger(), threeOpenFindings(), linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if linear.existingCalls != 1 {
		t.Fatalf("ExistingFingerprints calls = %d, want 1", linear.existingCalls)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d (%#v), want 1: only the entry that did not land may be recreated",
			len(linear.created), desiredFingerprints(linear.created))
	}
	if want := model.Fingerprint("target-a", "su2d3k-2"); linear.created[0].Fingerprint != want {
		t.Fatalf("recreated %q, want the un-landed %q", linear.created[0].Fingerprint, want)
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0", result.FailedOps)
	}
}

// TestRunSkipsRetryWhenReconciliationFails covers the other branch: with no way
// to tell what landed, nothing is retried. A missing ticket is recoverable on the
// next run; a duplicate is not.
func TestRunSkipsRetryWhenReconciliationFails(t *testing.T) {
	linear := &fakeLinear{
		createBatchErr: errors.New("connection reset by peer"),
		existingErr:    errors.New("lookup failed too"),
	}

	result, err := New(baseTestConfig(), discardLogger(), threeOpenFindings(), linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0: an unreconcilable ambiguous failure must not be retried", len(linear.created))
	}
	if result.FailedOps != 3 {
		t.Fatalf("FailedOps = %d, want 3 (the whole unreconciled batch)", result.FailedOps)
	}
}

func desiredFingerprints(desired []model.DesiredIssue) []string {
	out := make([]string, 0, len(desired))
	for _, d := range desired {
		out = append(out, d.Fingerprint)
	}
	return out
}

// TestRunCountsAmbiguousCommentBatchWithoutRetrying covers the comment half of
// the ambiguous-outcome handling. A comment carries no fingerprint to reconcile
// against, so retrying risks posting it twice while the issue state is already
// correct. The batch is therefore counted and not retried.
func TestRunCountsAmbiguousCommentBatchWithoutRetrying(t *testing.T) {
	cfg := baseTestConfig()
	cfg.Linear.CommentsEnabled = true

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       model.Fingerprint("target-a", "su2d3k-1"),
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	// An existing issue with a stale title, so the finding produces an update and
	// therefore a change comment.
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Triage",
				Fingerprint: model.Fingerprint("target-a", "su2d3k-1"),
			},
		},
		commentBatchErr: errors.New("connection reset by peer"),
	}

	result, err := New(cfg, discardLogger(), snykdast, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (the update itself must still apply)", result.PlannedUpdates)
	}
	// PlannedUpdates only proves the update was queued. Assert it actually
	// applied, so a regression that suppresses the update while keeping the
	// planned count is caught here too.
	if len(linear.updated) != 1 {
		t.Fatalf("applied updates = %d, want 1: the comment failure must not prevent the issue update", len(linear.updated))
	}
	// The batch is counted, not silently dropped.
	if result.FailedComments != 1 {
		t.Fatalf("FailedComments = %d, want 1 (the size of the ambiguous batch)", result.FailedComments)
	}
	// Exactly one PostComments call: the batch. A retry would risk duplicating a
	// comment that may already have posted.
	if len(linear.commentCalls) != 1 {
		t.Fatalf("PostComments calls = %#v, want exactly one batch call and no retry", linear.commentCalls)
	}
	if len(linear.comments) != 0 {
		t.Fatalf("comments recorded = %d, want 0: the batch call failed", len(linear.comments))
	}
	// A failed comment must not be recorded as a failed sync operation — the
	// issue state itself applied correctly.
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0: a comment failure is not a state-sync failure", result.FailedOps)
	}
}

// TestRunResultErrOnlyFailsOnUnappliedOps pins the exit-status contract: the run
// fails only when state-changing work was left permanently unapplied. A clean run
// and a run whose only casualty was an explanatory comment both succeed.
func TestRunResultErrOnlyFailsOnUnappliedOps(t *testing.T) {
	if err := (RunResult{PlannedCreates: 5}).Err(); err != nil {
		t.Fatalf("Err() = %v, want nil for a clean run", err)
	}
	if err := (RunResult{FailedComments: 3}).Err(); err != nil {
		t.Fatalf("Err() = %v, want nil: a missing change comment leaves the synced state correct", err)
	}
	err := (RunResult{FailedOps: 2}).Err()
	if err == nil {
		t.Fatal("Err() = nil, want an error when Linear operations could not be applied")
	}
	if !strings.Contains(err.Error(), "2") {
		t.Fatalf("Err() = %v, want the failed-operation count in the message", err)
	}
}

// TestRunSucceedsWhenBatchFailureIsRecoveredByRetry is the "eventually succeeded
// is fine" half of the contract. A batch that partially fails and whose retry
// lands must not fail the run: the sync did apply every operation, it just took
// two calls.
func TestRunSucceedsWhenBatchFailureIsRecoveredByRetry(t *testing.T) {
	// createFailIdx applies to the first batch call only, so the single-entry
	// retry succeeds.
	linear := &fakeLinear{createFailIdx: []int{1}}

	result, err := New(baseTestConfig(), discardLogger(), threeOpenFindings(), linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0: the retry landed, so nothing was left unapplied", result.FailedOps)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil: a batch failure recovered by retry is a successful run", err)
	}
}

// TestRunFailsWhenRetryAlsoFails is the other half: once the per-entry retry has
// itself failed the issue does not exist, and a run that logs that but reports
// success means nothing downstream notices.
func TestRunFailsWhenRetryAlsoFails(t *testing.T) {
	linear := &fakeLinear{createAlwaysFailIdx: true}

	result, err := New(baseTestConfig(), discardLogger(), threeOpenFindings(), linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FailedOps != 3 {
		t.Fatalf("FailedOps = %d, want 3", result.FailedOps)
	}
	if result.Err() == nil {
		t.Fatal("Err() = nil, want an error: three creates never landed")
	}
}

// TestRunSucceedsWhenOnlyCommentsFail keeps comment failures out of the exit
// status end to end, not just in the RunResult unit test: the issue state itself
// is already correct, and comments are opt-in via LINEAR_COMMENTS.
func TestRunSucceedsWhenOnlyCommentsFail(t *testing.T) {
	cfg := baseTestConfig()
	cfg.Linear.CommentsEnabled = true

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       model.Fingerprint("target-a", "su2d3k-1"),
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	// A stale title on the existing issue makes the finding produce an update, and
	// therefore a change comment.
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Triage",
				Fingerprint: model.Fingerprint("target-a", "su2d3k-1"),
			},
		},
		commentBatchErr: errors.New("connection reset by peer"),
	}

	result, err := New(cfg, discardLogger(), snykdast, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FailedComments == 0 {
		t.Fatal("FailedComments = 0, want the dropped comment counted")
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0: the update itself applied", result.FailedOps)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil: only an explanatory comment was lost", err)
	}
}
