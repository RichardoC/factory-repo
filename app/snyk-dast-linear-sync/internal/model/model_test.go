package model

import (
	"reflect"
	"testing"
)

// TestNormalizeWorkflowStateName pins the state mapping PROJECT.md promises:
// casing and whitespace are normalized away and the American spelling
// "canceled" maps onto Linear's "cancelled".
func TestNormalizeWorkflowStateName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "canceled american spelling", input: "Canceled", want: "cancelled"},
		{name: "cancelled padded and uppercase", input: " CANCELLED ", want: "cancelled"},
		{name: "already normalized", input: "Done", want: "done"},
		{name: "unmanaged state passes through", input: "In Progress", want: "in progress"},
		{name: "empty stays empty", input: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeWorkflowStateName(tt.input); got != tt.want {
				t.Errorf("NormalizeWorkflowStateName(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestNormalizeManagedLabelNames proves the dedupe + sort + normalize used in
// every hash and metadata block, and that nothing normalizes to an empty
// result.
func TestNormalizeManagedLabelNames(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name:  "dedupe, trim, lowercase and sort",
			input: []string{"B", " a ", "b", ""},
			want:  []string{"a", "b"},
		},
		{
			name:  "empty input yields nil",
			input: []string{},
			want:  nil,
		},
		{
			name:  "only blank values yields nil",
			input: []string{"", "   "},
			want:  nil,
		},
		{
			name:  "nil input yields nil",
			input: nil,
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeManagedLabelNames(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NormalizeManagedLabelNames(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestHasLabelNamedIsCaseInsensitive proves label lookup uses the normalized
// comparison, so callers do not have to guess at Linear's casing.
func TestHasLabelNamedIsCaseInsensitive(t *testing.T) {
	labels := []IssueLabel{
		{ID: "label-1", Name: "Snyk DAST"},
		{ID: "label-2", Name: "Pinned"},
	}

	if !HasLabelNamed(labels, "snyk dast") {
		t.Error("HasLabelNamed should find \"Snyk DAST\" by its lowercase name")
	}
	if !HasLabelNamed(labels, "  PINNED ") {
		t.Error("HasLabelNamed should find \"Pinned\" ignoring case and padding")
	}
	if HasLabelNamed(labels, "unmanaged") {
		t.Error("HasLabelNamed should not find a label that is not in the list")
	}
	if HasLabelNamed(labels, "") {
		t.Error("HasLabelNamed with an empty name should be false, never match anything")
	}
}

// TestFingerprintFormat pins the durable join key format every existing
// ticket's fingerprint depends on; changing it would orphan existing tickets.
func TestFingerprintFormat(t *testing.T) {
	if got, want := Fingerprint("project-a", "100"), "snyk-dast:project-a:100"; got != want {
		t.Errorf("Fingerprint = %q; want %q", got, want)
	}
}

// TestStateNameRoundTrip proves each canonical state's name is the state
// constant itself, and unknown states get no name.
func TestStateNameRoundTrip(t *testing.T) {
	for _, state := range []IssueState{StateTodo, StateBacklog, StateDone, StateCancelled} {
		if got := StateName(state); got != string(state) {
			t.Errorf("StateName(%q) = %q; want the constant itself", state, got)
		}
	}
	if got := StateName(IssueState("unknown")); got != "" {
		t.Errorf("StateName(unknown) = %q; want \"\"", got)
	}
}

// TestIssueDiffHasChangesPerField proves each managed field alone flips
// HasChanges, so a diff that changed only one field is still reported.
func TestIssueDiffHasChangesPerField(t *testing.T) {
	tests := []struct {
		name  string
		stamp func(*IssueDiff)
	}{
		{name: "TitleChanged", stamp: func(d *IssueDiff) { d.TitleChanged = true }},
		{name: "DescriptionChanged", stamp: func(d *IssueDiff) { d.DescriptionChanged = true }},
		{name: "DueDateChanged", stamp: func(d *IssueDiff) { d.DueDateChanged = true }},
		{name: "StateChanged", stamp: func(d *IssueDiff) { d.StateChanged = true }},
		{name: "PriorityChanged", stamp: func(d *IssueDiff) { d.PriorityChanged = true }},
		{name: "LabelsNeedUpdate", stamp: func(d *IssueDiff) { d.LabelsNeedUpdate = true }},
	}

	if (&IssueDiff{}).HasChanges() {
		t.Error("an empty diff should not report changes")
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff := &IssueDiff{}
			tt.stamp(diff)
			if !diff.HasChanges() {
				t.Errorf("%s alone should flip HasChanges", tt.name)
			}
		})
	}
}
