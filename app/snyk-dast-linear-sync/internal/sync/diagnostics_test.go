package sync

import (
	"testing"
	"time"

	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
)

func TestDiagnoseDueDateExposesCreationAndAcceptanceExpiryScenarios(t *testing.T) {
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
		IgnoreExpiresAt:   time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
	}
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       "Snyk DAST: [high] example",
		Description: "body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
		StateName:   "Todo",
		Fingerprint: finding.Fingerprint,
		Priority:    2,
	}

	diag := DiagnoseDueDate(cfg, finding, existing)

	if !diag.WouldUpdate {
		t.Fatalf("WouldUpdate = false, want true (due date missing on existing issue)")
	}

	if len(diag.Scenarios) != 2 {
		t.Fatalf("scenarios = %d, want 2 (creation and acceptance expiry)", len(diag.Scenarios))
	}

	creation := diag.Scenarios[0]
	if creation.Name != "finding creation (CreatedAt + severity SLA)" {
		t.Fatalf("creation scenario name = %q", creation.Name)
	}
	if creation.DueDate != "2026-01-31" {
		t.Fatalf("creation due date = %q, want 2026-01-31", creation.DueDate)
	}

	acceptanceExpiry := diag.Scenarios[1]
	if acceptanceExpiry.Name != "acceptance expiry (IgnoreExpiresAt + severity SLA)" {
		t.Fatalf("acceptance expiry scenario name = %q", acceptanceExpiry.Name)
	}
	if acceptanceExpiry.DueDate != "2026-07-01" {
		t.Fatalf("acceptance expiry due date = %q, want 2026-07-01", acceptanceExpiry.DueDate)
	}

	if diag.Desired.DueDate != "2026-07-01" {
		t.Fatalf("desired due date = %q, want 2026-07-01 (acceptance expiry wins)", diag.Desired.DueDate)
	}
}

func TestDiagnoseDueDateWasSnoozedWhenFindingStatusIsSnoozed(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{HighDays: 30},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetName:        "Example App",
		Severity:          "high",
		Status:            model.FindingSnoozed,
		CreatedAt:         time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       "Snyk DAST: [high] example",
		Description: "body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
		StateName:   "Todo",
		Fingerprint: finding.Fingerprint,
		Priority:    2,
	}

	diag := DiagnoseDueDate(cfg, finding, existing)

	if !diag.WasSnoozed {
		t.Fatal("WasSnoozed = false, want true")
	}
}
