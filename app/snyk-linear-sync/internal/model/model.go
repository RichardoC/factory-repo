package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

type FindingStatus string

const (
	FindingOpen        FindingStatus = "open"
	FindingAwaitingFix FindingStatus = "awaiting_fix"
	FindingSnoozed     FindingStatus = "snoozed"
	FindingFixed       FindingStatus = "fixed"
	FindingIgnored     FindingStatus = "ignored"
)

type Finding struct {
	Fingerprint       string
	SnykIssueID       string
	SnykIssueKey      string
	IssueType         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ProjectID         string
	ProjectName       string
	ProjectOrigin     string
	ProjectReference  string
	ProjectTargetFile string
	Repository        string
	// ProjectCluster is the Kubernetes cluster the finding's workload runs
	// in, populated only for projects with origin "kubernetes" (from the
	// v1 project detail's imageCluster field). Empty for other origins or
	// when Snyk does not report a cluster.
	ProjectCluster string
	// ProjectNamespace is the Kubernetes namespace of the finding's
	// workload, populated only for projects with origin "kubernetes".
	// Parsed from the project name, which the Snyk Kubernetes integration
	// builds as "<namespace>/<kind>.<group>/<workload>:<target>".
	ProjectNamespace string
	// ProjectClusterUnknown is set when the project has origin "kubernetes"
	// but the cluster lookup failed this run, so ProjectCluster is empty for
	// lack of data rather than because Snyk reports no cluster. Such a
	// finding has no identity for the run (see FindingIdentity).
	ProjectClusterUnknown bool
	IssueTitle            string
	Severity              string
	CVSS                  float64
	ExploitMaturity       string
	PackageName           string
	VulnerableVersion     string
	FixedVersion          string
	IssueURL              string
	IssueAPIURL           string
	Status                FindingStatus
	IntroducedThrough     string
	SourceFile            string
	SourceCommitID        string
	SourceLineStart       int
	SourceColumnStart     int
	SourceLineEnd         int
	SourceColumnEnd       int
	IgnoreExpiresAt       time.Time
	DisregardIfFixable    bool

	// LastResolvedAt is the most recent last_resolved_at Snyk reports across
	// the issue's coordinates: when Snyk last saw this issue resolved. Zero
	// means Snyk reports no resolution at all. LastResolvedAtInvalid is set
	// when a coordinate carried a non-empty value that could not be parsed;
	// callers must then treat the resolution history as unknown rather than
	// as "never resolved". The reopen guard in the sync uses both to tell a
	// ticket closed while the finding stayed continuously open (safe to
	// reopen) from one whose occurrence Snyk resolved before the issue ID
	// was reused (must get a fresh ticket).
	LastResolvedAt        time.Time
	LastResolvedAtInvalid bool

	// Issue detail fields surfaced from the Snyk REST issue resource so
	// consumers of the Linear ticket do not each need Snyk API credentials.
	// See https://github.com/tesslio/snyk-linear-sync/issues/26.
	Classes           []IssueClass
	CVEs              []string
	Description       string
	Remediation       string
	HasCoordinates    bool
	IsFixableManually bool
	IsFixableSnyk     bool
	IsFixableUpstream bool
	IsPatchable       bool
	IsPinnable        bool
	IsUpgradeable     bool
}

// IssueClass is a Snyk weakness class entry (e.g. a CWE) attached to an
// issue. The ID is the durable identifier such as "CWE-22".
type IssueClass struct {
	ID     string
	Source string
}

type SnykSnapshot struct {
	Findings           []Finding
	ProjectIDs         map[string]struct{}
	InactiveProjectIDs map[string]struct{}
	// ClusterLookupFailures counts Kubernetes projects whose cluster lookup
	// failed this run (their findings carry ProjectClusterUnknown).
	ClusterLookupFailures int
}

type IssueLabel struct {
	ID   string
	Name string
	// GroupID is the ID of the label's parent group, or "" when the label is
	// not in a group. Linear label groups are mutually exclusive: an issue may
	// carry at most one child of a group, and issueUpdate rejects a labelIds set
	// that names two ("labelIds not exclusive child labels").
	GroupID string
}

type IssueState string

const (
	StateTodo      IssueState = "todo"
	StateBacklog   IssueState = "backlog"
	StateDone      IssueState = "done"
	StateCancelled IssueState = "cancelled"
)

// WorkflowState is a concrete workflow state of the Linear team, as opposed
// to the model IssueState the sync maps onto a configured state name.
type WorkflowState struct {
	ID   string
	Name string
}

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
	// ArchivedAt is non-nil when the issue has been auto-archived by Linear.
	// Archived issues are excluded from the default Linear API response; the
	// sync includes them (via includeArchived: true) filtered to those
	// archived within a recent window so the reopen guard can still see
	// recently-closed tickets. An archived ticket is always terminal.
	ArchivedAt *time.Time
	// CreatedAt is when the Linear issue was created, or nil if Linear did
	// not report it.
	CreatedAt *time.Time
	// ClosedAt is when the Linear issue entered its current terminal state
	// (Linear's completedAt or canceledAt), or nil when it is open or Linear
	// did not report it. Only meaningful for terminal tickets.
	ClosedAt *time.Time
	// Identity is the project-independent finding identity recorded in the
	// metadata block (see FindingIdentity), or "" for tickets written before
	// the identity line existed. It lets the sync re-find a ticket after
	// Snyk recreates its project under a new project ID.
	Identity string
	// ClosedReason is the machine-readable reason the sync recorded in the
	// metadata block when it cancelled the ticket because its Snyk project
	// disappeared (ClosedReasonProjectMissing / ClosedReasonProjectDeactivated),
	// or "" when the sync did not close it for that reason.
	ClosedReason string
}

// Closed reasons the sync records in the metadata block when it cancels a
// ticket only because its Snyk project stopped reporting. They mark the
// closure as machine-made (not a fix and not a human decision), which is
// what makes the ticket safe to rebind when Snyk recreates the project.
const (
	ClosedReasonProjectMissing     = "project-missing"
	ClosedReasonProjectDeactivated = "project-deactivated"
)

type DesiredIssue struct {
	Fingerprint   string
	Title         string
	Description   string
	DueDate       string // effective due date written to Linear (floored to today if the raw SLA date is past)
	DueDateBase   string // raw SLA date from Snyk data (CreatedAt or IgnoreExpiresAt + offset); used for cache hashing so that the floor-to-today adjustment does not cause daily cache churn
	State         IssueState
	ManagedLabels []string
	// CreateOnlyLabels are stamped once, on create, and never reconciled.
	// Kept out of ManagedLabels on purpose so they never reach the
	// managed_labels: metadata and are never re-asserted or removed later.
	CreateOnlyLabels []string
	Priority         int
	PreserveState    bool
	StateReason      string
	DueDateReason    string
	LabelReasons     map[string]string // normalized label name → reason
	// DueDateUsedUpdatedAtFallback records whether DueDate/DueDateBase were
	// computed using the updated_at re-detection fallback (Snyk reusing an
	// issue ID for a new code occurrence) rather than the issue's original
	// created_at or ignore expiry. The sync's match loop uses this to keep
	// the due date sticky against an already-matched Linear ticket's due
	// date, since Snyk bumps updated_at on routine re-scans — not just
	// genuine re-detections — which would otherwise churn the due date
	// every run once the fallback triggers.
	DueDateUsedUpdatedAtFallback bool
	// Reopen marks a deliberate reuse of a terminal (Done/Cancelled) ticket
	// for a finding Snyk reports as open: a ticket the sync itself cancelled
	// because its project vanished (rebind after project recreation), or one
	// closed while Snyk kept reporting the same occurrence as continuously
	// open. Only the sync's match loop sets it, after its own safety checks;
	// ComputeDiff otherwise refuses any terminal-to-open state change.
	Reopen bool
	// RestoreState, set only with Reopen, is the open workflow state the
	// ticket was in before it was closed, read from its Linear history. A
	// reopen moves the ticket there instead of the configured open state, so
	// the triage done on it before the closure is not lost. Empty when the
	// history did not yield one; the configured open state applies then.
	RestoreState WorkflowState
}

// IssueDiff captures which managed fields changed between the existing and
// desired Linear issue. It is used to generate human-readable change
// comments posted after each update batch.
type IssueDiff struct {
	TitleChanged       bool
	TitleFrom          string
	TitleTo            string
	DescriptionChanged bool
	// MetadataOnlyDescriptionChange is set with DescriptionChanged when the
	// descriptions differ only inside the hidden metadata block (e.g. a newly
	// added identity line). The update still happens, but there is nothing
	// a human would want a change comment about.
	MetadataOnlyDescriptionChange bool
	DueDateChanged                bool
	DueDateFrom                   string
	DueDateTo                     string
	StateChanged                  bool
	StateFrom                     string
	StateTo                       string
	PriorityChanged               bool
	PriorityFrom                  int
	PriorityTo                    int
	LabelsAdded                   []string
	LabelsRemoved                 []string
	LabelsNeedUpdate              bool
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

// Fingerprint builds the dedup key the sync uses to match a Snyk finding to
// a Linear ticket. The base key is snyk:<projectID>:<issueID>, where issueID
// is Snyk's problem-type-in-project identifier (e.g. a vulnerability key),
// NOT a per-occurrence UUID. Snyk reuses the same issueID across scans and
// across code changes, so the base key alone is too coarse: when a problem
// of the same type reappears on different code, the fingerprint collides
// with an already-closed ticket and the sync reopens it.
//
// locationKey disambiguates finding instances using data Snyk already
// reports — the source file path for code (SAST) issues, or package@version
// for dependency issues. When present, a third segment is appended so each
// genuine occurrence gets its own ticket. When absent (e.g. Snyk didn't
// report coordinates), the coarse 2-segment key is returned for backward
// compatibility with existing Linear tickets.
//
// Line numbers and commit SHAs are deliberately excluded: they churn on
// every refactor and would orphan tickets. The file path / package identity
// is the stable "occurrence site."
func Fingerprint(projectID, issueID, locationKey string) string {
	if locationKey == "" {
		return fmt.Sprintf("snyk:%s:%s", projectID, issueID)
	}
	return CanonicalFingerprint(fmt.Sprintf("snyk:%s:%s:%s", projectID, issueID, locationKey))
}

// FingerprintLocation returns the location segment of a fingerprint (the
// part after snyk:<projectID>:<issueID>:), or "" for a coarse fingerprint.
func FingerprintLocation(fingerprint string) string {
	rest, ok := strings.CutPrefix(fingerprint, "snyk:")
	if !ok {
		return ""
	}
	_, rest, ok = strings.Cut(rest, ":")
	if !ok {
		return ""
	}
	_, location, _ := strings.Cut(rest, ":")
	return location
}

// identityLength is how many hex characters of the SHA-256 digest are kept
// for a finding identity: 128 bits, far beyond any collision risk at the
// scale of one Snyk org, while keeping the metadata line short.
const identityLength = 32

// FindingIdentity returns a project-independent identity for a finding,
// derived only from Snyk data: the project's origin, name, target file,
// target reference, repository and Kubernetes cluster, the issue key, and
// the fingerprint's location segment.
//
// Snyk sometimes recreates a project (same name, same target) under a new
// project ID, which also mints new issue IDs, so every fingerprint changes
// even though the findings are the same. The identity deliberately leaves
// both IDs out so it survives that recreation; the sync uses it to rebind
// the old project's tickets instead of cancelling them and creating copies.
// The issue key is Snyk's coalesced key (attributes.key, else the first
// problem ID, else the issue ID); only in the last case does the identity
// change on recreation, which merely falls back to today's behavior.
//
// The cluster matters because Snyk names Kubernetes projects
// "<namespace>/<kind>.<group>/<workload>:<target>" with no cluster in the
// name: the same workload in staging and prod would otherwise share an
// identity, so a recreation could swap tickets between clusters and a
// deleted staging project's cancelled tickets could be rebound to prod
// findings.
//
// It returns "" when the project name or issue key is missing, since the
// remaining fields are too generic to identify a finding across projects,
// and when the cluster lookup failed (ProjectClusterUnknown): an identity
// computed without the cluster would differ from the real one and could
// never be matched correctly, so no identity is better than a wrong one.
func FindingIdentity(finding Finding) string {
	name := strings.TrimSpace(finding.ProjectName)
	key := strings.TrimSpace(finding.SnykIssueKey)
	if name == "" || key == "" || finding.ProjectClusterUnknown {
		return ""
	}
	h := sha256.New()
	for _, part := range []string{
		"v1",
		strings.ToLower(strings.TrimSpace(finding.ProjectOrigin)),
		name,
		strings.TrimSpace(finding.ProjectTargetFile),
		strings.TrimSpace(finding.ProjectReference),
		strings.TrimSpace(finding.Repository),
		strings.TrimSpace(finding.ProjectCluster),
		key,
		FingerprintLocation(finding.Fingerprint),
	} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:identityLength]
}

// fingerprintEscapePattern matches the backslash escapes Linear's markdown
// serializer inserts before punctuation. Mirrors markdownEscapePattern in
// internal/sync.
var fingerprintEscapePattern = regexp.MustCompile(`\\([\\` + "`" + `*_{}\[\]()#+\-.!~])`)

// CanonicalFingerprint returns the fingerprint in the canonical form used
// for matching. Linear round-trips issue descriptions through its rich-text
// editor, which rewrites markdown-equivalent character sequences even inside
// the HTML-comment metadata block: __x__ (bold) is re-serialized as **x**,
// _x_ (italic) as *x*, and punctuation may gain backslash escapes. A stored
// fingerprint whose location segment contains such a sequence (e.g. a Python
// __main__.py path) therefore reads back different from what the sync wrote
// and never matches the fingerprint computed from the live Snyk finding, so
// every run closes the "orphaned" ticket and creates a fresh copy.
//
// Canonicalization makes matching immune to the rewrite: backslash escapes
// are stripped and every '*' becomes '_'. Asterisks do not occur in real
// file paths or package@version identities, so the mapping loses nothing,
// and it is applied symmetrically — to computed fingerprints on construction
// and to stored fingerprints on read — so even a location that genuinely
// contained '*' would still compare equal.
func CanonicalFingerprint(fingerprint string) string {
	fingerprint = fingerprintEscapePattern.ReplaceAllString(fingerprint, "$1")
	return strings.ReplaceAll(fingerprint, "*", "_")
}

// CoarseFingerprint returns the 2-segment snyk:<projectID>:<issueID> prefix
// of a fingerprint, stripping any location segment. It is used during
// migration to match new fine-grained findings against existing Linear
// tickets that still carry the old coarse fingerprint.
func CoarseFingerprint(fingerprint string) string {
	const prefix = "snyk:"
	rest := strings.TrimPrefix(fingerprint, prefix)
	// Cut twice: projectID:issueID[:location]
	first, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return fingerprint
	}
	issueID, _, _ := strings.Cut(rest, ":")
	return fmt.Sprintf("%s%s:%s", prefix, first, issueID)
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
