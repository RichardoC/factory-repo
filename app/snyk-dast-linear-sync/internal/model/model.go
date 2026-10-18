package model

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// FindingStatus is the normalized lifecycle state of a Snyk DAST finding as
// understood by the sync. Snyk DAST's API states (notfixed, fixed, invalid,
// accepted, retesting) are mapped into these canonical states.
type FindingStatus string

const (
	FindingOpen    FindingStatus = "open"
	FindingSnoozed FindingStatus = "snoozed"
	FindingFixed   FindingStatus = "fixed"
	FindingIgnored FindingStatus = "ignored"
)

// Finding is the normalized representation of a Snyk DAST vulnerability
// finding. Snyk DAST is a DAST product: findings describe vulnerabilities in a
// scanned web application or API target (URL, path, parameter, method) rather
// than source-code locations or package dependencies.
type Finding struct {
	Fingerprint       string
	SnykDASTFindingID string
	DefinitionID      string
	CreatedAt         time.Time
	LastFound         time.Time
	TargetID          string
	TargetName        string
	TargetType        string
	TargetURL         string
	TargetHost        string
	IssueTitle        string
	Severity          string
	CVSS              float64
	CWE               string
	CWEName           string
	Fix               string
	Evidence          string
	FindingURL        string
	IssueURL          string
	IssueAPIURL       string
	Status            FindingStatus
	Method            string
	Path              string
	Parameter         string
	InsertionPoint    string
	// CheckType is whether the finding was discovered by a passive check
	// (response analysis, e.g. missing security headers) or an active check
	// (exploit payloads, e.g. SQL injection, XSS). This is the Snyk DAST
	// equivalent of Snyk's multi-product "tool" dimension (Snyk Code vs
	// Snyk Open Source etc.): passive and active findings have different
	// triage workflows and "slightly different information," so the sync
	// can label them differently in Linear.
	CheckType string
	// CorrelationMarkdown holds the pre-rendered human-readable Snyk Code
	// (SAST) correlation blocks linked to this DAST finding, when Snyk DAST
	// has correlated the runtime vulnerability to one or more Snyk Code
	// source vulnerabilities. Each entry is markdown from the Snyk DAST
	// correlation endpoint describing the responsible source location
	// (repository, file, line) and the linked Snyk Code issue.
	CorrelationMarkdown []string
	// IgnoreExpiresAt is the expiration date of a Snyk DAST "accepted" finding
	// that has a time-limited acceptance. It is used as the base for the
	// SLA due date so the deadline extends to the normal severity offset from
	// when the acceptance expires.
	IgnoreExpiresAt time.Time
}

// SnykDASTSnapshot is the full source-of-truth view used by one sync run.
type SnykDASTSnapshot struct {
	Findings  []Finding
	TargetIDs map[string]struct{}
	// InactiveTargetIDs is kept for structural parity with the missing-finding
	// state logic. Snyk DAST targets are either present or deleted, so this set
	// is currently always empty; it exists so the cancellation-on-deletion
	// behavior remains explicit and forward-compatible.
	InactiveTargetIDs map[string]struct{}
}

type IssueLabel struct {
	ID   string
	Name string
}

type IssueState string

const (
	StateTodo      IssueState = "todo"
	StateBacklog   IssueState = "backlog"
	StateDone      IssueState = "done"
	StateCancelled IssueState = "cancelled"
)

type ExistingIssue struct {
	ID            string
	Identifier    string
	URL           string
	Title         string
	Description   string
	DueDate       string
	StateID       string
	StateName     string
	Fingerprint   string
	ManagedLabels []string
	Labels        []IssueLabel
	Priority      int
	// ArchivedAt is non-nil when Linear has archived the issue. Archived issues
	// are excluded from Linear's default issues query, so the sync asks for them
	// explicitly (includeArchived: true, bounded by
	// LINEAR_ARCHIVE_LOOKBACK_DAYS) to avoid recreating tickets that already
	// exist. The sync treats them as terminal and does not mutate them in
	// place; Linear's issueUnarchive would have to restore one first.
	ArchivedAt *time.Time
}

type DesiredIssue struct {
	Fingerprint   string
	Title         string
	Description   string
	DueDate       string // effective due date written to Linear
	DueDateBase   string // raw SLA date from Snyk DAST data (CreatedAt or IgnoreExpiresAt + offset); used for cache hashing. Past SLA dates are written through as-is rather than floored to today, so this currently matches DueDate.
	State         IssueState
	ManagedLabels []string
	Priority      int
	PreserveState bool
	StateReason   string
	DueDateReason string
	LabelReasons  map[string]string // normalized label name → reason
}

// IssueDiff captures which managed fields changed between the existing and
// desired Linear issue. It is used to generate human-readable change
// comments posted after each update batch.
type IssueDiff struct {
	TitleChanged       bool
	TitleFrom          string
	TitleTo            string
	DescriptionChanged bool
	DueDateChanged     bool
	DueDateFrom        string
	DueDateTo          string
	StateChanged       bool
	StateFrom          string
	StateTo            string
	PriorityChanged    bool
	PriorityFrom       int
	PriorityTo         int
	LabelsAdded        []string
	LabelsRemoved      []string
	LabelsNeedUpdate   bool
}

func (d *IssueDiff) HasChanges() bool {
	return d.TitleChanged || d.DescriptionChanged || d.DueDateChanged ||
		d.StateChanged || d.PriorityChanged || d.LabelsNeedUpdate
}

type IssueUpdate struct {
	Existing ExistingIssue
	Desired  DesiredIssue
	Diff     *IssueDiff
}

// NormalizeLabelName normalizes a label name for comparison.
func NormalizeLabelName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// NormalizeWorkflowStateName normalizes a Linear state name for comparison.
// It lowercases the value, strips whitespace, and maps common variants
// (e.g. "Canceled" → "cancelled") so state matching works regardless of
// how the Linear workspace is configured.
func NormalizeWorkflowStateName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "canceled":
		return "cancelled"
	default:
		return value
	}
}

// StateName returns the canonical state name for a model.IssueState.
func StateName(state IssueState) string {
	switch state {
	case StateTodo:
		return "todo"
	case StateBacklog:
		return "backlog"
	case StateDone:
		return "done"
	case StateCancelled:
		return "cancelled"
	default:
		return ""
	}
}

// Fingerprint builds the durable join key between a Snyk DAST target and one of
// its findings. The composite Snyk DAST finding id (<TARGET_ID>-<FINDING_ID>)
// is used as the finding component so the key is globally unique and stable.
func Fingerprint(targetID, findingID string) string {
	return fmt.Sprintf("snyk-dast:%s:%s", targetID, findingID)
}

// NormalizeManagedLabelNames deduplicates, normalizes, and sorts a set of
// label names for consistent comparison and storage.
func NormalizeManagedLabelNames(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := NormalizeLabelName(value)
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return out
}

// HasLabelNamed reports whether the label list contains a label with the
// given name, using case-insensitive normalized comparison.
func HasLabelNamed(labels []IssueLabel, name string) bool {
	name = NormalizeLabelName(name)
	if name == "" {
		return false
	}
	for _, label := range labels {
		if NormalizeLabelName(label.Name) == name {
			return true
		}
	}
	return false
}

func (i ExistingIssue) HasLabel(name string) bool {
	return HasLabelNamed(i.Labels, name)
}
