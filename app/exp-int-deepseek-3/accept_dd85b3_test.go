package main

// Acceptance test written by the review task t-dd85b3 (issue #275). It proves
// the issue sentences against the running code rather than reading the source:
// defaults and the negative refusal go through loadConfig, dispatch goes
// through Dispatch with JSON-decoded state, decide goes through Decide,
// Execute is called, and the CLI paths go through run() with real files.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---------------- req 3: Config.Digest / MinBacklog --------------------------

func TestAcceptDigestMinBacklogJSONTag(t *testing.T) {
	f, ok := reflect.TypeOf(Digest{}).FieldByName("MinBacklog")
	if !ok {
		t.Fatalf("Digest has no field MinBacklog")
	}
	if got := f.Tag.Get("json"); got != "min_backlog" {
		t.Fatalf("MinBacklog json tag = %q, want %q", got, "min_backlog")
	}
	df, ok := reflect.TypeOf(Config{}).FieldByName("Digest")
	if !ok {
		t.Fatalf("Config has no field Digest")
	}
	if df.Tag.Get("json") != "digest" {
		t.Fatalf("Config.Digest json tag = %q, want %q", df.Tag.Get("json"), "digest")
	}
	if df.Type.NumField() != 1 {
		t.Fatalf("Digest has %d fields, want 1", df.Type.NumField())
	}
	if k := df.Type.Field(0).Tag.Get("json"); k != "min_backlog" {
		t.Fatalf("Digest field json tag = %q, want min_backlog", k)
	}
	if n := df.Type.Field(0).Name; n != "MinBacklog" {
		t.Fatalf("Digest field name = %q, want MinBacklog", n)
	}
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "jobq.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAcceptMinBacklogDefaultUnset(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, `{"queue":{"max_parallel":2}}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("unset min_backlog = %d, want 2", cfg.Digest.MinBacklog)
	}
}

func TestAcceptMinBacklogExplicitZeroDefaults(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, `{"digest":{"min_backlog":0}}`))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Digest.MinBacklog != 2 {
		t.Fatalf("explicit min_backlog 0 = %d, want 2", cfg.Digest.MinBacklog)
	}
}

func TestAcceptMinBacklogNegativeRefused(t *testing.T) {
	for _, n := range []string{"-1", "-7"} {
		_, err := loadConfig(writeCfg(t, `{"digest":{"min_backlog":`+n+`}}`))
		if err == nil {
			t.Fatalf("loadConfig min_backlog %s: no error", n)
		}
		if !strings.Contains(err.Error(), "min_backlog") {
			t.Fatalf("error %q does not name min_backlog", err)
		}
		if _, werr := withDefaults(Config{Digest: Digest{MinBacklog: -1}}); werr == nil {
			t.Fatalf("withDefaults negative: no error")
		}
	}
}

// ---------------- req 4: State.DigestInFlight -------------------------------

func TestAcceptDigestInFlightJSONTag(t *testing.T) {
	f, ok := reflect.TypeOf(State{}).FieldByName("DigestInFlight")
	if !ok {
		t.Fatalf("State has no field DigestInFlight")
	}
	if f.Type.Kind() != reflect.Bool {
		t.Fatalf("DigestInFlight kind = %v, want bool", f.Type.Kind())
	}
	if got := f.Tag.Get("json"); got != "digest_in_flight" {
		t.Fatalf("DigestInFlight json tag = %q, want %q", got, "digest_in_flight")
	}
	var st State
	if err := json.Unmarshal([]byte(`{"digest_in_flight":true}`), &st); err != nil {
		t.Fatal(err)
	}
	if !st.DigestInFlight {
		t.Fatalf(`json {"digest_in_flight":true} did not bind`)
	}
}

// ---------------- req 5: dispatch -------------------------------------------

func runs(t *testing.T, cfg Config, state string) []map[string]string {
	t.Helper()
	var st State
	if err := json.Unmarshal([]byte(state), &st); err != nil {
		t.Fatalf("state %s: %v", state, err)
	}
	return Dispatch(cfg, st)
}

func digestInputs(runs []map[string]string) int {
	n := 0
	for _, r := range runs {
		if isDigest(r) {
			n++
		}
	}
	return n
}

// isDigest reports whether r is exactly the inputs map {"digest":"true"}.
func isDigest(r map[string]string) bool {
	if len(r) != 1 {
		return false
	}
	v, ok := r["digest"]
	return ok && v == "true"
}

func encode(r map[string]string) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func TestAcceptDispatch(t *testing.T) {
	cfg := Config{Queue: Queue{MaxParallel: 2}, Digest: Digest{MinBacklog: 2}}

	// free slot after queued issues + queued count < MinBacklog -> exactly one digest
	got := runs(t, cfg, `{"queued":[7],"in_flight":0}`)
	if len(got) != 2 || !reflect.DeepEqual(got[0], map[string]string{"issue": "7"}) || !isDigest(got[1]) {
		t.Fatalf(`{"queued":[7]} -> %v, want [{issue:7} {digest:true}]`, got)
	}
	if n := digestInputs(got); n != 1 {
		t.Fatalf("digest runs = %d, want 1", n)
	}
	// the inputs map is exactly {"digest":"true"} -- one key, exact JSON
	if enc := encode(got[1]); enc != `{"digest":"true"}` {
		t.Fatalf("digest inputs = %s, want {\"digest\":\"true\"}", enc)
	}

	// queued count >= MinBacklog -> no digest
	if got := runs(t, cfg, `{"queued":[1,2,3],"in_flight":0}`); digestInputs(got) != 0 {
		t.Fatalf(`{"queued":[1,2,3]} -> %v, want no digest`, got)
	}
	// no slot free after the queued issues (max_parallel 2, 2 in flight) -> no digest
	if got := runs(t, cfg, `{"queued":[],"in_flight":2}`); len(got) != 0 {
		t.Fatalf(`{"queued":[],"in_flight":2} -> %v, want none`, got)
	}
	// slots consumed by queued issues -> none left for a digest
	if got := runs(t, cfg, `{"queued":[1,2],"in_flight":0}`); digestInputs(got) != 0 {
		t.Fatalf(`{"queued":[1,2]} -> %v, want no digest`, got)
	}
	// digest already in progress
	if got := runs(t, cfg, `{"queued":[],"in_flight":0,"digest_in_flight":true}`); len(got) != 0 {
		t.Fatalf(`digest_in_flight -> %v, want none`, got)
	}
	// at most one digest per tick even with several free slots
	if got := runs(t, cfg, `{"queued":[],"in_flight":0}`); digestInputs(got) != 1 || len(got) != 1 {
		t.Fatalf(`empty state -> %v, want exactly one digest`, got)
	}
	// a digest run takes a slot like any other run: one digest + one issue in 2 slots
	if got := runs(t, cfg, `{"queued":[1],"in_flight":0}`); len(got) != 2 {
		t.Fatalf(`{"queued":[1]} -> %v, want 2 runs`, got)
	}
}

// ---------------- req 6: decide ---------------------------------------------

func TestAcceptDecideRefusals(t *testing.T) {
	bad := []map[string]string{
		{"digest": "true", "issue": "7"},
		{"digest": "true", "review": "7"},
		{"digest": "true", "issue": "7", "review": "8"},
		{"digest": ""}, // digest key present but empty is not a digest dispatch
		{"digest": "true "},
		{"digest": "TRUE"},
		{"digest": "True"},
		{"digest": "false"},
		{"digest": "1"},
		{"digest": "yes"},
	}
	for _, in := range bad {
		if d := Decide(Event{Inputs: in}); d.Run {
			t.Fatalf("inputs %v ran: %+v, want refusal", in, d)
		}
	}
	// an empty issue string alongside digest is not an issue input, so the
	// digest runs exactly as the seed treats an empty issue on its own
	if d := Decide(Event{Inputs: map[string]string{"digest": "true", "issue": ""}}); !d.Run || d.Mode != "digest" {
		t.Fatalf("digest true with empty issue string: %+v, want digest run", d)
	}
	if d := Decide(Event{Inputs: map[string]string{"digest": "true"}}); !d.Run || d.Mode != "digest" || d.Number != 0 {
		t.Fatalf("digest true: %+v", d)
	}
}

// The seed's Decide must be reproduced exactly for issue/review inputs. The
// seed function is copied here verbatim from app/exp-int-seed/decide.go at HEAD
// (verified identical to the file by the review task) because a Go test cannot
// import the sibling module.
func seedDecide(ev Event) Decision {
	issue, review := ev.Inputs["issue"], ev.Inputs["review"]
	switch {
	case issue != "" && review != "":
		return Decision{Reason: "issue and review are exclusive"}
	case issue != "":
		n, ok := number(issue)
		if !ok {
			return Decision{Reason: "issue must be a positive number"}
		}
		return Decision{Run: true, Mode: "issue", Number: n, Reason: "issue dispatch"}
	case review != "":
		n, ok := number(review)
		if !ok {
			return Decision{Reason: "review must be a positive number"}
		}
		return Decision{Run: true, Mode: "review", Number: n, Reason: "review dispatch"}
	}
	return Decision{Reason: "dispatch needs a numeric issue or review input"}
}

func TestAcceptDecideMatchesSeedForIssueAndReview(t *testing.T) {
	table := []map[string]string{
		{"issue": "7"}, {"issue": "1"}, {"review": "3"}, {"review": "9"},
		{"issue": "0"}, {"issue": "-1"}, {"issue": "abc"}, {"issue": ""},
		{"review": "0"}, {"review": "1e3"}, {"review": "12x"},
		{"issue": "7", "review": "8"}, {"issue": "0", "review": "0"},
		{"issue": "123456789"}, {"issue": "1234567890"},
		{"issue": "007"}, {"review": "000000001"},
		{"digest": "true", "issue": "7"}, {"digest": ""}, {},
	}
	for _, in := range table {
		got, want := Decide(Event{Inputs: in}), seedDecide(Event{Inputs: in})
		// digest inputs are new behaviour; compare only the seed's own cases
		if _, isDigest := in["digest"]; isDigest {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("input %v: copy %+v != seed %+v", in, got, want)
		}
	}
	// Digest must not change the outcome of an empty/no-input event either.
	if got := Decide(Event{Inputs: map[string]string{}}); got.Run || got.Reason != "dispatch needs a numeric issue or review input" {
		t.Fatalf("empty inputs: %+v", got)
	}
}

// ---------------- req 7: execute --------------------------------------------

func TestAcceptExecuteDigest(t *testing.T) {
	got, err := Execute(Decision{Run: true, Mode: "digest"})
	if err != nil || got != "wrote digest" {
		t.Fatalf("Execute(digest) = %q, %v, want \"wrote digest\"", got, err)
	}
	if got, err := Execute(Decision{Run: true, Mode: "issue", Number: 7}); err != nil || got != "worked issue #7" {
		t.Fatalf("Execute(issue) = %q, %v -- seed behaviour changed", got, err)
	}
	if got, err := Execute(Decision{Run: true, Mode: "review", Number: 9}); err != nil || got != "reviewed PR #9" {
		t.Fatalf("Execute(review) = %q, %v -- seed behaviour changed", got, err)
	}
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := run(args, &out)
	return out.String(), err
}

func TestAcceptDecideCommandDigestOutput(t *testing.T) {
	p := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(p, []byte(`{"inputs":{"digest":"true"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "decide", p)
	if err != nil {
		t.Fatalf("run decide: %v", err)
	}
	if !strings.Contains(out, "mode=digest") {
		t.Fatalf("decide output %q lacks mode=digest", out)
	}
	if strings.Contains(out, "number=") {
		t.Fatalf("decide output %q has a number= line", out)
	}
	// issue and review keep their number= line
	for _, tc := range []struct{ body, want string }{
		{`{"inputs":{"issue":"7"}}`, "mode=issue"},
		{`{"inputs":{"review":"9"}}`, "mode=review"},
	} {
		p := filepath.Join(t.TempDir(), "e.json")
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := runCLI(t, "decide", p)
		if err != nil {
			t.Fatalf("run decide: %v", err)
		}
		if !strings.Contains(out, tc.want) || !strings.Contains(out, "number=") {
			t.Fatalf("decide %s -> %q, want %s and number=", tc.body, out, tc.want)
		}
	}
}
