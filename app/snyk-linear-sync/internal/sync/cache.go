package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/tesslio/snyk-linear-sync/internal/model"
)

// Bump when the managed issue rendering (title/description/labels/state
// mapping) changes so cached hashes from the old format are discarded and
// every ticket is reconciled once against the new format.
const metadataSchemaVersion = "2026-09-23-identity-closed-reason"

func managedSchemaSignature() string {
	return metadataSchemaVersion
}

func desiredIssueHash(desired model.DesiredIssue) string {
	statePart := string(desired.State)
	if desired.PreserveState {
		statePart += ":preserve"
	}
	// Use DueDateBase (raw SLA date) instead of DueDate (floored) for cache
	// stability. The floor-to-today adjustment changes daily, which would
	// cause the Snyk hash to churn for overdue issues even when the
	// underlying finding data has not changed.
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
