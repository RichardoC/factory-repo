package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecideDigest(t *testing.T) {
	d := Decide(Event{Inputs: map[string]string{"digest": "true"}})
	if !d.Run || d.Mode != "digest" || d.Number != 0 || d.Reason != "digest dispatch" {
		t.Fatalf("Decide = %+v", d)
	}
}

func TestDecideDigestRejectsNonTrue(t *testing.T) {
	for _, v := range []string{"1", "TRUE", "true ", "yes"} {
		if d := Decide(Event{Inputs: map[string]string{"digest": v}}); d.Run {
			t.Fatalf("digest=%q ran: %+v", v, d)
		}
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": ""}}); d.Run {
		t.Fatalf("empty digest ran: %+v", d)
	}
}

func TestDecideDigestExclusive(t *testing.T) {
	for _, in := range []map[string]string{
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "7"},
	} {
		if d := Decide(Event{Inputs: in}); d.Run {
			t.Fatalf("%v ran: %+v", in, d)
		}
	}
}

func TestDecideIssueReviewUnchanged(t *testing.T) {
	if d := Decide(Event{Inputs: map[string]string{"issue": "7"}}); !d.Run || d.Mode != "issue" || d.Number != 7 || d.Reason != "issue dispatch" {
		t.Fatalf("issue Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"review": "7"}}); !d.Run || d.Mode != "review" || d.Number != 7 || d.Reason != "review dispatch" {
		t.Fatalf("review Decide = %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"issue": "7", "review": "8"}}); d.Run {
		t.Fatalf("both ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"issue": "0"}}); d.Run {
		t.Fatalf("issue 0 ran: %+v", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"other": "x"}}); d.Run {
		t.Fatalf("unknown input ran: %+v", d)
	}
}

func runDecide(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "EVENT.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := run([]string{"decide", path}, &buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	return buf.String()
}

func TestRunDecideOutput(t *testing.T) {
	got := runDecide(t, `{"inputs":{"digest":"true"}}`)
	want := "run=true\nreason=digest dispatch\nmode=digest\n"
	if got != want {
		t.Fatalf("digest output = %q, want %q", got, want)
	}
	if strings.Contains(got, "number=") {
		t.Fatalf("digest output has a number line: %q", got)
	}

	got = runDecide(t, `{"inputs":{"issue":"7"}}`)
	want = "run=true\nreason=issue dispatch\nmode=issue\nnumber=7\n"
	if got != want {
		t.Fatalf("issue output = %q, want %q", got, want)
	}
}
