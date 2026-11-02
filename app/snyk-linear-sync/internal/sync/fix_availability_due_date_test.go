package sync

import (
	"testing"
	"time"

	"github.com/tesslio/snyk-linear-sync/internal/model"
)

// TestFixAvailabilityDueDateUsesUTCDate pins the fix-availability due date to
// the UTC calendar date. issueDueDateFromFixAvailability used to take
// "today" from the local wall clock, so on a host whose local date differs
// from the UTC date (a container with a non-UTC TZ) a freshly fix-available
// finding got a due date one day off from what the created_at path computes
// for the same instant. Pacific/Kiritimati is UTC+14: from 10:00 UTC onward
// its local date is already the next UTC day, so a local-time implementation
// diverges from the created_at path.
func TestFixAvailabilityDueDateUsesUTCDate(t *testing.T) {
	t.Setenv("TZ", "Pacific/Kiritimati")

	cfg := minimalCfg()
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "XSS in postcss",
		IssueType:   "package_vulnerability",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Now(), // same instant the fix-availability path calls "today"
	}

	fixDue, _, _ := issueDueDateFromFixAvailability(cfg.Linear.Due, finding)
	createdDue, _, _ := issueDueDateFromCreatedAt(cfg.Linear.Due, finding)
	if fixDue == "" || createdDue == "" {
		t.Fatalf("empty due dates: fix-availability %q, created_at %q", fixDue, createdDue)
	}
	if fixDue != createdDue {
		t.Fatalf("fix-availability due date %q != created_at-path due date %q for the same instant under TZ=Pacific/Kiritimati", fixDue, createdDue)
	}
}
