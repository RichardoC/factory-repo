package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- config ---------------------------------------------------------------

func TestDigestMinBacklogDefault(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("MinBacklog = %d, want 2", cfg.Digest.MinBacklog)
	}
}

func TestDigestMinBacklogNegative(t *testing.T) {
	for _, cfg := range []Config{
		{Digest: Digest{MinBacklog: -1}},
		{MaxRetries: 3, Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: -5}},
	} {
		if _, err := withDefaults(cfg); err == nil {
			t.Fatalf("withDefaults(%+v) = nil error, want error", cfg)
		}
	}
}

func TestLoadConfigDigest(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"digest":{"min_backlog":4}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(good)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Digest.MinBacklog != 4 {
		t.Fatalf("MinBacklog = %d, want 4", cfg.Digest.MinBacklog)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"digest":{"min_backlog":-1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(bad); err == nil {
		t.Fatal("loadConfig with negative min_backlog = nil error, want error")
	}
}

// --- dispatch -------------------------------------------------------------

func TestDispatchFillsFreeSlots(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchStartsDigestWhenBacklogLow(t *testing.T) {
	cfg, _ := withDefaults(Config{}) // MaxParallel 2, MinBacklog 2
	got := Dispatch(cfg, State{Queued: []int{4}})
	if len(got) != 2 || got[0]["issue"] != "4" || got[1]["digest"] != "true" {
		t.Fatalf("Dispatch = %v", got)
	}
	if len(got[1]) != 1 {
		t.Fatalf("digest inputs = %v, want exactly {digest: true}", got[1])
	}
}

func TestDispatchDigestNeedsFreeSlot(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5}})
	for _, in := range got {
		if in["digest"] != "" {
			t.Fatalf("digest started with no free slot: %v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchDigestNeedsLowBacklog(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
}

func TestDispatchDigestNotAlreadyInFlight(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4}, DigestInFlight: true})
	for _, in := range got {
		if in["digest"] != "" {
			t.Fatalf("second digest started: %v", got)
		}
	}
}

func TestDispatchDigestInFlightJSONKey(t *testing.T) {
	var st State
	if err := unmarshalJSON([]byte(`{"queued":[1],"in_flight":0,"digest_in_flight":true}`), &st); err != nil {
		t.Fatalf("unmarshal State: %v", err)
	}
	if !st.DigestInFlight {
		t.Fatalf("State.DigestInFlight = false, want true from digest_in_flight")
	}
}

// --- decide ---------------------------------------------------------------

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
	d := Decide(Event{Inputs: map[string]string{"review": "8"}})
	if !d.Run || d.Mode != "review" || d.Number != 8 {
		t.Fatalf("Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"review": "0"}}); d.Run {
		t.Fatalf("review 0 ran: %+v", d)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" {
		t.Fatalf("Decide = %+v", d)
	}
	if d.Number != 0 {
		t.Fatalf("digest has a number: %+v", d)
	}
}

func TestDecideDigestMustBeTrue(t *testing.T) {
	// The digest input must be exactly "true": every other value is a skip
	// with a reason, including the empty string.
	for _, v := range []string{"false", "1", "True", "TRUE", "yes", " tru", "true ", ""} {
		d := Decide(Event{Inputs: map[string]string{"digest": v}})
		if d.Run {
			t.Fatalf("digest=%q ran: %+v", v, d)
		}
		if d.Reason == "" {
			t.Fatalf("digest=%q skipped without a reason: %+v", v, d)
		}
	}
}

func TestDecideDigestTrueMirrorsRun(t *testing.T) {
	// {"digest":"true"} is exactly a digest run: Run, Mode digest, Number 0
	// (a digest run has no issue or PR number) and a non-empty reason.
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 || d.Reason == "" {
		t.Fatalf("Decide = %+v, want Run=true Mode=digest Number=0 with a reason", d)
	}
}

func TestDecideIssueAndReviewUnchanged(t *testing.T) {
	// Issue and review runs keep their numbers and reasons.
	d := Decide(Event{Inputs: map[string]string{"issue": "12"}})
	if !d.Run || d.Mode != "issue" || d.Number != 12 || d.Reason == "" {
		t.Fatalf("issue Decide = %+v", d)
	}
	d = Decide(Event{Inputs: map[string]string{"review": "34"}})
	if !d.Run || d.Mode != "review" || d.Number != 34 || d.Reason == "" {
		t.Fatalf("review Decide = %+v", d)
	}
	// issue+review together is still refused, as before.
	if d := Decide(Event{Inputs: map[string]string{"issue": "1", "review": "2"}}); d.Run {
		t.Fatalf("issue+review ran: %+v", d)
	}
}

func TestDecideDigestExclusive(t *testing.T) {
	cases := []map[string]string{
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "8"},
		{"issue": "7", "digest": "true", "review": "8"},
	}
	for _, in := range cases {
		if d := Decide(Event{Inputs: in}); d.Run {
			t.Fatalf("inputs %v ran: %+v", in, d)
		}
	}
}

// --- execute --------------------------------------------------------------

func TestExecute(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "review", Number: 3}); err != nil || got != "reviewed PR #3" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
	if got, err := Execute(Decision{Run: true, Mode: "issue", Number: 5}); err != nil || got != "worked issue #5" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
	if got, err := Execute(Decision{Run: true, Mode: "digest"}); err != nil || got != "wrote digest" {
		t.Fatalf("Execute digest = %q, %v", got, err)
	}
}

// --- CLI ------------------------------------------------------------------

func TestRunDecideDigestOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs":{"digest":"true"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatalf("run decide: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "mode=digest\n") {
		t.Fatalf("output = %q, want mode=digest", got)
	}
	if strings.Contains(got, "number=") {
		t.Fatalf("output = %q, want no number= line", got)
	}
}

func TestRunDecideIssueStillPrintsNumber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs":{"issue":"7"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatalf("run decide: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "mode=issue\n") || !strings.Contains(got, "number=7\n") {
		t.Fatalf("output = %q", got)
	}
}

func TestRunDecideDigestExactOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs":{"digest":"true"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatalf("run decide: %v", err)
	}
	want := "run=true\nreason=" + Decide(Event{Inputs: map[string]string{"digest": "true"}}).Reason +
		"\nmode=digest\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want exactly %q", got, want)
	}
}

func TestRunDecideReviewStillPrintsNumber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs":{"review":"8"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatalf("run decide: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "mode=review\n") || !strings.Contains(got, "number=8\n") {
		t.Fatalf("output = %q", got)
	}
}

func TestExecuteDigestAndUnknownMode(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "digest"}); err != nil || got != "wrote digest" {
		t.Fatalf("Execute digest = %q, %v, want \"wrote digest\"", got, err)
	}
	if _, err := Execute(Decision{Run: true, Mode: "nonsense"}); err == nil {
		t.Fatal("Execute of an unknown mode = nil error, want error")
	}
}

func TestRunDispatchDigest(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "jobq.json")
	stPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stPath, []byte(`{"queued":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"dispatch", cfgPath, stPath}, &out); err != nil {
		t.Fatalf("run dispatch: %v", err)
	}
	if got := out.String(); got != "{\"digest\":\"true\"}\n" {
		t.Fatalf("output = %q", got)
	}
}
