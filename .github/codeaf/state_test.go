package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var sqliteHeader = []byte("SQLite format 3\x00")

func dbWith(tag string) []byte { return append(append([]byte{}, sqliteHeader...), tag...) }

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckDB(t *testing.T) {
	wal := append([]byte{0x37, 0x7f, 0x06, 0x82}, make([]byte, 28)...)
	for _, tc := range []struct {
		name    string
		db, wal []byte
		ok      bool
	}{
		{"none", nil, nil, true},
		{"db", dbWith("x"), nil, true},
		{"db and wal", dbWith("x"), wal, true},
		{"not sqlite", []byte("#!/bin/sh\nrm -rf /\n"), nil, false},
		{"wal alone", nil, wal, false},
		{"bad wal", dbWith("x"), []byte("not a wal, but long enough to pass the size check"), false},
	} {
		if err := CheckDB(tc.db, tc.wal); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
}

func TestCleanEvents(t *testing.T) {
	long := `{"x":"` + strings.Repeat("a", maxEventLineBytes) + `"}`
	in := "{\"at\":\"1\", \"model\":\"m\"}\nnot json\n[1,2]\n\n{\"at\":\"2\"}\n" + long + "\n{\"at\":\"3\"}\n"
	lines, dropped := CleanEvents([]byte(in))
	if len(lines) != 2 || lines[0] != `{"at":"1","model":"m"}` || lines[1] != `{"at":"2"}` {
		t.Errorf("lines = %q", lines)
	}
	if dropped != 3 {
		t.Errorf("dropped = %d, want 3 (not json, an array, and the over-long line ending the scan)", dropped)
	}
}

func TestMergeLedger(t *testing.T) {
	t0 := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	base := RouterLedger{
		Entries: []RouterEntry{
			{Model: "a", Class: "", Rating: 1, Count: 5, Updated: t0},
			{Model: "b", Class: "", Rating: 2, Count: 3, Updated: t0},
		},
		Aliases: map[string]string{"x": "a"},
	}
	run := RouterLedger{
		Entries: []RouterEntry{
			{Model: "a", Class: "", Rating: 9, Count: 4, Updated: t0.Add(time.Hour)}, // fewer observations: loses
			{Model: "b", Class: "", Rating: 7, Count: 3, Updated: t0.Add(time.Hour)}, // same count, newer: wins
			{Model: "c", Class: "crew", Rating: 3, Count: 1, Updated: t0},            // new: added
		},
		Aliases: map[string]string{"x": "b", "y": "c"},
	}
	got := MergeLedger(base, run)
	want := map[string]float64{"a": 1, "b": 7, "c": 3}
	if len(got.Entries) != 3 {
		t.Fatalf("entries = %+v", got.Entries)
	}
	for _, e := range got.Entries {
		if want[e.Model] != e.Rating {
			t.Errorf("%s rating = %v, want %v", e.Model, e.Rating, want[e.Model])
		}
	}
	if got.Aliases["x"] != "a" || got.Aliases["y"] != "c" {
		t.Errorf("aliases = %v", got.Aliases)
	}
	if len(base.Entries) != 2 || base.Entries[1].Rating != 2 {
		t.Errorf("base was modified: %+v", base.Entries)
	}
}

func TestMergeEvents(t *testing.T) {
	got := MergeEvents([]string{"a", "b"}, []string{"a", "b", "c", "b", "d"})
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Errorf("got %v", got)
	}
}

func TestMergeRun(t *testing.T) {
	shared := Files{Ledger: RouterLedger{Entries: []RouterEntry{}}, Events: []string{"e1"}, DB: dbWith("shared")}
	run := Files{Ledger: RouterLedger{Entries: []RouterEntry{}}, Events: []string{"e1", "e2"}, DB: dbWith("run")}

	got, how := mergeRun(shared, 3, RunMeta{RunID: "1", Attempt: "1", BaseGeneration: 3}, run)
	if how != "fast-forward" || string(got.DB) != string(run.DB) {
		t.Errorf("same generation: %s, db %q", how, got.DB)
	}

	got, how = mergeRun(shared, 4, RunMeta{RunID: "1", Attempt: "1", BaseGeneration: 3}, run)
	if !strings.HasPrefix(how, "merged") || string(got.DB) != string(shared.DB) || strings.Join(got.Events, ",") != "e1,e2" {
		t.Errorf("overlap: %s, db %q, events %v", how, got.DB, got.Events)
	}

	got, _ = mergeRun(Files{Ledger: RouterLedger{Entries: []RouterEntry{}}}, 4, RunMeta{RunID: "1", Attempt: "1", BaseGeneration: 3}, run)
	if string(got.DB) != string(run.DB) {
		t.Errorf("overlap with no shared db: db %q", got.DB)
	}
}

func TestSpendDateAndRunSpend(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for recorded, want := range map[string]string{
		"2026-10-16": "2026-10-16",
		"2026-10-15": "2026-10-15",
		"2026-10-01": "2026-10-16", // too old to be honest
		"2027-01-01": "2026-10-16", // in the future
		"yesterday":  "2026-10-16",
	} {
		if got := SpendDate(recorded, now); got != want {
			t.Errorf("SpendDate(%q) = %s, want %s", recorded, got, want)
		}
	}
	for _, tc := range []struct{ in, want float64 }{{0.25, 0.25}, {0, 0}, {-1, 5}, {1e9, 5}} {
		if got := RunSpend(tc.in, 5); got != tc.want {
			t.Errorf("RunSpend(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// fakeProxy answers the harness's /healthz like the running proxy does.
func fakeProxy(t *testing.T, spent float64) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":true,"run_spent_usd":%v}`, spent)
	}))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	t.Setenv("PROXY_PORT", u.Port())
}

// pack simulates one run job: load the bundle, let "CodeAF" change its home,
// then pack it.
func pack(t *testing.T, cfg Config, now time.Time, bundle, runID string, spent float64, change func(home string)) string {
	t.Helper()
	home := t.TempDir()
	out := filepath.Join(t.TempDir(), "codeaf-state")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	if err := stateLoad(bundle, home, cfg, now); err != nil {
		t.Fatalf("load: %v", err)
	}
	output, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT"))
	generation := strings.TrimPrefix(strings.Split(string(output), "\n")[0], "generation=")
	change(home)
	fakeProxy(t, spent)
	t.Setenv("GITHUB_RUN_ID", runID)
	t.Setenv("GITHUB_RUN_ATTEMPT", "1")
	t.Setenv("BASE_GENERATION", generation)
	if err := statePack(home, out, cfg, now); err != nil {
		t.Fatalf("pack: %v", err)
	}
	return out
}

func readManifestT(t *testing.T, dir string) Manifest {
	t.Helper()
	m, found, err := readManifest(dir)
	if err != nil || !found {
		t.Fatalf("manifest in %s: found=%v err=%v", dir, found, err)
	}
	return m
}

func TestStateRoundTripWithOverlappingRuns(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	t.Setenv("LEDGER_PATH", filepath.Join(t.TempDir(), "none.jsonl"))
	empty := filepath.Join(t.TempDir(), "no-bundle")

	// Run 10 starts from nothing and writes a db, an event and a rating.
	run10 := pack(t, cfg, now, empty, "10", 0.5, func(home string) {
		writeTestFile(t, filepath.Join(home, dbFile), dbWith("run10"))
		writeTestFile(t, filepath.Join(home, eventsFile), []byte(`{"at":"10"}`+"\n"))
		writeTestFile(t, filepath.Join(home, ledgerFile), []byte(`{"entries":[{"model":"zai-org/glm-5.3-flash","class":"","rating":1,"count":1,"updated":"2026-10-16T00:00:00Z"}]}`))
	})
	gen1 := filepath.Join(t.TempDir(), "gen1")
	if err := stateMerge(empty, gen1, []string{run10}, cfg, now); err != nil {
		t.Fatal(err)
	}
	m := readManifestT(t, gen1)
	if m.Generation != 1 || m.Spend["2026-10-16"] != 0.5 || len(m.Merged) != 1 {
		t.Fatalf("after run 10: %+v", m)
	}

	// Runs 11 and 12 both start from generation 1, at the same time.
	run11 := pack(t, cfg, now, gen1, "11", 1.25, func(home string) {
		writeTestFile(t, filepath.Join(home, dbFile), dbWith("run11"))
		f, _ := os.OpenFile(filepath.Join(home, eventsFile), os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(`{"at":"11"}` + "\n")
		f.Close()
	})
	run12 := pack(t, cfg, now, gen1, "12", 2, func(home string) {
		writeTestFile(t, filepath.Join(home, dbFile), dbWith("run12"))
		f, _ := os.OpenFile(filepath.Join(home, eventsFile), os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(`{"at":"12"}` + "\n")
		f.Close()
		// A planted symlink must not be followed into the bundle.
		os.Remove(filepath.Join(home, ledgerFile))
		os.Symlink("/etc/passwd", filepath.Join(home, ledgerFile))
	})

	// Run 12's merge was queued first; run 11's merge also sees run 12 (the
	// merge always folds in every packed run it is given).
	gen2 := filepath.Join(t.TempDir(), "gen2")
	if err := stateMerge(gen1, gen2, []string{run12, run11, run10}, cfg, now); err != nil {
		t.Fatal(err)
	}
	m = readManifestT(t, gen2)
	if m.Generation != 3 {
		t.Errorf("generation = %d, want 3 (run 10 skipped as already merged)", m.Generation)
	}
	if got := m.Spend["2026-10-16"]; got != 0.5+1.25+2 {
		t.Errorf("spend = %v, want 3.75", got)
	}
	if strings.Join(m.Merged, ",") != "10-1,11-1,12-1" {
		t.Errorf("merged = %v", m.Merged)
	}
	db, _ := os.ReadFile(filepath.Join(gen2, dbFile))
	if string(db) != string(dbWith("run11")) {
		t.Errorf("db = %q: run 11 fast-forwards, run 12 overlapped and keeps 11's", db)
	}
	events, _ := os.ReadFile(filepath.Join(gen2, eventsFile))
	if string(events) != "{\"at\":\"10\"}\n{\"at\":\"11\"}\n{\"at\":\"12\"}\n" {
		t.Errorf("events = %q", events)
	}
	ledger, _ := os.ReadFile(filepath.Join(gen2, ledgerFile))
	var l RouterLedger
	if json.Unmarshal(ledger, &l) != nil || len(l.Entries) != 1 {
		t.Errorf("ledger = %s", ledger)
	}

	// Merging the same runs again changes nothing.
	gen3 := filepath.Join(t.TempDir(), "gen3")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	if err := stateMerge(gen2, gen3, []string{run11, run12}, cfg, now); err != nil {
		t.Fatal(err)
	}
	if output, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT")); !strings.Contains(string(output), "changed=false") {
		t.Errorf("re-merge output = %q", output)
	}

	// The next run sees today's total and the merged files.
	home := t.TempDir()
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	if err := stateLoad(gen2, home, cfg, now); err != nil {
		t.Fatal(err)
	}
	if output, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT")); string(output) != "generation=3\nday_usd=3.75\n" {
		t.Errorf("load output = %q", output)
	}
}

func TestStateMergeRefusesBadRuns(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	bad := t.TempDir()
	writeTestFile(t, filepath.Join(bad, metaFile), []byte(`{"version":1,"run_id":"20","attempt":"1","base_generation":0,"date":"2026-10-16","run_usd":-100}`))
	writeTestFile(t, filepath.Join(bad, dbFile), []byte("#!/bin/sh\n"))
	noMeta := t.TempDir()
	badID := t.TempDir()
	writeTestFile(t, filepath.Join(badID, metaFile), []byte(`{"version":1,"run_id":"20; rm -rf /","attempt":"1","base_generation":0}`))

	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	if err := stateMerge(filepath.Join(t.TempDir(), "none"), out, []string{bad, noMeta, badID}, cfg, now); err != nil {
		t.Fatal(err)
	}
	m := readManifestT(t, out)
	if m.Spend["2026-10-16"] != cfg.Limits.RunUSD {
		t.Errorf("a negative spend should count as the run cap, got %v", m.Spend)
	}
	if _, err := os.Stat(filepath.Join(out, dbFile)); err == nil {
		t.Error("a file that isn't a SQLite db reached the bundle")
	}
	if strings.Join(m.Merged, ",") != "20-1" {
		t.Errorf("merged = %v", m.Merged)
	}
}

func TestReadManifestRefusesPoisonedSpend(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, manifestFile), []byte(`{"version":1,"generation":2,"merged":[],"spend":{"2026-10-16":-20}}`))
	if _, _, err := readManifest(dir); err == nil {
		t.Error("negative spend accepted")
	}
}
