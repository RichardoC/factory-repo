package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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

func TestDecideIssue(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"issue": "7"}})
	if !d.Run || d.Mode != "issue" || d.Number != 7 {
		t.Fatalf("Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"issue": "7", "review": "8"}}); d.Run {
		t.Fatalf("both inputs ran: %+v", d)
	}
}

func TestExecute(t *testing.T) {
	if got, err := Execute(Decision{Run: true, Mode: "review", Number: 3}); err != nil || got != "reviewed PR #3" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestDigestConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := withDefaults(Config{})
	if err != nil || cfg.Digest.MinBacklog != 2 {
		t.Fatalf("withDefaults(Config{}) = %+v, %v", cfg, err)
	}
	cfg, err = withDefaults(Config{Digest: Digest{MinBacklog: 4}})
	if err != nil || cfg.Digest.MinBacklog != 4 {
		t.Fatalf("explicit min_backlog = %+v, %v", cfg, err)
	}
	if _, err := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); err == nil {
		t.Fatal("negative min_backlog was accepted")
	}

	path := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(path, []byte(`{"digest":{"min_backlog":-1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig accepted negative min_backlog")
	}
}

func TestDispatchDigest(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		st   State
		want []map[string]string
	}{
		{
			name: "spare slot and low backlog",
			cfg:  Config{Queue: Queue{MaxParallel: 2}},
			st:   State{Queued: []int{7}},
			want: []map[string]string{{"issue": "7"}, {"digest": "true"}},
		},
		{
			name: "no free slot after issues",
			cfg:  Config{Queue: Queue{MaxParallel: 1}},
			st:   State{Queued: []int{7}},
			want: []map[string]string{{"issue": "7"}},
		},
		{
			name: "backlog at threshold",
			cfg:  Config{Queue: Queue{MaxParallel: 3}},
			st:   State{Queued: []int{7, 8}},
			want: []map[string]string{{"issue": "7"}, {"issue": "8"}},
		},
		{
			name: "custom threshold",
			cfg:  Config{Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: 1}},
			st:   State{Queued: []int{7}},
			want: []map[string]string{{"issue": "7"}},
		},
		{
			name: "already in flight",
			cfg:  Config{Queue: Queue{MaxParallel: 2}},
			st:   State{DigestInFlight: true},
			want: nil,
		},
		{
			name: "no slot initially free",
			cfg:  Config{Queue: Queue{MaxParallel: 2}},
			st:   State{InFlight: 2},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := withDefaults(tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := Dispatch(cfg, tt.st); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Dispatch = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("Decide = %+v", d)
	}
	for _, inputs := range []map[string]string{
		{"digest": "True"},
		{"digest": "false"},
		{"digest": ""},
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "8"},
	} {
		if d := Decide(Event{Inputs: inputs}); d.Run {
			t.Errorf("Decide(%v) ran: %+v", inputs, d)
		}
	}
}

func TestExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute = %q, %v", got, err)
	}
}

func TestDecideDigestOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, []byte(`{"inputs":{"digest":"true"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"decide", path}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "mode=digest\n") || strings.Contains(out.String(), "number=") {
		t.Fatalf("decide output = %q", out.String())
	}
}
