package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
)

// metadataSchemaVersion invalidates cached hashes whenever the sync's managed
// behaviour changes. Bumped for: archived-issue awareness in the Linear
// snapshot, and lapsed time-limited acceptances now reopening instead of
// cancelling (both change the desired state/due date for existing findings).
const metadataSchemaVersion = "2026-08-19-snyk-dast-sync-cache-v2"

func managedSchemaSignature() string {
	return metadataSchemaVersion
}

func desiredIssueHash(desired model.DesiredIssue) string {
	statePart := string(desired.State)
	if desired.PreserveState {
		statePart += ":preserve"
	}
	// Hash DueDateBase (the raw SLA date) rather than DueDate so that any
	// presentation-level adjustment to the written due date cannot churn the
	// source hash while the underlying finding data is unchanged. The two
	// currently match, since past SLA dates are written through as-is.
	dueDateForHash := desired.DueDateBase
	if dueDateForHash == "" {
		dueDateForHash = desired.DueDate
	}
	return digestParts(
		desired.Fingerprint,
		desired.Title,
		normalizeDescriptionForCompare(desired.Description),
		dueDateForHash,
		statePart,
		strings.Join(model.NormalizeManagedLabelNames(desired.ManagedLabels), ","),
		fmt.Sprintf("%d", desired.Priority),
	)
}

func existingIssueHash(existing model.ExistingIssue) string {
	return digestParts(
		existing.Fingerprint,
		existing.Title,
		normalizeDescriptionForCompare(existing.Description),
		existing.DueDate,
		model.NormalizeWorkflowStateName(existing.StateName),
		strings.Join(model.NormalizeManagedLabelNames(existing.ManagedLabels), ","),
		strings.Join(presentManagedLabelNames(existing.Labels, existing.ManagedLabels), ","),
		fmt.Sprintf("%d", existing.Priority),
	)
}

func digestParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func presentManagedLabelNames(labels []model.IssueLabel, managed []string) []string {
	out := make([]string, 0, len(managed))
	for _, label := range model.NormalizeManagedLabelNames(managed) {
		if model.HasLabelNamed(labels, label) {
			out = append(out, label)
		}
	}
	return out
}
