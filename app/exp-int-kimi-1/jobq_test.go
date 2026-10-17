package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchFillsFreeSlots(t *testing.T) {
	cfg, _ := withDefaults(Config{})
	got := Dispatch(cfg, State{Queued: []int{4, 5, 6}, InFlight: 1})
	if len(got) != 1 || got[0]["issue"] != "4" {
		t.Fatalf("Dispatch = %v", got)
	}
}

// A digest run takes a spare slot only when the backlog is below MinBacklog
// and no digest run is already in flight.
func TestDispatchDigest(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       Config
		st        State
		wantLen   int
		wantLast  map[string]string // nil: no digest run expected
		wantNoDig bool
	}{
		{
			name:     "no queued issues, default min_backlog 2",
			cfg:      Config{},
			st:       State{},
			wantLen:  1,
			wantLast: map[string]string{"digest": "true"},
		},
		{
			name:     "backlog below min_backlog, slot free after issues",
			cfg:      Config{Queue: Queue{MaxParallel: 2}},
			st:       State{Queued: []int{4}},
			wantLen:  2,
			wantLast: map[string]string{"digest": "true"},
		},
		{
			name:      "backlog equal to min_backlog: no digest",
			cfg:       Config{},
			st:        State{Queued: []int{4, 5}},
			wantNoDig: true,
		},
		{
			name:      "backlog above min_backlog: no digest",
			cfg:       Config{Digest: Digest{MinBacklog: 3}},
			st:        State{Queued: []int{4, 5, 6}},
			wantNoDig: true,
		},
		{
			name:      "no free slot: no digest",
			cfg:       Config{},
			st:        State{InFlight: 2},
			wantNoDig: true,
		},
		{
			name:      "issues fill all slots: no digest",
			cfg:       Config{},
			st:        State{Queued: []int{4, 5, 6}},
			wantNoDig: true,
		},
		{
			name:      "digest already in flight: no digest",
			cfg:       Config{},
			st:        State{DigestInFlight: true},
			wantNoDig: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := withDefaults(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := Dispatch(cfg, tc.st)
			if tc.wantNoDig {
				for _, in := range got {
					if _, ok := in["digest"]; ok {
						t.Fatalf("unexpected digest run in %v", got)
					}
				}
				return
			}
			if len(got) != tc.wantLen {
				t.Fatalf("Dispatch = %v, want %d runs", got, tc.wantLen)
			}
			last := got[len(got)-1]
			if len(last) != 1 || last["digest"] != "true" {
				t.Fatalf("last run = %v, want exactly %v", last, tc.wantLast)
			}
			digests := 0
			for _, in := range got {
				if _, ok := in["digest"]; ok {
					digests++
				}
			}
			if digests != 1 {
				t.Fatalf("got %d digest runs in %v, want at most one", digests, got)
			}
		})
	}
}

func TestConfigDigestDefaults(t *testing.T) {
	for _, raw := range []string{`{}`, `{"digest":{}}`, `{"digest":{"min_backlog":0}}`} {
		var cfg Config
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatal(err)
		}
		cfg, err := withDefaults(cfg)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if cfg.Digest.MinBacklog != 2 {
			t.Fatalf("%s: MinBacklog = %d, want 2", raw, cfg.Digest.MinBacklog)
		}
	}
	cfg, err := withDefaults(Config{Digest: Digest{MinBacklog: 5}})
	if err != nil || cfg.Digest.MinBacklog != 5 {
		t.Fatalf("explicit 5: %d, %v", cfg.Digest.MinBacklog, err)
	}
}

func TestLoadConfigDigestNegative(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"digest":{"min_backlog":-1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(bad); err == nil || !strings.Contains(err.Error(), "digest.min_backlog") {
		t.Fatalf("loadConfig(negative) = %v, want digest.min_backlog error", err)
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -2}}); err == nil {
		t.Fatal("withDefaults(negative min_backlog) = nil error")
	}
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"digest":{"min_backlog":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(good)
	if err != nil || cfg.Digest.MinBacklog != 1 {
		t.Fatalf("loadConfig(good) = %+v, %v", cfg, err)
	}
}

func TestStateDigestInFlightJSON(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{"queued":[1],"in_flight":1,"digest_in_flight":true}`), &st); err != nil {
		t.Fatal(err)
	}
	if !st.DigestInFlight || st.InFlight != 1 || len(st.Queued) != 1 {
		t.Fatalf("State = %+v", st)
	}
}

func TestDecideIssue(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"issue": "7"}})
	if !d.Run || d.Mode != "issue" || d.Number != 7 {
		t.Fatalf("Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"issue": "7", "review": "8"}}); d.Run {
		t.Fatalf("both inputs ran: %+v", d)
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	// The digest input must be exactly "true".
	for _, bad := range []string{"TRUE", "1", "yes", "True", ""} {
		if d := Decide(Event{Inputs: map[string]string{"digest": bad}}); d.Run {
			t.Fatalf("digest=%q ran: %+v", bad, d)
		}
	}
	// digest together with issue or review is refused.
	for _, in := range []map[string]string{
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "8"},
		{"digest": "true", "issue": "7", "review": "8"},
	} {
		if d := Decide(Event{Inputs: in}); d.Run {
			t.Fatalf("%v ran: %+v", in, d)
		}
	}
}

// Issue and review runs behave exactly as before when no digest input is set.
func TestDecideUnchangedWithoutDigest(t *testing.T) {
	for _, tc := range []struct {
		inputs  map[string]string
		wantRun bool
		mode    string
		number  int
	}{
		{map[string]string{"issue": "0"}, false, "", 0},
		{map[string]string{"issue": "abc"}, false, "", 0},
		{map[string]string{"review": "12"}, true, "review", 12},
		{map[string]string{"review": "-3"}, false, "", 0},
		{map[string]string{}, false, "", 0},
	} {
		d := Decide(Event{Inputs: tc.inputs})
		if d.Run != tc.wantRun || (d.Run && (d.Mode != tc.mode || d.Number != tc.number)) {
			t.Fatalf("Decide(%v) = %+v, want run=%t mode=%s number=%d", tc.inputs, d, tc.wantRun, tc.mode, tc.number)
		}
	}
}

func TestExecute(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "review", Number: 3}); err != nil || got != "reviewed PR #3" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

// jobq decide prints mode=digest with no number= line for a digest run.
func TestCmdDecideDigest(t *testing.T) {
	dir := t.TempDir()
	ev := filepath.Join(dir, "event.json")
	if err := os.WriteFile(ev, []byte(`{"inputs":{"digest":"true"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run([]string{"decide", ev}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "mode=digest\n") {
		t.Fatalf("output %q lacks mode=digest", got)
	}
	if strings.Contains(got, "number=") {
		t.Fatalf("output %q must not print number= for a digest run", got)
	}
}

// jobq decide still prints mode and number for issue runs.
func TestCmdDecideIssue(t *testing.T) {
	dir := t.TempDir()
	ev := filepath.Join(dir, "event.json")
	if err := os.WriteFile(ev, []byte(`{"inputs":{"issue":"9"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run([]string{"decide", ev}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "mode=issue\n") || !strings.Contains(got, "number=9\n") {
		t.Fatalf("output %q lacks mode=issue/number=9", got)
	}
}

// jobq dispatch emits the digest run's inputs as exactly {"digest":"true"}.
func TestCmdDispatchDigest(t *testing.T) {
	dir := t.TempDir()
	cfgP := filepath.Join(dir, "config.json")
	stP := filepath.Join(dir, "state.json")
	if err := os.WriteFile(cfgP, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stP, []byte(`{"queued":[],"in_flight":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run([]string{"dispatch", cfgP, stP}, &out); err != nil {
		t.Fatal(err)
	}
	var inputs map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &inputs); err != nil {
		t.Fatalf("output %q is not one JSON object: %v", out.String(), err)
	}
	if len(inputs) != 1 || inputs["digest"] != "true" {
		t.Fatalf("inputs = %v, want exactly digest=true", inputs)
	}
}
