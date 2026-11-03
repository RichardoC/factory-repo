package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// finishedRunFilter extracts the jq filter the queue applies to the run
// list for the run that dispatched it (inputs.finished_run).
func finishedRunFilter(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "workflows", "codeaf-queue.yml"))
	if err != nil {
		t.Fatalf("reading codeaf-queue.yml: %v", err)
	}
	m := regexp.MustCompile(`jq --arg id "\$FINISHED_RUN" '([^']+)' > runs\.json`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal(`codeaf-queue.yml no longer filters the run list with jq --arg id "$FINISHED_RUN"; update this test`)
	}
	return m[1]
}

// TestFinishedPlanRunStillCoolsDown runs the queue's finished-run filter
// over a run list in which the plan run that dispatched this tick still
// reads as in progress, then hands the result to PlanCheck. The finished
// run's lane must count as free, and the plan run must still hold the
// cooldown: dropping it from the list let plan-check see only an older
// plan run and dispatch a second plan run straight after the first.
func TestFinishedPlanRunStillCoolsDown(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	filter := finishedRunFilter(t)
	now := time.Now().UTC()
	runs := []runStatus{
		{Status: "in_progress", DatabaseId: 200, DisplayTitle: "CodeAF plan (lane 0)", CreatedAt: now.Add(-40 * time.Minute).Format(time.RFC3339)},
		{Status: "completed", DatabaseId: 100, DisplayTitle: "CodeAF plan (lane 0)", CreatedAt: now.Add(-13 * time.Hour).Format(time.RFC3339)},
	}
	in, err := json.Marshal(runs)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("jq", "--arg", "id", "200", filter)
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jq %q: %v", filter, err)
	}
	var got []runStatus
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("filter output %q: %v", out, err)
	}
	var finished *runStatus
	for i := range got {
		if got[i].DatabaseId == 200 {
			finished = &got[i]
		}
	}
	if finished == nil {
		t.Fatal("the finished plan run was dropped from the run list, hiding it from the cooldown")
	}
	if finished.Status == "in_progress" || finished.Status == "queued" {
		t.Fatalf("the finished run still reads as %q, so its lane is not freed", finished.Status)
	}
	if _, ok, reason := PlanCheck(nil, got, planOpts()); ok || !strings.Contains(reason, "plan cooldown active (run 200") {
		t.Fatalf("PlanCheck = ok %v, %q; want the finished plan run to hold the cooldown", ok, reason)
	}
}
