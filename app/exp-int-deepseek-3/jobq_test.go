package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- config -----------------------------------------------------------------

func TestWithDefaultsDigest(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("MinBacklog default = %d, want 2", cfg.Digest.MinBacklog)
	}
	cfg, err = withDefaults(Config{Digest: Digest{MinBacklog: 5}})
	if err != nil || cfg.Digest.MinBacklog != 5 {
		t.Fatalf("MinBacklog = %d, %v, want 5, nil", cfg.Digest.MinBacklog, err)
	}
}

func TestLoadConfigDigestNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":-1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig accepted a negative digest.min_backlog")
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("withDefaults accepted a negative digest.min_backlog")
	}
}

// A config file without a digest section still defaults.
func TestLoadConfigDigestJSONDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(path, []byte(`{"max_retries":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("MinBacklog = %d, want 2", cfg.Digest.MinBacklog)
	}
}

// --- state ------------------------------------------------------------------

func TestStateDigestInFlightJSON(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{"queued":[4],"in_flight":2,"digest_in_flight":true}`), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.InFlight != 2 {
		t.Fatalf("InFlight = %d, want 2 (the in_flight key must bind)", st.InFlight)
	}
	if !st.DigestInFlight {
		t.Fatalf("DigestInFlight = false, want true (state %+v)", st)
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"digest_in_flight":true`) {
		t.Fatalf("marshal = %s, want digest_in_flight", data)
	}
}

// --- dispatch ---------------------------------------------------------------

func TestDispatchFillsFreeSlots(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchDigestWhenBacklogLow(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 2 {
		t.Fatalf("Dispatch = %v, want issue 4 then digest", got)
	}
	if got[0]["issue"] != "4" {
		t.Fatalf("got[0] = %v, want the queued issue first", got[0])
	}
	if len(got[1]) != 1 || got[1]["digest"] != "true" {
		t.Fatalf("got[1] = %v, want exactly {\"digest\":\"true\"}", got[1])
	}
}

func TestDispatchDigestNeedsFreeSlot(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MaxParallel 2
	got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 0})
	if len(got) != 2 {
		t.Fatalf("Dispatch = %v, want 2 issues and no digest", got)
	}
	for _, in := range got {
		if _, ok := in["digest"]; ok {
			t.Fatalf("digest started with no free slot: %v", got)
		}
	}
	// A free slot left after the queue is drained is still not enough on its
	// own: the backlog must also be below MinBacklog.
	got = Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v, want only issue 4", got)
	}
}

func TestDispatchDigestBacklogThreshold(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	// len(Queued) == MinBacklog: not fewer, so no digest.
	got := Dispatch(cfg, State{Queued: []int{4, 5}, InFlight: 1})
	for _, in := range got {
		if _, ok := in["digest"]; ok {
			t.Fatalf("digest started at backlog == MinBacklog: %v", got)
		}
	}
	// len(Queued) < MinBacklog: one issue, started into a free slot, then the digest.
	got = Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 2 {
		t.Fatalf("Dispatch = %v, want 1 issue + digest", got)
	}
	last := got[len(got)-1]
	if len(last) != 1 || last["digest"] != "true" {
		t.Fatalf("last = %v, want the digest", last)
	}
}

func TestDispatchDigestInFlightBlocksSecond(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4}, DigestInFlight: true})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v, want only the queued issue", got)
	}
}

func TestDispatchDigestCustomMinBacklog(t *testing.T) {
	cfg, err := withDefaults(Config{Digest: Digest{MinBacklog: 1}})
	if err != nil {
		t.Fatal(err)
	}
	got := Dispatch(cfg, State{})
	if len(got) != 1 || got[0]["digest"] != "true" {
		t.Fatalf("Dispatch = %v, want a digest", got)
	}
	// MinBacklog 1 and one queued issue: backlog is not below it.
	got = Dispatch(cfg, State{Queued: []int{4}})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v, want only the issue", got)
	}
}

// A digest run takes a slot like any other run.
func TestDispatchDigestTakesASlot(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4}, InFlight: 0})
	if len(got) != 2 {
		t.Fatalf("Dispatch = %v, want 2 runs", got)
	}
	// A second tick with the digest under way starts nothing more.
	got = Dispatch(cfg, State{Queued: []int{4}, InFlight: 2, DigestInFlight: true})
	if len(got) != 0 {
		t.Fatalf("Dispatch = %v, want nothing with no free slot", got)
	}
	// The threshold is the queue length at the start of the tick.
	got = Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 0})
	if len(got) != 2 {
		t.Fatalf("Dispatch = %v, want 2 issues and no digest", got)
	}
}

func TestDispatchOneDigestPerTick(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{})
	digests := 0
	for _, in := range got {
		if in["digest"] == "true" {
			digests++
		}
	}
	if digests != 1 {
		t.Fatalf("Dispatch = %v, want exactly one digest", got)
	}
}

// --- decide -----------------------------------------------------------------

func TestDecideIssue(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"issue": "7"}})
	if !d.Run || d.Mode != "issue" || d.Number != 7 {
		t.Fatalf("Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"issue": "7", "review": "8"}}); d.Run {
		t.Fatalf("both inputs ran: %+v", d)
	}
}

func TestDecideReview(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"review": "3"}})
	if !d.Run || d.Mode != "review" || d.Number != 3 {
		t.Fatalf("Decide = %+v", d)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v, want run digest with number 0", d)
	}
}

func TestDecideDigestMustBeExactlyTrue(t *testing.T) {
	for _, v := range []string{"1", "yes", "TRUE", "True", "false", "0"} {
		if d := Decide(Event{Inputs: map[string]string{"digest": v}}); d.Run {
			t.Fatalf("digest %q ran: %+v", v, d)
		}
	}
}

func TestDecideDigestExclusive(t *testing.T) {
	for _, in := range []map[string]string{
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "7"},
		{"digest": "true", "issue": "7", "review": "8"},
	} {
		if d := Decide(Event{Inputs: in}); d.Run {
			t.Fatalf("inputs %v ran: %+v", in, d)
		}
	}
}

// --- execute ----------------------------------------------------------------

func TestExecute(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "review", Number: 3}); err != nil || got != "reviewed PR #3" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
	if got, err := Execute(Decision{Run: true, Mode: "issue", Number: 3}); err != nil || got != "worked issue #3" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v, want \"wrote digest\"", got, err)
	}
}

// --- main -------------------------------------------------------------------

func writeJSON(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDecideCommandDigestOutput(t *testing.T) {
	path := writeJSON(t, "event.json", `{"inputs":{"digest":"true"}}`)
	var buf bytes.Buffer
	if err := run([]string{"decide", path}, &buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "run=true\n") || !strings.Contains(out, "mode=digest\n") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, "number=") {
		t.Fatalf("digest printed a number line: %q", out)
	}
}

func TestDecideCommandIssueOutputUnchanged(t *testing.T) {
	path := writeJSON(t, "event.json", `{"inputs":{"issue":"7"}}`)
	var buf bytes.Buffer
	if err := run([]string{"decide", path}, &buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "run=true\nreason=issue dispatch\nmode=issue\nnumber=7\n"
	if buf.String() != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}

func TestDecideCommandReviewOutputUnchanged(t *testing.T) {
	path := writeJSON(t, "event.json", `{"inputs":{"review":"3"}}`)
	var buf bytes.Buffer
	if err := run([]string{"decide", path}, &buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "run=true\nreason=review dispatch\nmode=review\nnumber=3\n"
	if buf.String() != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}

func TestDispatchCommandPrintsDigestRun(t *testing.T) {
	cfgPath := writeJSON(t, "jobq.json", `{"queue":{"max_parallel":2}}`)
	stPath := writeJSON(t, "state.json", `{"queued":[4]}`)
	var buf bytes.Buffer
	if err := run([]string{"dispatch", cfgPath, stPath}, &buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "{\"issue\":\"4\"}\n{\"digest\":\"true\"}\n"
	if buf.String() != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}
