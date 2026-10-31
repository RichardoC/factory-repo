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

func readManifestT(t *testing.T, dir string, cfg Config, now time.Time) Manifest {
	t.Helper()
	m, found, err := readManifest(dir, cfg, now)
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
	m := readManifestT(t, gen1, cfg, now)
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
	m = readManifestT(t, gen2, cfg, now)
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

	// The next run sees today's and the week's totals and the merged files.
	home := t.TempDir()
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	if err := stateLoad(gen2, home, cfg, now); err != nil {
		t.Fatal(err)
	}
	if output, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT")); string(output) != "generation=3\nday_usd=3.75\nweek_usd=3.75\n" {
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
	m := readManifestT(t, out, cfg, now)
	if m.Spend["2026-10-16"] != cfg.Limits.RunCreditUSD {
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
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, manifestFile), []byte(`{"version":1,"generation":2,"merged":[],"updated":"2026-10-16T11:00:00Z","spend":{"2026-10-16":-20}}`))
	if _, _, err := readManifest(dir, cfg, now); err == nil {
		t.Error("negative spend accepted")
	}
}
func TestReadManifestRejectsOutOfRangeSpend(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	day := func(offset int) string {
		return now.Add(time.Duration(offset) * 24 * time.Hour).Format(time.DateOnly)
	}
	// Each of these is parseable as a date and a valid USD value, but the
	// write path (SpendDate, RunSpend, pruneSpend) can never produce them,
	// so the read path must not accept them from a merged bundle. The
	// fixtures carry the manifest's own updated timestamp, so what is
	// refused is the entry, not a missing write time.
	dayCap := cfg.Limits.WeeklyCreditUSD + float64(cfg.Queue.MaxParallelRuns)*maxRunSpend(cfg.Limits.RunCreditUSD)
	for name, entry := range map[string]string{
		"future day":        `"2099-01-01": 0.5`,
		"a day 30 days old": fmt.Sprintf("%q: 0.5", day(-30)),
	} {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, manifestFile), []byte(fmt.Sprintf(`{"version":1,"generation":2,"merged":[],"updated":"2026-10-16T11:00:00Z","spend":{"2026-10-16":0.5,%s}}`, entry)))
		if _, _, err := readManifest(dir, cfg, now); err == nil {
			t.Errorf("%s (%s) accepted", name, entry)
		}
	}

	// An oversized day is capped at the ceiling, not refused: the merge
	// adds spend without that bound and a config edit can lower it, so a
	// refusal would fail every run on a bundle the harness wrote. The cap
	// never loosens the hold, since the ceiling is at least the weekly
	// credit.
	for name, usd := range map[string]float64{
		"just over the ceiling":       dayCap + 0.01,
		"twice the weekly credit":     2*cfg.Limits.WeeklyCreditUSD + dayCap,
		"just under validUSD's bound": 999_999,
	} {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, manifestFile), []byte(fmt.Sprintf(`{"version":1,"generation":2,"merged":[],"updated":"2026-10-16T11:00:00Z","spend":{"2026-10-15":0.5,%q:%v}}`, day(0), usd)))
		m := readManifestT(t, dir, cfg, now)
		if m.Spend[day(0)] != dayCap || m.Spend["2026-10-15"] != 0.5 {
			t.Errorf("%s: spend = %v, want %s capped at %v and the other day unchanged", name, m.Spend, day(0), dayCap)
		}
		if week := weekSpend(m, now); week < cfg.Limits.WeeklyCreditUSD {
			t.Errorf("%s: week spend %v after the cap is under the weekly credit %v: the cap loosened the hold", name, week, cfg.Limits.WeeklyCreditUSD)
		}
	}

	// A past its own boundary day: a bundle the 2026-10-15 merge wrote,
	// keeping 2026-10-08 at pruneSpend's edge. The gate must read it back
	// (the reader's now is a day later than the writer's).
	boundary := t.TempDir()
	writeTestFile(t, filepath.Join(boundary, manifestFile), []byte(`{"version":1,"generation":2,"merged":[],"updated":"2026-10-15T23:00:00Z","spend":{"2026-10-08":0.5}}`))
	if _, _, err := readManifest(boundary, cfg, now); err != nil {
		t.Errorf("a harness-written boundary day refused: %v", err)
	}

	// A day total past the weekly credit is still a day the write path
	// produces: RunSpend believes up to 2*run_cap+1 per run, so ten runs
	// merged onto one UTC day sum past the cap ($109 > $100). It reads
	// back; only a day far past it (see above) is refused.
	over := t.TempDir()
	writeTestFile(t, filepath.Join(over, manifestFile), []byte(fmt.Sprintf(`{"version":1,"generation":2,"merged":[],"updated":"2026-10-16T11:00:00Z","spend":{"2026-10-16":%v}}`, cfg.Limits.WeeklyCreditUSD+9)))
	if _, _, err := readManifest(over, cfg, now); err != nil {
		t.Errorf("a day total of ten overlapped runs refused: %v", err)
	}

	// A day exactly at the write-path ceiling — the weekly credit plus the
	// overshoot of every parallel run in flight when the cap was crossed —
	// is still a day the queue funds.
	atCap := t.TempDir()
	writeTestFile(t, filepath.Join(atCap, manifestFile), []byte(fmt.Sprintf(`{"version":1,"generation":2,"merged":[],"updated":"2026-10-16T11:00:00Z","spend":{"2026-10-16":%v}}`, cfg.Limits.WeeklyCreditUSD+float64(cfg.Queue.MaxParallelRuns)*maxRunSpend(cfg.Limits.RunCreditUSD))))
	if _, _, err := readManifest(atCap, cfg, now); err != nil {
		t.Errorf("a day at the write-path ceiling refused: %v", err)
	}

	// A manifest of ordinary in-week entries reads back unchanged.
	dir := t.TempDir()
	ordinary := `{"version":1,"generation":4,"merged":["10-1","11-1"],"updated":"2026-10-16T11:00:00Z","spend":{"2026-10-16":1.25,"2026-10-15":0.5,"2026-10-13":2}}`
	writeTestFile(t, filepath.Join(dir, manifestFile), []byte(ordinary))
	m := readManifestT(t, dir, cfg, now)
	if m.Generation != 4 || strings.Join(m.Merged, ",") != "10-1,11-1" || len(m.Spend) != 3 ||
		m.Spend["2026-10-16"] != 1.25 || m.Spend["2026-10-15"] != 0.5 || m.Spend["2026-10-13"] != 2 {
		t.Errorf("ordinary manifest changed: %+v", m)
	}
}

// TestReadManifestRejectsBadUpdated pins the bound on the manifest's own
// write time: the spend days are checked against Updated's window, so a
// made-up future Updated would widen that window, and a manifest without a
// write time has no window at all. Either is refused. A write time a little
// ahead of the loader's clock is still a write time — the merge's clock can
// lead this load's by up to a day.
func TestReadManifestRejectsBadUpdated(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for name, updated := range map[string]string{
		"a write time over a day ahead": now.Add(25 * time.Hour).Format(time.RFC3339),
		"a week ahead":                  now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
		"no write time":                 "0001-01-01T00:00:00Z",
	} {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, manifestFile), []byte(fmt.Sprintf(`{"version":1,"generation":2,"merged":[],"updated":%q,"spend":{"2026-10-16":0.5}}`, updated)))
		if _, _, err := readManifest(dir, cfg, now); err == nil {
			t.Errorf("manifest with %s accepted", name)
		}
	}
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, manifestFile), []byte(fmt.Sprintf(`{"version":1,"generation":2,"merged":[],"updated":%q,"spend":{"2026-10-16":0.5}}`, now.Add(time.Hour).Format(time.RFC3339))))
	if _, _, err := readManifest(dir, cfg, now); err != nil {
		t.Errorf("manifest written an hour ahead of this clock refused: %v", err)
	}
}

// TestReadManifestRereadsHarnessWrittenBundle merges two runs through the
// real write path a week apart, so the second merge keeps the first run's
// day exactly at pruneSpend's boundary, then reads the bundle back a day
// later. A now-based gate refused that load (finding on #311); the manifest
// has to be read by its own write time, whatever the loader's clock says.
func TestReadManifestRereadsHarnessWrittenBundle(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEDGER_PATH", filepath.Join(t.TempDir(), "none.jsonl"))
	empty := filepath.Join(t.TempDir(), "no-bundle")
	day1 := time.Date(2026, 10, 9, 0, 30, 0, 0, time.UTC)
	run10 := pack(t, cfg, day1, empty, "10", 1.5, func(home string) {})
	gen1 := filepath.Join(t.TempDir(), "gen1")
	if err := stateMerge(empty, gen1, []string{run10}, cfg, day1); err != nil {
		t.Fatal(err)
	}
	// Seven days and an hour later the same bundle feeds a second run, whose
	// merge prunes at its own now-7d and so keeps 2026-10-09 at the boundary.
	day2 := day1.Add(7*24*time.Hour + time.Hour)
	run11 := pack(t, cfg, day2, gen1, "11", 2, func(home string) {})
	gen2 := filepath.Join(t.TempDir(), "gen2")
	if err := stateMerge(gen1, gen2, []string{run11}, cfg, day2); err != nil {
		t.Fatal(err)
	}
	day3 := day2.Add(24 * time.Hour)
	m := readManifestT(t, gen2, cfg, day3)
	if m.Spend["2026-10-09"] != 1.5 || m.Spend["2026-10-16"] != 2 {
		t.Errorf("spend = %v, want the 2026-10-09 boundary day and the new run", m.Spend)
	}
	if got := weekSpend(m, day3); got != 2 {
		t.Errorf("weekSpend = %v, want 2 (the boundary day is counted by no week)", got)
	}
}

func TestWeekSpend(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	day := func(offset int) string {
		return now.Add(time.Duration(offset) * 24 * time.Hour).Format(time.DateOnly)
	}
	if got := weekSpend(newManifest(), now); got != 0 {
		t.Errorf("empty manifest: weekSpend = %v, want 0", got)
	}
	m := newManifest()
	m.Spend[day(0)] = 1.5 // today
	m.Spend[day(-1)] = 2  // yesterday
	m.Spend[day(-6)] = 4  // the oldest day that still counts
	if got := weekSpend(m, now); got != 7.5 {
		t.Errorf("sparse days: weekSpend = %v, want 7.5", got)
	}
	m = newManifest()
	m.Spend[day(0)] = 2.5
	if got := weekSpend(m, now); got != 2.5 {
		t.Errorf("today only: weekSpend = %v, want 2.5", got)
	}
	// Entries older than the rolling week do not count; pruneSpend drops
	// them from the manifest.
	m = newManifest()
	m.Spend[day(0)] = 3
	m.Spend[day(-7)] = 100 // at pruneSpend's edge: kept, but outside the sum
	m.Spend[day(-8)] = 100 // gone after pruning
	pruneSpend(m.Spend, now)
	if _, ok := m.Spend[day(-8)]; ok {
		t.Error("pruneSpend kept an entry from 8 days ago")
	}
	if got := weekSpend(m, now); got != 3 {
		t.Errorf("old entries: weekSpend = %v, want 3", got)
	}
}

func TestStateLoadOutputsWeekSpend(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	m := newManifest()
	m.Generation = 4
	// The merge of 2026-10-15 (a day before this load) wrote this bundle;
	// it kept 2026-10-08, its own pruneSpend boundary day: a harness-written
	// entry the gate must accept even though no week counts it.
	m.Updated = time.Date(2026, 10, 15, 23, 0, 0, 0, time.UTC)
	m.Spend["2026-10-16"] = 1.5 // today
	m.Spend["2026-10-15"] = 2   // yesterday
	m.Spend["2026-10-13"] = 4   // earlier in the week
	m.Spend["2026-10-08"] = 1.5 // the 10-15 merge's boundary day: kept, counted by no week
	data, _ := json.MarshalIndent(m, "", "  ")
	writeTestFile(t, filepath.Join(dir, manifestFile), append(data, '\n'))
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	if err := stateLoad(dir, t.TempDir(), cfg, now); err != nil {
		t.Fatal(err)
	}
	output, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT"))
	if string(output) != "generation=4\nday_usd=1.5\nweek_usd=7.5\n" {
		t.Errorf("load output = %q, want day_usd 1.5 and week_usd 7.5", output)
	}
}

// TestStateMergeSeedsMergedKeysFromALegacyBundle covers a bundle written before
// issue #312: only manifest.json has dedup keys, and the first merge after the
// change must carry them into merged-keys.jsonl instead of forgetting them.
func TestStateMergeSeedsMergedKeysFromALegacyBundle(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	bundle := t.TempDir()
	writeTestFile(t, filepath.Join(bundle, manifestFile),
		[]byte(`{"version":1,"generation":3,"merged":["10-1","11-1"],"updated":"2026-10-16T11:00:00Z","spend":{"2026-10-16":1.25}}`))
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))

	// Run 10 is re-presented: the legacy window still knows it.
	run := t.TempDir()
	writeRecordRun(t, run, "10", RunRecord{RunID: "10", Attempt: "1", Mode: "issue", Stop: stopDone, Date: "2026-10-16"}, true)

	// Nothing to merge: the bundle is handed back with the seeded key set.
	out := filepath.Join(t.TempDir(), "out")
	if err := stateMerge(bundle, out, []string{run}, cfg, now); err != nil {
		t.Fatal(err)
	}
	keys, err := readMergedKeys(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "10-1,11-1" {
		t.Errorf("%s = %v, want the legacy window seeded in order", mergedFile, keys)
	}
	m := readManifestT(t, out, cfg, now)
	if m.Generation != 3 || m.Spend["2026-10-16"] != 1.25 {
		t.Errorf("no-op merge changed the bundle: %+v", m)
	}
	if output, _ := os.ReadFile(os.Getenv("GITHUB_OUTPUT")); !strings.Contains(string(output), "changed=false") {
		t.Errorf("output = %q, want changed=false", output)
	}
}

// TestStateLoadRefusesBadMergedKeys pins where a bad merged-keys.jsonl fails:
// at the load, before anything is installed — not for the first time at the
// merge, where it would fail every later run with nothing to reload
// (issue #312 review, AGENTS.md: carried data is validated on every load).
func TestStateLoadRefusesBadMergedKeys(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	bundle := t.TempDir()
	writeTestFile(t, filepath.Join(bundle, manifestFile),
		[]byte(`{"version":1,"generation":3,"merged":[],"updated":"2026-10-16T11:00:00Z","spend":{}}`))
	writeTestFile(t, filepath.Join(bundle, mergedFile), []byte("1-1\nnot-a-key\n"))
	home := t.TempDir()
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	if err := stateLoad(bundle, home, cfg, now); err == nil {
		t.Fatalf("stateLoad accepted a bundle whose %s is malformed; the load must refuse it first", mergedFile)
	}
	if _, err := os.Stat(filepath.Join(home, recordsFile)); !os.IsNotExist(err) {
		t.Error("stateLoad installed files from a bundle it refused")
	}

	// The same bundle with a well-formed key set loads.
	writeTestFile(t, filepath.Join(bundle, mergedFile), []byte("1-1\n2-2\n"))
	if err := stateLoad(bundle, home, cfg, now); err != nil {
		t.Errorf("stateLoad refused a valid %s: %v", mergedFile, err)
	}
}

// TestMergedUnion covers the `state merged` download filter: the set the
// merge decides on is the union of merged-keys.jsonl and the manifest window,
// so a legacy bundle still lists its window and no key is printed twice
// (issue #312 review; reverting this to the manifest window alone means the
// state workflow re-downloads already-merged artifacts).
func TestMergedUnion(t *testing.T) {
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, manifestFile),
		[]byte(`{"version":1,"generation":3,"merged":["1-1","2-1"],"updated":"2026-10-16T11:00:00Z","spend":{}}`))
	// A legacy bundle (no key file): the window is the whole set.
	union, err := mergedUnion(dir, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(union, ",") != "1-1,2-1" {
		t.Errorf("legacy union = %v, want the window 1-1,2-1", union)
	}
	// The key file is the dedup set; window keys it has not recorded join
	// it, and nothing is listed twice.
	writeTestFile(t, filepath.Join(dir, mergedFile), []byte("2-1\n5-1\n"))
	union, err = mergedUnion(dir, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(union, ",") != "1-1,2-1,5-1" {
		t.Errorf("union = %v, want 1-1,2-1,5-1 (window plus file, each key once)", union)
	}
	// A window that has forgotten old runs must not shrink the set: the
	// file still holds their keys.
	writeTestFile(t, filepath.Join(dir, manifestFile),
		[]byte(`{"version":1,"generation":3,"merged":["9-1"],"updated":"2026-10-16T11:00:00Z","spend":{}}`))
	union, err = mergedUnion(dir, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(union, ",") != "9-1,2-1,5-1" {
		t.Errorf("union = %v, want 9-1,2-1,5-1 (no re-download of 2-1 because the window forgot it)", union)
	}
	// A key file with a bad line is refused here too, not at the merge.
	writeTestFile(t, filepath.Join(dir, mergedFile), []byte("2-1\nbad key\n"))
	if _, err := mergedUnion(dir, cfg, now); err == nil {
		t.Error("mergedUnion accepted a malformed key file")
	}
	// A manifest that fails validation fails here too.
	writeTestFile(t, filepath.Join(dir, manifestFile),
		[]byte(`{"version":1,"generation":3,"merged":["9-1"],"updated":"2026-10-16T11:00:00Z","spend":{"2026-10-16":-5}}`))
	if _, err := mergedUnion(dir, cfg, now); err == nil {
		t.Error("mergedUnion accepted an unvalidated manifest")
	}
}
