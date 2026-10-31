package main

// Shared CodeAF state: the files in $CODEAF_HOME that carry what one run
// learned into the next (the memory db, the router's event log and model
// ratings), plus the rolling week's credit spend.
//
// One bundle is shared by every run. It lives in the Actions cache and only
// the codeaf-state workflow writes it:
//
//	run job        harness state load   restores the latest bundle into $CODEAF_HOME
//	               harness state pack   after CodeAF, packs what it left (an artifact)
//	codeaf-state   harness state merge  folds every packed run not merged yet into
//	                                    a new bundle, which it saves to the cache
//
// Runs can overlap, so the merge decides per run: a run that started from the
// current bundle replaces it (fast-forward); a run that started from an older
// one is merged into it (see mergeRun). Spend is always added, never replaced.
//
// The run job's files were written while the agent had a shell, so a packed
// run is untrusted input: every file is checked here before it is merged, and
// nothing in a bundle is ever executed.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	stateVersion = 1

	manifestFile = "manifest.json"
	metaFile     = "meta.json"
	ledgerFile   = "router-ledger.json"
	eventsFile   = "router-events.jsonl"
	dbFile       = "graph.db"
	walFile      = "graph.db-wal"

	maxDBBytes        = 100 << 20
	maxEventsBytes    = 20 << 20
	maxEventLineBytes = 64 << 10
	maxEventLines     = 20_000
	maxMetaBytes      = 64 << 10
	maxMergedRuns     = 500
	spendDays         = 7
)

// Manifest is a shared bundle's manifest.json.
type Manifest struct {
	Version    int                `json:"version"`
	Generation int                `json:"generation"`
	Merged     []string           `json:"merged"` // "<run id>-<attempt>", newest last
	Spend      map[string]float64 `json:"spend"`  // UTC date -> credit USD
	Updated    time.Time          `json:"updated"`
}

// RunMeta is a packed run's meta.json.
type RunMeta struct {
	Version        int    `json:"version"`
	RunID          string `json:"run_id"`
	Attempt        string `json:"attempt"`
	BaseGeneration int    `json:"base_generation"`
	Date           string `json:"date"`
	// RunUSD is what the run cost in credit dollars; the JSON key stays
	// run_usd so packed artifacts from before the rework still merge.
	RunUSD float64 `json:"run_usd"`
}

func (m RunMeta) key() string { return m.RunID + "-" + m.Attempt }

func digits(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- file checks

// readRegular reads path if it is a regular file (not a symlink) of at most
// limit bytes. A missing file is (nil, nil).
func readRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s is %d bytes, over the %d limit", path, info.Size(), limit)
	}
	return os.ReadFile(path)
}

// CheckDB accepts a SQLite database file and its optional write-ahead log by
// their headers. They are copied as opaque files and opened only by CodeAF.
func CheckDB(db, wal []byte) error {
	if db == nil {
		if wal != nil {
			return errors.New("a write-ahead log without its database")
		}
		return nil
	}
	if !bytes.HasPrefix(db, []byte("SQLite format 3\x00")) {
		return errors.New("graph.db is not a SQLite database")
	}
	if wal != nil && len(wal) > 0 {
		if len(wal) < 32 || !(bytes.HasPrefix(wal, []byte{0x37, 0x7f, 0x06, 0x82}) || bytes.HasPrefix(wal, []byte{0x37, 0x7f, 0x06, 0x83})) {
			return errors.New("graph.db-wal is not a SQLite write-ahead log")
		}
	}
	return nil
}

// CleanEvents keeps the lines of router-events.jsonl that are JSON objects of
// a bounded size, re-encoded compactly, and at most the last maxEventLines.
func CleanEvents(data []byte) ([]string, int) {
	var lines []string
	dropped := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, maxEventLineBytes+1), maxEventLineBytes+1)
	for scanner.Scan() {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			dropped++
			continue
		}
		var compact bytes.Buffer
		if json.Compact(&compact, raw) != nil {
			dropped++
			continue
		}
		lines = append(lines, compact.String())
	}
	if scanner.Err() != nil {
		// An over-long line ends the scan; what follows it is dropped.
		dropped++
	}
	if len(lines) > maxEventLines {
		dropped += len(lines) - maxEventLines
		lines = lines[len(lines)-maxEventLines:]
	}
	return lines, dropped
}

// Files are the state files of a bundle or a packed run, checked.
type Files struct {
	Ledger  RouterLedger
	Events  []string
	Records []RunRecord
	DB      []byte
	WAL     []byte
}

// readFiles reads and checks the state files in dir. With strict (the shared
// bundle, which only the codeaf-state workflow writes) any bad file is an
// error. Otherwise (a packed run, written while the agent had a shell) a bad
// file is left out with a warning and the rest is kept.
func readFiles(dir string, models []Model, strict bool) (Files, error) {
	f := Files{Ledger: RouterLedger{Entries: []RouterEntry{}}}
	bad := func(err error) error {
		if strict {
			return err
		}
		log.Printf("warning: %s: left out: %v", dir, err)
		return nil
	}
	ledger, err := readRegular(filepath.Join(dir, ledgerFile), maxLedgerBytes)
	if err != nil {
		if err := bad(err); err != nil {
			return f, err
		}
	} else if ledger != nil {
		var dropped int
		if f.Ledger, dropped = SanitizeLedger(ledger, models); dropped > 0 {
			log.Printf("warning: %s: dropped %d invalid router-ledger rows", dir, dropped)
		}
	}
	events, err := readRegular(filepath.Join(dir, eventsFile), maxEventsBytes)
	if err != nil {
		if err := bad(err); err != nil {
			return f, err
		}
	} else {
		var dropped int
		if f.Events, dropped = CleanEvents(events); dropped > 0 {
			log.Printf("warning: %s: dropped %d invalid router-events lines", dir, dropped)
		}
	}
	// Run records (issue #140) are line data like the events: a line that
	// does not validate is dropped and counted, in the shared bundle too —
	// the file is re-checked on every load and merge.
	records, err := readRegular(filepath.Join(dir, recordsFile), maxRecordsBytes)
	if err != nil {
		if err := bad(err); err != nil {
			return f, err
		}
	} else if records != nil {
		var dropped int
		if f.Records, dropped = CleanRecords(records); dropped > 0 {
			log.Printf("warning: %s: dropped %d invalid run-record lines", dir, dropped)
		}
	}
	db, err := readRegular(filepath.Join(dir, dbFile), maxDBBytes)
	if err == nil {
		var wal []byte
		if wal, err = readRegular(filepath.Join(dir, walFile), maxDBBytes); err == nil {
			if err = CheckDB(db, wal); err == nil {
				f.DB, f.WAL = db, wal
			}
		}
	}
	if err != nil {
		// The db and its write-ahead log go together or not at all.
		if err := bad(fmt.Errorf("memory db: %w", err)); err != nil {
			return f, err
		}
	}
	return f, nil
}

// writeFile replaces path rather than writing through it, in case something
// planted a symlink there.
func writeFile(path string, data []byte) error {
	_ = os.Remove(path)
	return os.WriteFile(path, data, 0o644)
}

func (f Files) write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Empty files are left out, as CodeAF would not have written them.
	_ = os.Remove(filepath.Join(dir, ledgerFile))
	if len(f.Ledger.Entries) > 0 || len(f.Ledger.Aliases) > 0 {
		ledger, err := json.MarshalIndent(f.Ledger, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, ledgerFile), append(ledger, '\n')); err != nil {
			return err
		}
	}
	_ = os.Remove(filepath.Join(dir, eventsFile))
	if len(f.Events) > 0 {
		if err := writeFile(filepath.Join(dir, eventsFile), []byte(strings.Join(f.Events, "\n")+"\n")); err != nil {
			return err
		}
	}
	_ = os.Remove(filepath.Join(dir, recordsFile))
	if len(f.Records) > 0 {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, r := range f.Records {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		if err := writeFile(filepath.Join(dir, recordsFile), buf.Bytes()); err != nil {
			return err
		}
	}
	for name, data := range map[string][]byte{dbFile: f.DB, walFile: f.WAL} {
		_ = os.Remove(filepath.Join(dir, name))
		if data != nil {
			if err := writeFile(filepath.Join(dir, name), data); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- manifest

func newManifest() Manifest {
	return Manifest{Version: stateVersion, Merged: []string{}, Spend: map[string]float64{}}
}

// readManifest reads a bundle's manifest. No bundle at all (the first run) is
// generation 0; a bundle that is present but unreadable is an error, since the
// bundle is written only by the codeaf-state workflow.
func readManifest(dir string, now time.Time, weekCap float64) (Manifest, bool, error) {
	data, err := readRegular(filepath.Join(dir, manifestFile), maxMetaBytes)
	if err != nil {
		return Manifest{}, false, err
	}
	if data == nil {
		return newManifest(), false, nil
	}
	m := newManifest()
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, false, fmt.Errorf("%s: %w", manifestFile, err)
	}
	if m.Version != stateVersion || m.Generation < 0 {
		return Manifest{}, false, fmt.Errorf("%s: unsupported version %d / generation %d", manifestFile, m.Version, m.Generation)
	}
	// The spend is re-checked here on every load because the queue holds
	// new starts on it (CODEAF_WEEK_SPENT_USD). The write path clamps both
	// halves: SpendDate keeps a day at most 24h ahead, RunSpend a value at
	// most the per-run cap. So a day older than pruneSpend would keep (or
	// further ahead than SpendDate allows), or a single day past the weekly
	// credit, is not a manifest this harness wrote:
	// refuse it rather than carry an entry no week sums and no pruning
	// deletes.
	oldest := now.UTC().Add(-spendDays * 24 * time.Hour).Format(time.DateOnly)
	latest := now.UTC().Add(24 * time.Hour).Format(time.DateOnly)
	for day, usd := range m.Spend {
		if _, err := time.Parse(time.DateOnly, day); err != nil || day < oldest || day > latest || !validUSD(usd) || (weekCap > 0 && usd > weekCap) {
			return Manifest{}, false, fmt.Errorf("%s: invalid spend %q: %v", manifestFile, day, usd)
		}
	}
	if m.Merged == nil {
		m.Merged = []string{}
	}
	return m, true, nil
}

func validUSD(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) && x >= 0 && x < 1e6 }

// ---------------------------------------------------------------- merge rules

// MergeLedger combines two sanitized ledgers from runs that overlapped. For
// each model and class the entry backed by more observations wins, then the
// newer one; aliases already in base are kept.
func MergeLedger(base, run RouterLedger) RouterLedger {
	type key struct{ model, class string }
	index := map[key]int{}
	out := RouterLedger{Entries: slices.Clone(base.Entries)}
	for i, e := range out.Entries {
		index[key{e.Model, e.Class}] = i
	}
	for _, e := range run.Entries {
		k := key{e.Model, e.Class}
		i, ok := index[k]
		switch {
		case !ok:
			if len(out.Entries) < maxLedgerEntries {
				index[k] = len(out.Entries)
				out.Entries = append(out.Entries, e)
			}
		case e.Count > out.Entries[i].Count || e.Count == out.Entries[i].Count && e.Updated.After(out.Entries[i].Updated):
			out.Entries[i] = e
		}
	}
	if out.Entries == nil {
		out.Entries = []RouterEntry{}
	}
	for from, to := range base.Aliases {
		if out.Aliases == nil {
			out.Aliases = map[string]string{}
		}
		out.Aliases[from] = to
	}
	for from, to := range run.Aliases {
		if _, ok := out.Aliases[from]; !ok {
			if out.Aliases == nil {
				out.Aliases = map[string]string{}
			}
			out.Aliases[from] = to
		}
	}
	return out
}

// MergeEvents appends the run's events that base doesn't already have, in
// the run's order, keeping the newest maxEventLines.
func MergeEvents(base, run []string) []string {
	seen := make(map[string]bool, len(base))
	for _, line := range base {
		seen[line] = true
	}
	out := slices.Clone(base)
	for _, line := range run {
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	if len(out) > maxEventLines {
		out = out[len(out)-maxEventLines:]
	}
	return out
}

// mergeRun applies one packed run to the shared files and says what it did.
//
// Ratings and events are always merged (MergeLedger, MergeEvents): a run that
// started from the current bundle holds everything in it plus its own changes,
// so merging gives the same result as taking its files, and a file the checks
// refused can't wipe what the bundle already has.
//
// The memory db can't be merged without opening it, and its rows are keyed by
// numbers local to each copy. So it is taken from the run only when the run
// started from the bundle's current generation (fast-forward), or when the
// bundle has no db yet. A run that overlapped another keeps the bundle's db;
// its own copy stays in its codeaf-state artifact.
func mergeRun(shared Files, generation int, meta RunMeta, run Files) (Files, string) {
	merged := Files{
		Ledger:  MergeLedger(shared.Ledger, run.Ledger),
		Events:  MergeEvents(shared.Events, run.Events),
		Records: MergeRecords(shared.Records, run.Records),
		DB:      shared.DB,
		WAL:     shared.WAL,
	}
	switch {
	case meta.BaseGeneration == generation:
		if run.DB != nil {
			merged.DB, merged.WAL = run.DB, run.WAL
		}
		return merged, "fast-forward"
	case shared.DB == nil && run.DB != nil:
		merged.DB, merged.WAL = run.DB, run.WAL
		return merged, fmt.Sprintf("merged into generation %d (it started from %d); memory db taken, as the bundle had none", generation, meta.BaseGeneration)
	case run.DB != nil && !bytes.Equal(run.DB, shared.DB):
		log.Printf("warning: run %s overlapped another run; its memory db was not merged (it is kept in its codeaf-state artifact)", meta.key())
	}
	return merged, fmt.Sprintf("merged into generation %d (it started from %d)", generation, meta.BaseGeneration)
}

// SpendDate is the day a run's spend counts against: the date it recorded,
// if that is a valid date within a few days of now, else today.
func SpendDate(recorded string, now time.Time) string {
	today := now.UTC().Format(time.DateOnly)
	day, err := time.Parse(time.DateOnly, recorded)
	if err != nil || day.After(now.UTC().Add(24*time.Hour)) || day.Before(now.UTC().Add(-3*24*time.Hour)) {
		return today
	}
	return recorded
}

// RunSpend is what a packed run cost, as far as the merge believes it. The
// number was written by the run job, so one that can't be right counts as the
// whole per-run credit cap: a wrong guess must never loosen that cap.
func RunSpend(recorded float64, runCap float64) float64 {
	if math.IsNaN(recorded) || math.IsInf(recorded, 0) || recorded < 0 || recorded > 2*runCap+1 {
		return runCap
	}
	return recorded
}

// weekSpend is the rolling week's credit spend: the sum over the last
// spendDays UTC days, today included. The weekly credit regenerates
// continuously, so a rolling 7-day sum of what runs recorded stands in for
// the refill.
func weekSpend(m Manifest, now time.Time) float64 {
	total := 0.0
	for i := 0; i < spendDays; i++ {
		total += m.Spend[now.UTC().Add(-time.Duration(i)*24*time.Hour).Format(time.DateOnly)]
	}
	return total
}

func pruneSpend(spend map[string]float64, now time.Time) {
	oldest := now.UTC().Add(-spendDays * 24 * time.Hour).Format(time.DateOnly)
	for day := range spend {
		if day < oldest {
			delete(spend, day)
		}
	}
}

// ---------------------------------------------------------------- commands

func setOutput(pairs ...string) error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		for i := 0; i+1 < len(pairs); i += 2 {
			fmt.Printf("%s=%s\n", pairs[i], pairs[i+1])
		}
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for i := 0; i+1 < len(pairs); i += 2 {
		if _, err := fmt.Fprintf(f, "%s=%s\n", pairs[i], pairs[i+1]); err != nil {
			return err
		}
	}
	return nil
}

func runState() error {
	if len(os.Args) < 3 {
		return errors.New("usage: harness state load|pack|merged|merge ...")
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		return err
	}
	args := os.Args[3:]
	switch os.Args[2] {
	case "load":
		if len(args) != 2 {
			return errors.New("usage: harness state load BUNDLE CODEAF_HOME")
		}
		return stateLoad(args[0], args[1], cfg, time.Now())
	case "pack":
		if len(args) != 2 {
			return errors.New("usage: harness state pack CODEAF_HOME OUT")
		}
		return statePack(args[0], args[1], cfg, time.Now())
	case "merged":
		if len(args) != 1 {
			return errors.New("usage: harness state merged BUNDLE")
		}
		m, _, err := readManifest(args[0], time.Now(), cfg.Limits.WeeklyCreditUSD)
		if err != nil {
			return err
		}
		for _, key := range m.Merged {
			fmt.Println(key)
		}
		return nil
	case "merge":
		if len(args) < 2 {
			return errors.New("usage: harness state merge BUNDLE OUT [RUN_DIR...]")
		}
		return stateMerge(args[0], args[1], args[2:], cfg, time.Now())
	}
	return fmt.Errorf("unknown state command %q", os.Args[2])
}

// stateLoad installs the shared bundle into CodeAF's home and outputs its
// generation and today's and this week's credit spend for the run job.
func stateLoad(bundle, home string, cfg Config, now time.Time) error {
	m, found, err := readManifest(bundle, now, cfg.Limits.WeeklyCreditUSD)
	if err != nil {
		return err
	}
	files := Files{Ledger: RouterLedger{Entries: []RouterEntry{}}}
	if found {
		if files, err = readFiles(bundle, cfg.Models, true); err != nil {
			return err
		}
	}
	if err := files.write(home); err != nil {
		return err
	}
	today := m.Spend[now.UTC().Format(time.DateOnly)]
	week := weekSpend(m, now)
	log.Printf("loaded shared state generation %d (%d ratings, %d events, memory db %d bytes); spent today $%.4f, this week $%.4f",
		m.Generation, len(files.Ledger.Entries), len(files.Events), len(files.DB), today, week)
	log.Printf("records: %d (last 7 days: %s)", len(files.Records), recordShape(files.Records, now))
	return setOutput("generation", strconv.Itoa(m.Generation),
		"day_usd", strconv.FormatFloat(today, 'f', -1, 64),
		"week_usd", strconv.FormatFloat(week, 'f', -1, 64))
}

// statePack packs what CodeAF left in its home, and what the run cost, for
// the codeaf-state workflow. It runs after CodeAF, so it reads files the agent
// could have changed; the merge checks them again.
func statePack(home, out string, cfg Config, now time.Time) error {
	meta := RunMeta{
		Version: stateVersion,
		RunID:   os.Getenv("GITHUB_RUN_ID"),
		Attempt: envOr("GITHUB_RUN_ATTEMPT", "1"),
		Date:    now.UTC().Format(time.DateOnly),
	}
	if !digits(meta.RunID) || !digits(meta.Attempt) {
		return fmt.Errorf("GITHUB_RUN_ID %q / GITHUB_RUN_ATTEMPT %q are not numbers", meta.RunID, meta.Attempt)
	}
	base, err := strconv.Atoi(envOr("BASE_GENERATION", "0"))
	if err != nil || base < 0 {
		return fmt.Errorf("BASE_GENERATION: %q is not a generation", os.Getenv("BASE_GENERATION"))
	}
	meta.BaseGeneration = base
	if meta.RunUSD, err = meteredRunSpend(); err != nil {
		log.Printf("warning: proxy unreachable (%v); using the ledger file", err)
		lines, err := readLedger(os.Getenv("LEDGER_PATH"))
		if err != nil {
			log.Printf("warning: ledger unreadable (%v); counting the whole run cap", err)
			meta.RunUSD = cfg.Limits.RunCreditUSD
		} else {
			meta.RunUSD = ledgerTotal(lines)
		}
	}
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	// Bad state files are left out; the spend is still handed on.
	files, _ := readFiles(home, cfg.Models, false)
	if err := files.write(out); err != nil {
		return err
	}
	// The run record (issue #140): one bounded line about this run for the
	// shared bundle. The environment the run job already carries names the
	// mode, the number, the ledger and the result; a field that cannot be
	// computed stays zero, and packing never fails because of the record.
	if rec := runRecordFromRun(now); rec.validate() == nil {
		data, err := json.MarshalIndent(rec, "", "  ")
		if err == nil {
			if err := writeFile(filepath.Join(out, runRecordFile), append(data, '\n')); err != nil {
				log.Printf("warning: run record not packed: %v", err)
			}
		} else {
			log.Printf("warning: run record not packed: %v", err)
		}
		log.Printf("run record: mode %s, stop %s, %d call(s), %d 429(s), $%.4f",
			rec.Mode, rec.Stop, rec.Calls, rec.Rate429, rec.CostUSD)
	} else {
		log.Printf("warning: run record left out: %v", rec.validate())
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	log.Printf("packed run %s: base generation %d, $%.4f, %d ratings, %d events, memory db %d bytes",
		meta.key(), meta.BaseGeneration, meta.RunUSD, len(files.Ledger.Entries), len(files.Events), len(files.DB))
	return writeFile(filepath.Join(out, metaFile), append(data, '\n'))
}

type packedRun struct {
	dir  string
	meta RunMeta
}

func readRunMeta(dir string) (RunMeta, error) {
	data, err := readRegular(filepath.Join(dir, metaFile), maxMetaBytes)
	if err != nil {
		return RunMeta{}, err
	}
	if data == nil {
		return RunMeta{}, errors.New("no meta.json")
	}
	var meta RunMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return RunMeta{}, err
	}
	if meta.Version != stateVersion || !digits(meta.RunID) || !digits(meta.Attempt) || meta.BaseGeneration < 0 {
		return RunMeta{}, fmt.Errorf("invalid meta.json: %+v", meta)
	}
	return meta, nil
}

// stateMerge folds the packed runs in runDirs into the bundle and writes the
// result to out. Runs already merged are skipped; the rest are applied in run
// order. It outputs changed=false when there was nothing to merge.
func stateMerge(bundle, out string, runDirs []string, cfg Config, now time.Time) error {
	m, found, err := readManifest(bundle, now, cfg.Limits.WeeklyCreditUSD)
	if err != nil {
		return err
	}
	shared := Files{Ledger: RouterLedger{Entries: []RouterEntry{}}}
	if found {
		if shared, err = readFiles(bundle, cfg.Models, true); err != nil {
			return err
		}
	}
	done := map[string]bool{}
	for _, key := range m.Merged {
		done[key] = true
	}
	var runs []packedRun
	for _, dir := range runDirs {
		meta, err := readRunMeta(dir)
		if err != nil {
			log.Printf("warning: skipping %s: %v", dir, err)
			continue
		}
		if done[meta.key()] {
			continue
		}
		done[meta.key()] = true
		runs = append(runs, packedRun{dir, meta})
	}
	sort.Slice(runs, func(i, j int) bool {
		a, _ := strconv.ParseUint(runs[i].meta.RunID, 10, 64)
		b, _ := strconv.ParseUint(runs[j].meta.RunID, 10, 64)
		if a != b {
			return a < b
		}
		return runs[i].meta.Attempt < runs[j].meta.Attempt
	})
	if len(runs) == 0 {
		log.Printf("nothing to merge; shared state stays at generation %d", m.Generation)
		return setOutput("changed", "false", "generation", strconv.Itoa(m.Generation))
	}
	for _, r := range runs {
		day := SpendDate(r.meta.Date, now)
		usd := RunSpend(r.meta.RunUSD, cfg.Limits.RunCreditUSD)
		m.Spend[day] += usd
		files, _ := readFiles(r.dir, cfg.Models, false)
		// The run record (issue #140) is appended to the bundle's
		// records.jsonl. A record that cannot be believed is left out; the
		// run itself still merges.
		// A run contributes its own one record and nothing else: a
		// records.jsonl in its artifact could otherwise push the bundle's
		// real history past the 2000-line cap, and a record naming another
		// run could stand in for it.
		files.Records = nil
		if rec, err := readRunRecord(r.dir); err != nil {
			log.Printf("warning: run %s: run record left out: %v", r.meta.key(), err)
		} else if rec.RunID != "" && rec.key() != r.meta.key() {
			log.Printf("warning: run %s: run record names run %s; left out", r.meta.key(), rec.key())
		} else if rec.RunID != "" {
			files.Records = []RunRecord{rec}
		}
		var how string
		shared, how = mergeRun(shared, m.Generation, r.meta, files)
		log.Printf("run %s: %s; spend $%.4f on %s", r.meta.key(), how, usd, day)
		m.Generation++
		m.Merged = append(m.Merged, r.meta.key())
	}
	if len(m.Merged) > maxMergedRuns {
		m.Merged = m.Merged[len(m.Merged)-maxMergedRuns:]
	}
	pruneSpend(m.Spend, now)
	m.Updated = now.UTC()
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := shared.write(out); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFile(filepath.Join(out, manifestFile), append(data, '\n')); err != nil {
		return err
	}
	log.Printf("shared state is now generation %d (%d runs merged)", m.Generation, len(runs))
	return setOutput("changed", "true", "generation", strconv.Itoa(m.Generation))
}
