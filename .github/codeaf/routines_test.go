package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedNow anchors every RoutinesDue test: the routine's clock is o.Now,
// never the wall, so the cooldown arithmetic is exact.
var fixedNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func stampBefore(hours float64) string {
	return fixedNow.Add(-time.Duration(hours * float64(time.Hour))).Format(time.RFC3339)
}

func routineRuns(routine, stamp string, extra ...runStatus) []runStatus {
	runs := []runStatus{{Status: "completed", DatabaseId: 101, DisplayTitle: "CodeAF routine " + routine + " (lane 0)", CreatedAt: stamp}}
	return append(runs, extra...)
}

func TestLoadRoutines(t *testing.T) {
	// The committed file loads and validates: it is trusted config on the
	// default branch, but validation runs on every load, not once at edit
	// time.
	rts, err := LoadRoutines()
	if err != nil {
		t.Fatalf("LoadRoutines: %v", err)
	}
	if len(rts) == 0 {
		t.Fatal("LoadRoutines: committed routines.json holds no routines")
	}
	seen := map[string]bool{}
	for _, r := range rts {
		if err := validateRoutine(r, 0); err != nil {
			t.Fatalf("committed routine %q: %v", r.Name, err)
		}
		if seen[r.Name] {
			t.Fatalf("duplicate routine name %q", r.Name)
		}
		seen[r.Name] = true
	}
	cases := []struct {
		name string
		json string
		want string // the field the error must name
	}{
		{"bad name", `{"routines":[{"name":"Bad_Name","every_hours":24,"kind":"check","command":"x"}]}`, "name"},
		{"every_hours zero", `{"routines":[{"name":"ok","every_hours":0,"kind":"check","command":"x"}]}`, "every_hours"},
		{"unknown kind", `{"routines":[{"name":"ok","every_hours":24,"kind":"cron","command":"x"}]}`, "kind"},
		{"unknown key", `{"routines":[{"name":"ok","every_hours":24,"kind":"check","command":"x","max_usd":1}]}`, "max_usd"},
		{"unknown top-level key", `{"routines":[],"extra":1}`, "extra"},
		{"check with max_usd", `{"routines":[{"name":"ok","every_hours":24,"kind":"check","command":"x","max_usd":0.5}]}`, "max_usd"},
		{"agent without max_usd", `{"routines":[{"name":"ok","every_hours":24,"kind":"agent","brief":"routines/x.md"}]}`, "max_usd"},
		{"agent without brief", `{"routines":[{"name":"ok","every_hours":24,"kind":"agent","max_usd":0.5}]}`, "brief"},
		{"agent escaping brief", `{"routines":[{"name":"ok","every_hours":24,"kind":"agent","brief":"../models.json","max_usd":0.5}]}`, "brief"},
		{"duplicate name", `{"routines":[{"name":"ok","every_hours":24,"kind":"check","command":"x"},{"name":"ok","every_hours":24,"kind":"check","command":"x"}]}`, "duplicate"},
	}
	for _, tc := range cases {
		_, err := parseRoutines([]byte(tc.json))
		if err == nil {
			t.Errorf("%s: parseRoutines succeeded, want an error naming %q", tc.name, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name the field %q", tc.name, err, tc.want)
		}
	}
}

func TestRoutinesDue(t *testing.T) {
	rt := routine{Name: "model-prices", EveryHours: 24, Kind: routineKindCheck, Command: "model-prices"}
	opts := func(cap, spent float64, mpr int) routineOptions {
		return routineOptions{WeekCap: cap, WeekSpent: spent, MaxParallelRuns: mpr, Now: fixedNow}
	}
	due := func(rts []routine, runs []runStatus, o routineOptions) []dueRoutine {
		t.Helper()
		return RoutinesDue(rts, runs, o)
	}
	// A routine with no run at all is due.
	if got := due([]routine{rt}, nil, opts(36, 0, 4)); len(got) != 1 || got[0].Name != "model-prices" || got[0].Lane != 0 {
		t.Fatalf("no run at all: got %+v, want model-prices on lane 0", got)
	}
	// Due only after every_hours: 25h is due, 1h and 23.9h are not.
	if got := due([]routine{rt}, routineRuns("model-prices", stampBefore(25)), opts(36, 0, 4)); len(got) != 1 {
		t.Fatalf("25h old: got %+v, want due", got)
	}
	for _, h := range []float64{1, 12, 23.9} {
		if got := due([]routine{rt}, routineRuns("model-prices", stampBefore(h)), opts(36, 0, 4)); len(got) != 0 {
			t.Fatalf("%.1fh old: got %+v, want held", h, got)
		}
	}
	// Exactly every_hours old: the cooldown has passed.
	if got := due([]routine{rt}, routineRuns("model-prices", stampBefore(24)), opts(36, 0, 4)); len(got) != 1 {
		t.Fatalf("exactly 24h: got %+v, want due", got)
	}
	// The spend floor holds everything, as it holds the queue.
	if got := due([]routine{rt}, nil, opts(36, queueSpendFraction*36, 4)); len(got) != 0 {
		t.Fatalf("spend floor: got %+v, want nothing due", got)
	}
	// Full lanes hold it; the routine takes the first free lane, and a
	// second due routine waits when the lanes run out.
	busy := runStatus{Status: "in_progress", DatabaseId: 7, DisplayTitle: "CodeAF #12 (lane 0)"}
	if got := due([]routine{rt}, []runStatus{busy}, opts(36, 0, 1)); len(got) != 0 {
		t.Fatalf("full lane: got %+v, want held", got)
	}
	other := routine{Name: "quota-calibration", EveryHours: 6, Kind: routineKindCheck, Command: "quota-calibration"}
	// An in-flight issue run occupies lane 0; model-prices' own cooldown
	// has passed, so it takes lane 1 and quota-calibration waits.
	busyLane0 := runStatus{Status: "in_progress", DatabaseId: 7, DisplayTitle: "CodeAF #12 (lane 0)"}
	got := due([]routine{rt, other}, append(routineRuns("model-prices", stampBefore(25)), busyLane0), opts(36, 0, 2))
	if len(got) != 1 || got[0].Name != "model-prices" || got[0].Lane != 1 {
		t.Fatalf("lane 0 taken: got %+v, want only model-prices on lane 1", got)
	}
	got = due([]routine{rt, other}, nil, opts(36, 0, 4))
	if len(got) != 2 || got[0].Lane != 0 || got[1].Lane != 1 {
		t.Fatalf("two due: got %+v, want lanes 0 and 1", got)
	}
	// A run still queued or in progress holds its routine whatever its stamp.
	fresh := runStatus{Status: "in_progress", DatabaseId: 8, DisplayTitle: "CodeAF routine quota-calibration (lane 1)", CreatedAt: stampBefore(50)}
	got = due([]routine{rt, other}, []runStatus{fresh}, opts(36, 0, 4))
	if len(got) != 1 || got[0].Name != "model-prices" {
		t.Fatalf("in-flight routine run: got %+v, want only model-prices", got)
	}
	// A routine whose last run has an unparseable stamp is not due (fail
	// closed): an unknown age must not wave a routine through.
	bad := routineRuns("model-prices", "not-a-stamp")
	if got := due([]routine{rt}, bad, opts(36, 0, 4)); len(got) != 0 {
		t.Fatalf("unparseable stamp: got %+v, want held", got)
	}
	missing := []runStatus{{Status: "completed", DatabaseId: 9, DisplayTitle: "CodeAF routine model-prices (lane 0)"}}
	if got := due([]routine{rt}, missing, opts(36, 0, 4)); len(got) != 0 {
		t.Fatalf("missing stamp: got %+v, want held", got)
	}
	// A different routine's run never holds another routine.
	if got := due([]routine{rt}, routineRuns("quota-calibration", stampBefore(1)), opts(36, 0, 4)); len(got) != 1 {
		t.Fatalf("other routine's run: got %+v, want model-prices due", got)
	}
	// The title parse is strict: other shapes are not routine runs.
	for _, title := range []string{"CodeAF plan (lane 0)", "CodeAF #12", "CodeAF routine", "CodeAF routine Model_Prices (lane 0)", "CodeAF routine model-prices (lane x)"} {
		if _, _, ok := runRoutineFields(title); ok {
			t.Errorf("runRoutineFields(%q) = ok, want false", title)
		}
	}
	if name, lane, ok := runRoutineFields("CodeAF routine model-prices (lane 2)"); !ok || name != "model-prices" || lane != 2 {
		t.Errorf("runRoutineFields(lane 2) = %q, %d, %v", name, lane, ok)
	}
	if name, lane, ok := runRoutineFields("CodeAF routine model-prices"); !ok || name != "model-prices" || lane != 0 {
		t.Errorf("runRoutineFields(no lane) = %q, %d, %v", name, lane, ok)
	}
}

// TestRoutineCatalog pins the committed routines to their implementations:
// every committed routine is either a registered check command or an agent
// routine whose brief template exists under routines/. A routine whose
// command was renamed or whose template was deleted fails here, not in a
// scheduled job.
func TestRoutineCatalog(t *testing.T) {
	rts, err := LoadRoutines()
	if err != nil {
		t.Fatalf("LoadRoutines: %v", err)
	}
	if len(rts) == 0 {
		t.Fatal("no routines committed")
	}
	for _, r := range rts {
		switch r.Kind {
		case routineKindCheck:
			if _, ok := routineChecks[r.Command]; !ok {
				t.Errorf("routine %q: check command %q is not registered", r.Name, r.Command)
			}
		case routineKindAgent:
			path := filepath.Join(harnessDir(), filepath.FromSlash(r.Brief))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("routine %q: brief template %s: %v", r.Name, path, err)
				continue
			}
			if len(strings.TrimSpace(string(data))) == 0 {
				t.Errorf("routine %q: brief template %s is empty", r.Name, path)
			}
		default:
			t.Errorf("routine %q: unknown kind %q", r.Name, r.Kind)
		}
	}
}

// TestRoutineChecksDeterministic pins the registry contract: registering a
// name twice is a programming error, and routine-check refuses an unknown
// command rather than running nothing quietly.
func TestRoutineChecksDeterministic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registerCheck twice did not panic")
		}
	}()
	registerCheck("model-prices", func() (string, error) { return "", errors.New("no") })
}

// TestRunRoutineCheckUnknown covers the CLI's refusal of an unregistered
// command name.
func TestRunRoutineCheckUnknown(t *testing.T) {
	orig := os.Args
	os.Args = []string{"harness", "routine-check", "no-such-check"}
	defer func() { os.Args = orig }()
	if err := runRoutineCheck(); err == nil || !strings.Contains(err.Error(), "unknown check command") {
		t.Errorf("runRoutineCheck: got %v, want an unknown-check-command error", err)
	}
}

// TestRoutineBrief builds the codeaf-upgrade brief: the template's text,
// the budget line and the plan output contract, capped, written to
// BRIEF_PATH and printed. A check routine and an unknown name are refused:
// a brief exists only for agent routines.
func TestRoutineBrief(t *testing.T) {
	// Read the committed files before HARNESS_DIR moves: the package
	// directory is the harness directory while the tests run.
	routinesJSON, err := os.ReadFile("routines.json")
	if err != nil {
		t.Fatal(err)
	}
	modelsJSON, err := os.ReadFile("models.json")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := os.ReadFile(filepath.Join("routines", "codeaf-upgrade.md"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("HARNESS_DIR", dir)
	// Copy the committed files into the temp harness dir, so the test
	// exercises the real template.
	if err := os.WriteFile(filepath.Join(dir, "routines.json"), routinesJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	// models.json too: budgetLine reads the credit cap from it, and the
	// brief's budget line must name the cap, not "unknown".
	if err := os.WriteFile(filepath.Join(dir, "models.json"), modelsJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "routines"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "routines", "codeaf-upgrade.md"), tmpl, 0o644); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(dir, "brief.md")
	t.Setenv("BRIEF_PATH", briefPath)
	orig := os.Args
	os.Args = []string{"harness", "routine-brief", "codeaf-upgrade"}
	out, err := captureStdout(t, runRoutineBrief)
	if err != nil {
		t.Fatalf("runRoutineBrief: %v", err)
	}
	os.Args = orig
	_ = orig
	written, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != out {
		t.Fatal("routine-brief: stdout and BRIEF_PATH differ")
	}
	for _, want := range []string{"# Routine: codeaf-upgrade", "of credit", "### Proposal:", "write no patch"} {
		if !strings.Contains(out, want) {
			t.Errorf("brief does not mention %q", want)
		}
	}
	if len(out) > maxBriefBytes {
		t.Errorf("brief is %d bytes, over the cap %d", len(out), maxBriefBytes)
	}
	for _, bad := range []string{"model-prices", "no-such-routine"} {
		os.Args = []string{"harness", "routine-brief", bad}
		err := runRoutineBrief()
		os.Args = orig
		if err == nil {
			t.Errorf("routine-brief %q: want an error", bad)
		}
	}
}
