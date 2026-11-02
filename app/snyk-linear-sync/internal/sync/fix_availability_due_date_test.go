package sync

import (
	"testing"
	"time"
	_ "time/tzdata" // the zones below load on any host

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
//
// The zone is set on time.Local directly: Go reads TZ only once, the first
// time the local zone is used, so t.Setenv("TZ") changes nothing once an
// earlier test in the package has used it. And the zone is chosen from the
// current UTC hour so that its calendar date differs from UTC's whenever the
// test runs: UTC+14 is a day ahead from 10:00 UTC, UTC-12 a day behind until
// 12:00 UTC.
func TestFixAvailabilityDueDateUsesUTCDate(t *testing.T) {
	name := "Pacific/Kiritimati"
	if time.Now().UTC().Hour() < 10 {
		name = "Etc/GMT+12"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	saved := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = saved })

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
		t.Fatalf("fix-availability due date %q != created_at-path due date %q for the same instant with the local zone set to %s", fixDue, createdDue, name)
	}
}
