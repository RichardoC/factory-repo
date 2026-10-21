package sync

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/tesslio/snyk-linear-sync/internal/config"
	"github.com/tesslio/snyk-linear-sync/internal/model"
)

// emptySnapshotConfig is the configuration the empty-snapshot tests share:
// the default due-date windows and one sync worker.
func emptySnapshotConfig(dryRun bool) config.Config {
	return config.Config{
		DryRun: dryRun,
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

// managedOpenIssue is an open (non-terminal) managed ticket, the shape a
// resolved run would cancel when Snyk reports nothing at all.
func managedOpenIssue(id, identifier, fingerprint string) model.ExistingIssue {
	return model.ExistingIssue{
		ID:          id,
		Identifier:  identifier,
		Title:       "tracked issue",
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: fingerprint,
	}
}

// TestRunRefusesToResolveFromEmptySnapshot locks in the guard: a Snyk
// snapshot that lists no projects at all is not a truthful source while
// managed tickets exist, so Run refuses instead of cancelling every one of
// them. The refusal must happen before any Linear write and before the cache
// is refreshed, or the next run would skip what this one declined to resolve.
func TestRunRefusesToResolveFromEmptySnapshot(t *testing.T) {
	cfg := emptySnapshotConfig(false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// An empty snapshot: no findings, and no active or inactive projects.
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{}}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			managedOpenIssue("existing-1", "SNYK-100", "snyk:project-a:issue-1"),
			managedOpenIssue("existing-2", "SNYK-101", "snyk:project-b:issue-2"),
		},
	}
	store := &fakeCache{}

	result, err := New(cfg, logger, snyk, linear, store).Run(context.Background())
	if err == nil {
		t.Fatalf("Run() error = nil, want a refusal error for an empty Snyk snapshot with managed issues")
	}
	// The error has to tell the operator what to check, and must not name a
	// real organization, project, ticket or issue.
	for _, want := range []string{"SNYK_ORG_ID", "API key", "no projects"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Run() error = %q, want it to mention %q", err.Error(), want)
		}
	}
	if result != (RunResult{}) {
		t.Fatalf("Run() result = %#v, want the zero RunResult on refusal", result)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0 (a refused run performs no Linear write)", len(linear.updated))
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0", len(linear.created))
	}
	if len(linear.updateCallBatches) != 0 || len(linear.createCallBatches) != 0 {
		t.Fatalf("Linear writes attempted: update batches = %d, create batches = %d, want 0",
			len(linear.updateCallBatches), len(linear.createCallBatches))
	}
	if store.saved.SnykHashes != nil || store.saved.LinearHashes != nil || store.saved.SchemaSignature != "" {
		t.Fatalf("cache saved = %#v, want the cache untouched", store.saved)
	}
}

// TestRunEmptySnapshotAndNoManagedIssuesSucceeds covers the brand-new
// deployment: an empty Snyk snapshot with nothing managed yet is legitimate
// and must complete normally, cache refresh included.
func TestRunEmptySnapshotAndNoManagedIssuesSucceeds(t *testing.T) {
	cfg := emptySnapshotConfig(false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{}}
	linear := &fakeLinear{}
	store := &fakeCache{}

	result, err := New(cfg, logger, snyk, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for an empty snapshot with no managed issues", err)
	}
	if result.Findings != 0 {
		t.Fatalf("Findings = %d, want 0", result.Findings)
	}
	if result.ExistingIssues != 0 {
		t.Fatalf("ExistingIssues = %d, want 0", result.ExistingIssues)
	}
	if result.PlannedCreates != 0 || result.PlannedResolves != 0 {
		t.Fatalf("planned creates = %d, resolves = %d, want 0 and 0", result.PlannedCreates, result.PlannedResolves)
	}
	if store.saved.SchemaSignature != managedSchemaSignature() {
		t.Fatalf("cache signature = %q, want the run to complete and refresh the cache", store.saved.SchemaSignature)
	}
}

// TestRunEmptySnapshotDryRunReportsPlanWithoutError locks in the dry-run
// contract: the plan is still reported so the operator can see what a live
// run would have done, the refusal is logged at error level instead of
// returned, and nothing is written to Linear or to the cache.
func TestRunEmptySnapshotDryRunReportsPlanWithoutError(t *testing.T) {
	cfg := emptySnapshotConfig(true)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{}}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			managedOpenIssue("existing-1", "SNYK-100", "snyk:project-a:issue-1"),
			managedOpenIssue("existing-2", "SNYK-101", "snyk:project-b:issue-2"),
		},
	}
	store := &fakeCache{}

	result, err := New(cfg, logger, snyk, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil on a dry run (the refusal is logged, not returned)", err)
	}
	if result.PlannedResolves != 2 {
		t.Fatalf("PlannedResolves = %d, want 2 (the dry run still reports the plan)", result.PlannedResolves)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0 (a dry run writes nothing)", len(linear.updated))
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "SNYK_ORG_ID") {
		t.Fatalf("logs = %q, want the refusal logged at error level with what to check", logs.String())
	}
}

// TestRunAllFixedStillResolvesTicketsWithProjectsPresent covers the case the
// guard must not swallow: an organization whose issues are all fixed still
// lists its projects, so the snapshot is truthful and the open tickets move
// to the resolved state even though there are no findings.
func TestRunAllFixedStillResolvesTicketsWithProjectsPresent(t *testing.T) {
	cfg := emptySnapshotConfig(false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// No findings at all, but the project the ticket belongs to is still
	// listed: every issue of that project is fixed.
	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			managedOpenIssue("existing-1", "SNYK-100", "snyk:project-a:issue-1"),
		},
	}
	store := &fakeCache{}

	result, err := New(cfg, logger, snyk, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil when the project is still listed", err)
	}
	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateDone {
		t.Fatalf("resolved state = %q, want %q", linear.updated[0].State, model.StateDone)
	}
}
