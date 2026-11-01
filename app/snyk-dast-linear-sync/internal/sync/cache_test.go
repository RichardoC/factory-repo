package sync

import (
	"fmt"
	"testing"

	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
)

// TestExistingIssueHashSeesManagedLabelPastDefaultPageSize covers the cache
// side of the snapshot label paging fix: the snapshot used to read labels with
// no page size, so on an issue carrying more than Linear's default page of 50
// labels a managed label sitting past the boundary read as absent both before
// and after a manual removal, and the hash never changed. The complete label
// set the snapshot now returns must make the hash of the many-label issue
// match the same issue with few labels — the hash is a function of which
// managed labels are present, not of how many unmanaged labels surround them.
func TestExistingIssueHashSeesManagedLabelPastDefaultPageSize(t *testing.T) {
	base := model.ExistingIssue{
		Fingerprint:   "snyk-dast:target-1:finding-1",
		Title:         "Snyk DAST: issue-1",
		Description:   "description",
		StateName:     "Todo",
		ManagedLabels: []string{"snyk-dast-automation"},
		Priority:      3,
	}

	manyLabels := make([]model.IssueLabel, 0, 55)
	for i := range 54 {
		manyLabels = append(manyLabels, model.IssueLabel{ID: fmt.Sprintf("label-%d", i), Name: fmt.Sprintf("area-%02d", i)})
	}
	manyLabels = append(manyLabels, model.IssueLabel{ID: "label-managed", Name: "snyk-dast-automation"})

	many := base
	many.Labels = manyLabels

	few := base
	few.Labels = []model.IssueLabel{
		{ID: "label-0", Name: "area-0"},
		{ID: "label-managed", Name: "snyk-dast-automation"},
	}

	if existingIssueHash(many) == "" || existingIssueHash(few) == "" {
		t.Fatal("hashes must be non-empty")
	}
	if got, want := existingIssueHash(many), existingIssueHash(few); got != want {
		t.Fatalf("existingIssueHash(many labels) = %s, want %s: the managed label past the default page size must be seen", got, want)
	}

	// The converse guards the test itself: an issue whose managed label really
	// is absent must hash differently, otherwise the equality above is
	// meaningless.
	missing := base
	missing.Labels = []model.IssueLabel{{ID: "label-0", Name: "area-0"}}
	if existingIssueHash(missing) == existingIssueHash(few) {
		t.Fatal("an issue whose managed label is absent must hash differently from one where it is present")
	}
}
