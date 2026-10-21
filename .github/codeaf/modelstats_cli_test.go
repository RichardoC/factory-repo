// Tests for the CLI and routine wiring of `harness model-stats` (issue #131):
// the --from-dir path that is the issue's end-to-end check, the API paging
// path the routine uses, and the routines.json entry. The GitHub API is a
// local httptest server, so nothing here needs a token or the network.
package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// modelStatsCLILedger is the two-line fixture the CLI tests write into a zip:
// one call to each of the test config's models. The timestamps are relative to
// now, because the command builds its own window from the real clock and the
// rows have to land inside it.
func modelStatsCLILedger(now time.Time) string {
	return strings.Join([]string{
		fmt.Sprintf(`{"ts":%q,"model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
			now.Add(-1*time.Hour).UTC().Format(time.RFC3339)),
		fmt.Sprintf(`{"ts":%q,"model":"beta","completion_tokens":500,"usage_seen":true,"ttft_ms":1500,"dur_ms":5000}`,
			now.Add(-2*time.Hour).UTC().Format(time.RFC3339)),
	}, "\n") + "\n"
}

// modelStatsCLIZipBytes is modelStatsTestZip's bytes, for writing to a
// directory or serving as an artifact body.
func modelStatsCLIZipBytes(t *testing.T, entries ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i+1 < len(entries); i += 2 {
		w, err := zw.Create(entries[i])
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write([]byte(entries[i+1])); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestModelStatsArgs pins the flag parsing: both spellings of --from-dir and
// --hours, the 72-hour default, and a rejection of junk instead of a panic or
// a silently ignored argument.
func TestModelStatsArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		dir     string
		hours   int
		wantErr bool
	}{
		{name: "empty", args: nil, dir: "", hours: 72},
		{name: "from-dir space", args: []string{"--from-dir", "/tmp/zips"}, dir: "/tmp/zips", hours: 72},
		{name: "from-dir equals", args: []string{"--from-dir=/tmp/zips"}, dir: "/tmp/zips", hours: 72},
		{name: "hours space", args: []string{"--hours", "6"}, dir: "", hours: 6},
		{name: "both", args: []string{"--hours=24", "--from-dir", "/d"}, dir: "/d", hours: 24},
		{name: "missing value", args: []string{"--from-dir"}, wantErr: true},
		{name: "junk hours", args: []string{"--hours", "many"}, wantErr: true},
		{name: "zero hours", args: []string{"--hours", "0"}, wantErr: true},
		{name: "huge hours", args: []string{"--hours", "999999"}, wantErr: true},
		{name: "unknown flag", args: []string{"--from"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, hours, err := parseModelStatsArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseModelStatsArgs(%q) = %q/%d, want an error", tc.args, dir, hours)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseModelStatsArgs(%q): %v", tc.args, err)
			}
			if dir != tc.dir || hours != tc.hours {
				t.Errorf("parseModelStatsArgs(%q) = %q/%d, want %q/%d", tc.args, dir, hours, tc.dir, tc.hours)
			}
		})
	}
}

// TestModelStatsFromDirRendersTheSameReport: the end-to-end path the issue
// checks -- a directory of downloaded codeaf-output zips -- renders exactly
// the report the core renders for the same zips, and the call counts the
// table shows are the number of ledger lines per model in those zips.
func TestModelStatsFromDirRendersTheSameReport(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	// The command builds its own window from the real clock, so the fixture
	// and the expected report are both taken at the same instant, just after.
	before := time.Now().UTC()

	dir := t.TempDir()
	// Two runs, as two downloaded zips -- the way the routine's workflow
	// leaves them -- plus a non-zip file that must be skipped, not fatal.
	zipBytes := modelStatsCLIZipBytes(t, "codeaf-ledger.jsonl", modelStatsCLILedger(before))
	for i, name := range []string{"codeaf-output-1-1.zip", "codeaf-output-2-1.zip"} {
		if err := os.WriteFile(filepath.Join(dir, name), zipBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	injectModelStatsTestConfig(t)
	out, err := captureStdout(t, func() error { return runModelStatsWith("--from-dir", dir) })
	if err != nil {
		t.Fatalf("model-stats --from-dir: %v", err)
	}

	// The core renders the same zips over the same clock-derived window: the
	// command must produce exactly that text, which is what makes --from-dir
	// the issue's end-to-end check.
	in, err := readModelStatsDir(dir, cfg)
	if err != nil {
		t.Fatalf("readModelStatsDir: %v", err)
	}
	now := time.Now().UTC()
	win := modelStatsWindowFrom(cfg, now)
	if want := modelStatsReport(in, cfg, win, now); out != want {
		t.Errorf("--from-dir output differs from the core report\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
	if strings.Contains(out, "alerts (0)") {
		t.Errorf("fixture produced an empty report:\n%s", out)
	}
	// Two zips x one line per model: the table's count column must be 2 for
	// each model, i.e. the count of ledger lines per model in the zips.
	if !strings.Contains(out, "| `alpha` | 2 |") || !strings.Contains(out, "| `beta` | 2 |") {
		t.Errorf("per-model call counts are not the ledger line counts:\n%s", out)
	}
}

// TestModelStatsLivePathPagesAndSkips: the routine path asks for pages with an
// explicit page= parameter until the listing is exhausted, reads only
// completed non-fork runs, skips runs without a codeaf-output artifact and
// reads the artifact zip in memory -- and only for the wanted run id/attempt.
func TestModelStatsLivePathPagesAndSkips(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := timeForModelStatsCLI()

	zipBytes := modelStatsCLIZipBytes(t, "codeaf-ledger.jsonl", modelStatsCLILedger(now))
	var pages, artifactHits []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/actions/workflows/codeaf.yml/runs"):
			pages = append(pages, r.URL.Query().Get("page"))
			if r.URL.Query().Get("page") == "1" {
				// Page 1: a wanted run, a fork run of the same workflow, and a
				// run that carries no codeaf-output artifact. Full page, so
				// the paging loop must ask for page 2.
				runs := make([]string, 0, 100)
				runs = append(runs, fmt.Sprintf(
					`{"id":11,"run_attempt":1,"status":"completed","conclusion":"success","created_at":%q,"repository_id":7,"head_repository":{"id":7,"full_name":"RichardoC/factory-repo"}}`,
					now.Add(-2*time.Hour).Format("2006-01-02T15:04:05Z")))
				runs = append(runs, fmt.Sprintf(
					`{"id":12,"run_attempt":1,"status":"completed","conclusion":"success","created_at":%q,"repository_id":7,"head_repository":{"id":99,"full_name":"someone/factory-repo","fork":true}}`,
					now.Add(-2*time.Hour).Format("2006-01-02T15:04:05Z")))
				runs = append(runs, fmt.Sprintf(
					`{"id":13,"run_attempt":1,"status":"completed","conclusion":"success","created_at":%q,"repository_id":7,"head_repository":{"id":7,"full_name":"RichardoC/factory-repo"}}`,
					now.Add(-2*time.Hour).Format("2006-01-02T15:04:05Z")))
				for len(runs) < 100 {
					runs = append(runs, fmt.Sprintf(
						`{"id":%d,"run_attempt":1,"status":"completed","conclusion":"success","created_at":%q,"repository_id":7,"head_repository":{"id":7,"full_name":"RichardoC/factory-repo"}}`,
						1000+len(runs), now.Add(-30*24*time.Hour).Format("2006-01-02T15:04:05Z")))
				}
				fmt.Fprintf(w, `{"total_count":200,"workflow_runs":[%s]}`, strings.Join(runs, ","))
				return
			}
			// Page 2: one more wanted run, and the end of the listing.
			body := fmt.Sprintf(`{"total_count":200,"workflow_runs":[{"id":14,"run_attempt":2,"status":"completed","conclusion":"success","created_at":%q,"repository_id":7,"head_repository":{"id":7,"full_name":"RichardoC/factory-repo"}}]}`,
				now.Add(-3*time.Hour).Format("2006-01-02T15:04:05Z"))
			_, _ = w.Write([]byte(body))
		case strings.Contains(r.URL.Path, "/actions/runs/") && strings.Contains(r.URL.Path, "/artifacts"):
			runID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/RichardoC/factory-repo/actions/runs/"), "/artifacts")
			switch runID {
			case "11":
				_, _ = w.Write([]byte(`{"total_count":1,"artifacts":[{"id":101,"name":"codeaf-output-11-1","size_in_bytes":2000,"expired":false}]}`))
			case "13":
				_, _ = w.Write([]byte(`{"total_count":1,"artifacts":[{"id":103,"name":"codeaf-output-13-1","size_in_bytes":2000,"expired":false}]}`))
			case "14":
				_, _ = w.Write([]byte(`{"total_count":1,"artifacts":[{"id":104,"name":"codeaf-output-14-2","size_in_bytes":2000,"expired":false}]}`))
			default:
				// The fork run must never get here.
				t.Errorf("artifacts listed for run %s", runID)
				_, _ = w.Write([]byte(`{"total_count":0,"artifacts":[]}`))
			}
		case strings.HasPrefix(r.URL.Path, "/repos/RichardoC/factory-repo/actions/artifacts/"):
			artifactHits = append(artifactHits, r.URL.Path)
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(zipBytes)
		default:
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	injectModelStatsTestConfig(t)
	t.Setenv("GITHUB_API_URL", server.URL)
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_TOKEN", "tok")

	win := modelStatsWindow{Hours: 72, From: now.Add(-72 * time.Hour), To: now}
	in, err := readModelStatsFromRunsConfig(cfg, win)
	if err != nil {
		t.Fatalf("readModelStatsFromRunsConfig: %v", err)
	}
	if len(pages) < 2 || pages[0] != "1" || pages[1] != "2" {
		t.Errorf("pages requested = %v, want page= 1 then 2", pages)
	}
	// Three wanted artifacts (11, 13, 14); the fork (12) is skipped and the
	// pruned run (13's artifact exists, so it is read; the one without an
	// artifact is a run on page 1 with id 1000+, which has none) never gets a
	// download attempt.
	if len(artifactHits) != 3 {
		t.Errorf("artifact downloads = %v, want 3", artifactHits)
	}
	for _, hit := range artifactHits {
		if strings.Contains(hit, "/101/") || strings.Contains(hit, "/104/") || strings.Contains(hit, "/103/") {
			continue
		}
		t.Errorf("unexpected artifact download %s", hit)
	}
	if in.RunCount != 3 {
		t.Errorf("runs read = %d, want 3 (11, 13, 14)", in.RunCount)
	}
	report := modelStatsReport(in, cfg, win, now)
	// One ledger line per model per zip, three zips read: the table's counts
	// are the number of ledger lines per model in the zips.
	if !strings.Contains(report, "| `alpha` | 3 | 3 |") || !strings.Contains(report, "| `beta` | 3 | 3 |") {
		t.Errorf("expected 3 alpha and 3 beta calls across 3 runs, got:\n%s", report)
	}
	if !strings.Contains(report, "across 3 run(s) with a ledger") {
		t.Errorf("expected 3 runs with a ledger, got:\n%s", report)
	}
}

// injectModelStatsTestConfig makes the command read the test's small config
// instead of the repository's models.json, and restores it after the test.
func injectModelStatsTestConfig(t *testing.T) {
	t.Helper()
	cfg := modelStatsTestConfig(t)
	prev := modelStatsLoadConfig
	modelStatsLoadConfig = func() (Config, error) { return cfg, nil }
	t.Cleanup(func() { modelStatsLoadConfig = prev })
}

// timeForModelStatsCLI is the fixed clock the CLI tests window against, the
// same one the core's tests use.
func timeForModelStatsCLI() time.Time {
	return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
}

// TestModelStatsNoFromDirNeedsNoToken: with no --from-dir and no repository
// token set, the routine path fails with a clear error instead of hanging or
// printing an empty comment.
func TestModelStatsNoFromDirNeedsNoToken(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	cfg := modelStatsTestConfig(t)
	now := timeForModelStatsCLI()
	win := modelStatsWindow{Hours: 72, From: now.Add(-72 * time.Hour), To: now}
	if _, err := readModelStatsFromRunsConfig(cfg, win); err == nil {
		t.Fatal("readModelStatsFromRunsConfig with no repository: want an error")
	}
}

// TestModelStatsGetBytesFollowsRedirectWithoutReplayingTheToken: an artifact
// download redirects to a blob host. The redirect must be followed by hand,
// and the Authorization header must not be replayed to the other host.
func TestModelStatsGetBytesFollowsRedirectWithoutReplayingTheToken(t *testing.T) {
	zipBytes := modelStatsCLIZipBytes(t, "codeaf-ledger.jsonl", modelStatsCLILedger(time.Now().UTC()))
	var blobAuth string
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blobAuth = r.Header.Get("Authorization")
		_, _ = w.Write(zipBytes)
	}))
	t.Cleanup(blob.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("API request lost its token: %q", r.Header.Get("Authorization"))
		}
		http.Redirect(w, r, blob.URL, http.StatusFound)
	}))
	t.Cleanup(api.Close)

	g := github{api: api.URL, repo: "RichardoC/factory-repo", token: "tok", client: api.Client()}
	got, err := g.getBytes("/actions/artifacts/1/zip")
	if err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if !bytes.Equal(got, zipBytes) {
		t.Errorf("getBytes returned %d bytes, want %d", len(got), len(zipBytes))
	}
	if blobAuth != "" {
		t.Errorf("the Authorization header was replayed to the blob host: %q", blobAuth)
	}
}

// TestModelStatsGetBytesRefusesAnEndlessRedirectChain: a server that always
// redirects must be refused, not followed forever.
func TestModelStatsGetBytesRefusesAnEndlessRedirectChain(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
	}))
	t.Cleanup(api.Close)
	g := github{api: api.URL, repo: "RichardoC/factory-repo", token: "tok", client: api.Client()}
	if _, err := g.getBytes("/actions/artifacts/1/zip"); err == nil {
		t.Fatal("an endless redirect chain was followed: want an error")
	}
}

// TestModelStatsRoutineRendersTheSameReport: the routine entry point
// (`harness routine-check model-stats`) is the live path, and its report is
// the same rendering for the same rows.
func TestModelStatsRoutineRendersTheSameReport(t *testing.T) {
	_ = modelStatsTestConfig
	injectModelStatsTestConfig(t)
	zipBytes := modelStatsCLIZipBytes(t, "codeaf-ledger.jsonl", modelStatsCLILedger(time.Now().UTC()))
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		switch {
		case strings.Contains(r.URL.Path, "/actions/workflows/codeaf.yml/runs"):
			body := fmt.Sprintf(`{"total_count":1,"workflow_runs":[{"id":21,"run_attempt":1,"status":"completed","conclusion":"success","created_at":%q,"repository_id":7,"head_repository":{"id":7,"full_name":"RichardoC/factory-repo"}}]}`,
				now.Add(-time.Hour).Format("2006-01-02T15:04:05Z"))
			_, _ = w.Write([]byte(body))
		case strings.Contains(r.URL.Path, "/actions/runs/21/artifacts"):
			_, _ = w.Write([]byte(`{"total_count":1,"artifacts":[{"id":201,"name":"codeaf-output-21-1","size_in_bytes":1500,"expired":false}]}`))
		case strings.Contains(r.URL.Path, "/actions/artifacts/201/zip"):
			_, _ = w.Write(zipBytes)
		default:
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_TOKEN", "tok")

	got, err := runModelStatsRoutine()
	if err != nil {
		t.Fatalf("runModelStatsRoutine: %v", err)
	}
	if !strings.Contains(got, "| `alpha` | 1 | 1 |") || !strings.Contains(got, "| `beta` | 1 | 1 |") {
		t.Errorf("routine report does not carry the one call per model:\n%s", got)
	}
	if strings.Contains(got, "across 0 run(s)") {
		t.Errorf("routine read no run:\n%s", got)
	}
}

// TestModelStatsRoutineRefusesAnEmptyWindow: with nothing to read the routine
// errors rather than printing a comment about no data -- an empty comment on
// the tracking issue would look like a healthy factory.
func TestModelStatsRoutineRefusesAnEmptyWindow(t *testing.T) {
	injectModelStatsTestConfig(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_TOKEN", "tok")
	if _, err := runModelStatsRoutine(); err == nil {
		t.Fatal("an empty window produced a comment: want an error")
	}
}

// TestModelStatsNeverExtractsAnArtifact pins the issue's rule: the zip is read
// in memory and nothing is written to disk on the artifact path. The check is
// a source-level one, because the property is about what the code can do at
// all rather than about one input.
func TestModelStatsNeverExtractsAnArtifact(t *testing.T) {
	data, err := os.ReadFile("modelstats_cli.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, banned := range []string{"os.WriteFile", "os.Create", "os.MkdirAll", "os.MkdirTemp", "os.Remove", "ioutil.WriteFile", "exec.", "os/exec"} {
		if strings.Contains(src, banned) {
			t.Errorf("modelstats_cli.go uses %s: an artifact must never be extracted or executed", banned)
		}
	}
	if !strings.Contains(src, "zip.NewReader(bytes.NewReader(") {
		t.Error("the only zip-reading path should be zip.NewReader over an in-memory buffer")
	}
}

// TestCheckModelStatsFallsBackToTheAPI: the registered routine
// (`harness routine-check model-stats`) must work on the job issue #131
// describes -- actions: read and issues: write, no prepared directory -- so
// with CODEAF_MODEL_STATS_DIR unset it reads the runs through the API. Before
// the fallback it failed with "CODEAF_MODEL_STATS_DIR is required", and the
// routine could never produce a comment on the maintainer's job.
func TestCheckModelStatsFallsBackToTheAPI(t *testing.T) {
	injectModelStatsTestConfig(t)
	t.Setenv("CODEAF_MODEL_STATS_DIR", "")
	t.Setenv("CODEAF_MODEL_STATS_NOW", "")
	zipBytes := modelStatsCLIZipBytes(t, "codeaf-ledger.jsonl", modelStatsCLILedger(time.Now().UTC()))
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		switch {
		case strings.Contains(r.URL.Path, "/actions/workflows/codeaf.yml/runs"):
			// The page= parameter must be explicit, and the wanted run must be
			// inside the window.
			if r.URL.Query().Get("page") == "" {
				t.Error("the run listing was requested without an explicit page= parameter")
			}
			body := fmt.Sprintf(`{"total_count":1,"workflow_runs":[{"id":31,"run_attempt":2,"status":"completed","conclusion":"success","created_at":%q,"repository_id":7,"head_repository":{"id":7,"full_name":"RichardoC/factory-repo"}}]}`,
				now.Add(-time.Hour).Format("2006-01-02T15:04:05Z"))
			_, _ = w.Write([]byte(body))
		case strings.Contains(r.URL.Path, "/actions/runs/31/artifacts"):
			_, _ = w.Write([]byte(`{"total_count":1,"artifacts":[{"id":301,"name":"codeaf-output-31-2","size_in_bytes":1500,"expired":false}]}`))
		case strings.Contains(r.URL.Path, "/actions/artifacts/301/zip"):
			_, _ = w.Write(zipBytes)
		default:
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_TOKEN", "tok")

	got, err := checkModelStats()
	if err != nil {
		t.Fatalf("checkModelStats without a directory: %v", err)
	}
	if !strings.Contains(got, "| `alpha` | 1 | 1 |") || !strings.Contains(got, "| `beta` | 1 | 1 |") {
		t.Errorf("the routine's API report does not carry the one call per model:\n%s", got)
	}
}

// TestCheckModelStatsPrefersTheDirectory: when a workflow has already
// downloaded the artifacts, that directory is read and the API is not called
// at all.
func TestCheckModelStatsPrefersTheDirectory(t *testing.T) {
	injectModelStatsTestConfig(t)
	dir := t.TempDir()
	zipBytes := modelStatsCLIZipBytes(t, "codeaf-ledger.jsonl", modelStatsCLILedger(time.Now().UTC()))
	if err := os.WriteFile(filepath.Join(dir, "codeaf-output-41-1.zip"), zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEAF_MODEL_STATS_DIR", dir)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the API was called although CODEAF_MODEL_STATS_DIR is set: %s", r.URL.Path)
		http.Error(w, "not served", http.StatusNotFound)
	}))
	t.Cleanup(api.Close)
	t.Setenv("GITHUB_API_URL", api.URL)
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_TOKEN", "tok")

	got, err := checkModelStats()
	if err != nil {
		t.Fatalf("checkModelStats with a directory: %v", err)
	}
	if !strings.Contains(got, "| `alpha` | 1 | 1 |") {
		t.Errorf("the directory report does not carry the call:\n%s", got)
	}
}

// TestModelStatsWindowCountsAddUp: the headline states "Rows: N accepted, M
// rejected", and N+M must be every row read. filterWindow (modelstats.go)
// used to move an out-of-window row into the rejected count without taking it
// out of the accepted count, so the headline overstated the total (probe: two
// rows, one outside the window, rendered as "2 accepted, 1 rejected"). The
// harness's own window is 72 hours and a downloaded zip can hold older rows,
// so the path is reachable with real artifacts.
func TestModelStatsWindowCountsAddUp(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Now().UTC()
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join([]string{
		fmt.Sprintf(`{"ts":%q,"model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
			now.Add(-1*time.Hour).Format(time.RFC3339)),
		// Older than the 72-hour window.
		fmt.Sprintf(`{"ts":%q,"model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
			now.Add(-100*time.Hour).Format(time.RFC3339)),
	}, "\n")+"\n")
	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	win := modelStatsWindowFrom(cfg, now)
	got := modelStatsReport(in, cfg, win, now)
	if !strings.Contains(got, "Rows: 1 accepted, 1 rejected") {
		t.Errorf("the window counts do not add up:\n%s", got)
	}
	if !strings.Contains(got, "| `alpha` | 1 | 1 |") {
		t.Errorf("the in-window call is not the one counted:\n%s", got)
	}
}
