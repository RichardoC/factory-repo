package sync

import (
	"time"

	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
)

// DueDateScenario describes one possible due date computation for a finding.
type DueDateScenario struct {
	Name    string `json:"name"`
	DueDate string `json:"due_date"`
	Base    string `json:"base"`
	Reason  string `json:"reason"`
}

// DueDateDiagnostics contains the full picture for a single managed issue and
// its matching Snyk DAST finding.
type DueDateDiagnostics struct {
	Finding                   model.Finding       `json:"finding"`
	Existing                  model.ExistingIssue `json:"existing"`
	Desired                   model.DesiredIssue  `json:"desired"`
	Diff                      *model.IssueDiff    `json:"diff"`
	WouldUpdate               bool                `json:"would_update"`
	Scenarios                 []DueDateScenario   `json:"scenarios"`
	SnoozedScenario           DueDateScenario     `json:"snoozed_scenario"`
	WasSnoozed                bool                `json:"was_snoozed"`
	SnykHash                  string              `json:"snyk_dast_hash"`
	LinearHash                string              `json:"linear_hash"`
	PendingTerminalTransition bool                `json:"pending_terminal_transition"`
}

// DiagnoseDueDate computes the desired issue, the diff against the existing
// Linear issue, and every plausible due date scenario for a single finding.
func DiagnoseDueDate(cfg config.Config, finding model.Finding, existing model.ExistingIssue) DueDateDiagnostics {
	desired := desiredIssue(cfg, finding)
	diff := ComputeDiff(existing, desired)
	snoozedDueDate, snoozedBase, snoozedReason := issueDueDateFromAcceptanceExpiry(cfg.Linear.Due, finding)

	scenarios := make([]DueDateScenario, 0, 2)
	if creationDueDate, creationBase, creationReason := issueDueDateFromCreatedAt(cfg.Linear.Due, finding); creationDueDate != "" {
		scenarios = append(scenarios, DueDateScenario{
			Name:    "finding creation (CreatedAt + severity SLA)",
			DueDate: creationDueDate,
			Base:    creationBase,
			Reason:  creationReason,
		})
	}
	if ignoreExpiryDueDate, ignoreExpiryBase, ignoreExpiryReason := issueDueDateFromAcceptanceExpiry(cfg.Linear.Due, finding); ignoreExpiryDueDate != "" {
		scenarios = append(scenarios, DueDateScenario{
			Name:    "acceptance expiry (IgnoreExpiresAt + severity SLA)",
			DueDate: ignoreExpiryDueDate,
			Base:    ignoreExpiryBase,
			Reason:  ignoreExpiryReason,
		})
	}

	return DueDateDiagnostics{
		Finding:                   finding,
		Existing:                  existing,
		Desired:                   desired,
		Diff:                      diff,
		WouldUpdate:               needsUpdate(existing, desired),
		WasSnoozed:                finding.Status == model.FindingSnoozed,
		SnoozedScenario:           dueDateScenario("acceptance expiry (IgnoreExpiresAt + severity SLA)", snoozedDueDate, snoozedBase, snoozedReason),
		Scenarios:                 scenarios,
		SnykHash:                  desiredIssueHash(desired),
		LinearHash:                existingIssueHash(existing),
		PendingTerminalTransition: pendingTerminalTransition(existing, desired),
	}
}

func dueDateScenario(name, dueDate, base, reason string) DueDateScenario {
	return DueDateScenario{
		Name:    name,
		DueDate: dueDate,
		Base:    base,
		Reason:  reason,
	}
}

func issueDueDateFromCreatedAt(dueCfg config.DueDateConfig, finding model.Finding) (string, string, string) {
	if finding.CreatedAt.IsZero() {
		return "", "", ""
	}
	createdAtUTC := finding.CreatedAt.UTC()
	baseDate := time.Date(createdAtUTC.Year(), createdAtUTC.Month(), createdAtUTC.Day(), 0, 0, 0, 0, time.UTC)
	return dueDateFromBase(baseDate, "finding creation", dueCfg, finding)
}

func issueDueDateFromAcceptanceExpiry(dueCfg config.DueDateConfig, finding model.Finding) (string, string, string) {
	if finding.IgnoreExpiresAt.IsZero() {
		return "", "", ""
	}
	expiresUTC := finding.IgnoreExpiresAt.UTC()
	baseDate := time.Date(expiresUTC.Year(), expiresUTC.Month(), expiresUTC.Day(), 0, 0, 0, 0, time.UTC)
	return dueDateFromBase(baseDate, "acceptance expiry", dueCfg, finding)
}
