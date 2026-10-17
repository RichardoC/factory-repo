package sync

import (
	"testing"
	"time"

	"github.com/tesslio/snyk-linear-sync/internal/config"
	"github.com/tesslio/snyk-linear-sync/internal/model"
)

func TestDiagnoseDueDateExposesCreationAndIgnoreExpiryScenarios(t *testing.T) {
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
		Fingerprint:     "snyk:project-a:issue-1",
		SnykIssueID:     "issue-1",
		ProjectName:     "Project A",
		Severity:        "high",
		Status:          model.FindingOpen,
		CreatedAt:       time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		IgnoreExpiresAt: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
	}
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       "Snyk: [high] example",
		Description: "body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
		StateName:   "Todo",
		Fingerprint: finding.Fingerprint,
		Priority:    2,
	}

	diag := DiagnoseDueDate(cfg, finding, existing)

	if !diag.WouldUpdate {
		// The existing issue has no due date; the desired issue wants 2026-07-01.
		t.Fatalf("WouldUpdate = false, want true (due date missing on existing issue)")
	}

	if len(diag.Scenarios) != 2 {
		t.Fatalf("scenarios = %d, want 2 (creation and ignore expiry)", len(diag.Scenarios))
	}

	creation := diag.Scenarios[0]
	if creation.Name != "issue creation (CreatedAt + severity SLA)" {
		t.Fatalf("creation scenario name = %q, want issue creation", creation.Name)
	}
	if creation.DueDate != "2026-01-31" {
		t.Fatalf("creation due date = %q, want 2026-01-31", creation.DueDate)
	}

	ignoreExpiry := diag.Scenarios[1]
	if ignoreExpiry.Name != "ignore expiry (IgnoreExpiresAt + severity SLA)" {
		t.Fatalf("ignore expiry scenario name = %q, want ignore expiry", ignoreExpiry.Name)
	}
	if ignoreExpiry.DueDate != "2026-07-01" {
		t.Fatalf("ignore expiry due date = %q, want 2026-07-01", ignoreExpiry.DueDate)
	}

	if diag.Desired.DueDate != "2026-07-01" {
		t.Fatalf("desired due date = %q, want 2026-07-01 (ignore expiry wins)", diag.Desired.DueDate)
	}
}

func TestDiagnoseDueDateDetectsFixAvailabilityOverride(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				HighDays: 30,
			},
			Labels: config.LabelConfig{
				AwaitingFix: "triage-dependency",
			},
		},
	}
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectName: "Project A",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	existing := model.ExistingIssue{
		ID:            "existing-1",
		Identifier:    "SEC-1",
		Title:         "Snyk: [high] example",
		Description:   "body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\nmanaged_labels: triage-dependency\n-->",
		StateName:     "Backlog",
		Fingerprint:   finding.Fingerprint,
		Priority:      2,
		ManagedLabels: []string{"triage-dependency"},
	}

	diag := DiagnoseDueDate(cfg, finding, existing)

	if !diag.WasAwaitingFix {
		t.Fatal("WasAwaitingFix = false, want true")
	}
	if diag.Desired.DueDate == "" {
		t.Fatalf("desired due date is empty, want fix-availability based date")
	}
	if diag.FixAvailabilityScenario.DueDate == "" {
		t.Fatalf("fix availability scenario due date is empty")
	}
	if diag.Desired.DueDate != diag.FixAvailabilityScenario.DueDate {
		t.Fatalf("desired due date %q != fix availability scenario %q", diag.Desired.DueDate, diag.FixAvailabilityScenario.DueDate)
	}
}
