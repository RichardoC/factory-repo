package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tesslio/snyk-linear-sync/internal/cache"
	"github.com/tesslio/snyk-linear-sync/internal/config"
	"github.com/tesslio/snyk-linear-sync/internal/model"
)

// errLinearWrite is the error the failing Linear client reports for every
// state-changing write: the batch call and each per-item retry alike. It
// models a Linear host that persistently rejects writes.
var errLinearWrite = errors.New("linear rejected the write")

// testFinding is one open Snyk finding in a project the snapshot lists, so a
// run against an empty Linear workspace has state-changing work to plan:
// exactly one ticket create.
func testFinding() model.Finding {
	return model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Outdated package",
		PackageName: "github.com/example/pkg",
		Severity:    "high",
		Status:      model.FindingOpen,
		IssueURL:    "https://example.test/issue-1",
		CreatedAt:   time.Date(2026, time.August, 1, 14, 0, 0, 0, time.UTC),
	}
}

// testConfig is the minimal configuration the sync service needs: one worker
// and the due-date windows, mirroring the internal/sync tests' fixtures.
func testConfig() config.Config {
	return config.Config{
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
}

// testSnyk serves the one-finding snapshot.
type testSnyk struct{}

func (testSnyk) LoadSnapshot(context.Context) (model.SnykSnapshot, error) {
	return model.SnykSnapshot{
		Findings:   []model.Finding{testFinding()},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}, nil
}

// recordingCache is an empty in-memory CacheStore: every run starts from no
// cached state and saves whatever snapshot the run produced.
type recordingCache struct {
	saved int
}

func (c *recordingCache) Load(context.Context) (cache.Snapshot, error) {
	return cache.Snapshot{}, nil
}

func (c *recordingCache) Save(context.Context, cache.Snapshot) error {
	c.saved++
	return nil
}

// failingLinear reads fine and fails every state-changing write — the batch
// call and every per-item retry — so the run cannot apply its plan. It counts
// the CreateIssues calls so the test can prove the per-item fallback ran and
// still failed, which is what makes failed_ops permanent.
type failingLinear struct {
	createCalls int
}

func (f *failingLinear) LoadSnapshot(context.Context) ([]model.ExistingIssue, error) {
	return nil, nil
}

func (f *failingLinear) ExistingFingerprints(context.Context, []string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (f *failingLinear) CreateIssues(context.Context, []model.DesiredIssue) ([]int, error) {
	f.createCalls++
	return nil, errLinearWrite
}

func (f *failingLinear) UpdateIssues(context.Context, []model.IssueUpdate) error {
	return errLinearWrite
}

func (f *failingLinear) PostComments(context.Context, []model.IssueUpdate) ([]int, error) {
	return nil, errLinearWrite
}

func (f *failingLinear) LastOpenStates(context.Context, []string) (map[string]model.WorkflowState, error) {
	return map[string]model.WorkflowState{}, nil
}

// healthyLinear reads fine, records every write, and succeeds at each. It
// exists so the nil-return arm is not vacuous: a nil from runSync that never
// attempted a write would prove nothing about a run that applied its plan.
type healthyLinear struct {
	created []model.DesiredIssue
	updated []model.IssueUpdate
}

func (h *healthyLinear) LoadSnapshot(context.Context) ([]model.ExistingIssue, error) {
	return nil, nil
}

func (h *healthyLinear) ExistingFingerprints(context.Context, []string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (h *healthyLinear) CreateIssues(_ context.Context, desired []model.DesiredIssue) ([]int, error) {
	h.created = append(h.created, desired...)
	return nil, nil
}

func (h *healthyLinear) UpdateIssues(_ context.Context, updates []model.IssueUpdate) error {
	h.updated = append(h.updated, updates...)
	return nil
}

func (h *healthyLinear) PostComments(context.Context, []model.IssueUpdate) ([]int, error) {
	return nil, nil
}

func (h *healthyLinear) LastOpenStates(context.Context, []string) (map[string]model.WorkflowState, error) {
	return map[string]model.WorkflowState{}, nil
}

// TestRunSyncFailsWhenLinearWritesRejectEveryRetry drives runSync with a
// Linear client whose writes always fail and asserts the run reports an error
// naming failed_ops, after the sync complete summary has been logged.
func TestRunSyncFailsWhenLinearWritesRejectEveryRetry(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	linear := &failingLinear{}

	err := runSync(context.Background(), testConfig(), logger, testSnyk{}, linear, &recordingCache{})
	if err == nil {
		t.Fatalf("runSync() = nil, want an error: every Linear write failed, so a nil would report a run that quietly dropped its work")
	}
	if !strings.Contains(err.Error(), "failed_ops=1") {
		t.Fatalf("runSync() error = %q, want it to name failed_ops=1", err)
	}
	if linear.createCalls < 2 {
		t.Fatalf("CreateIssues called %d time(s), want at least 2: the batch call and a per-item retry; a single call would mean failed_ops counted a write the sync never retried", linear.createCalls)
	}
	// The summary must reach the log before the exit status is decided, so the
	// counts are visible on a failing run too.
	if !strings.Contains(logs.String(), "sync complete") || !strings.Contains(logs.String(), "failed_ops=1") {
		t.Fatalf("summary log does not report the completed sync with failed_ops=1:\n%s", logs.String())
	}
}

// TestRunSyncReturnsNilWithoutFailures is the control twin: the same run
// against a Linear client whose writes succeed returns nil. It asserts a write
// was genuinely attempted and applied, so the nil is the outcome of a run that
// applied its plan, not of one that never tried.
func TestRunSyncReturnsNilWithoutFailures(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	linear := &healthyLinear{}

	err := runSync(context.Background(), testConfig(), logger, testSnyk{}, linear, &recordingCache{})
	if err != nil {
		t.Fatalf("runSync() error = %v, want nil for a run whose writes all applied", err)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1: the run must have attempted and applied a write for the nil return to mean anything", len(linear.created))
	}
	if !strings.Contains(logs.String(), "failed_ops=0") {
		t.Fatalf("summary log does not report failed_ops=0:\n%s", logs.String())
	}
}
