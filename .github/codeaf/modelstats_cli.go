// modelstats_cli.go wires the model-stats core (modelstats.go, issue #131)
// into the harness CLI and the routine mechanism (#117).
//
// Two paths, one renderer:
//
//	harness model-stats --from-dir DIR [--hours N]
//	    reads already-downloaded codeaf-output zips from a directory and
//	    prints the comment. Offline, no token: this is the issue's
//	    end-to-end check.
//	harness model-stats [--hours N]
//	    the routine path: lists completed codeaf.yml runs of the last N
//	    hours (default 72) through the GitHub API with an explicit page=
//	    parameter, skips forks and runs without a codeaf-output artifact,
//	    downloads each artifact through the API and reads the zip IN
//	    MEMORY with archive/zip.
//
// The job that runs it needs actions: read and issues: write only. Nothing
// from an artifact is ever extracted to disk or executed, and the agent is
// never run (the codeaf-state.yml pattern). Strings from GitHub -- run
// titles, artifact names, file names inside a zip -- are untrusted data and
// are never echoed into the report; only validated ledger rows are.
package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// modelStatsDefaultHours is the routine window when --hours is not given:
// the issue's 72 hours, which models.json's alerts.window_hours also carries.
const modelStatsDefaultHours = 72

// modelStatsWorkflow is the workflow whose runs carry a codeaf-output
// artifact. Only completed runs of it are listed.
const modelStatsWorkflow = "codeaf.yml"

// modelStatsMaxRunPages bounds the run listing. GitHub pages at 100 runs a
// page, so this is 10,000 runs of history -- far past any 72-hour window,
// and it makes the loop terminate even if the API keeps answering.
const modelStatsMaxRunPages = 100

// runListPage is one page of GET /actions/workflows/{id}/runs. Only the
// fields the routine needs are decoded, each bounded.
type runListPage struct {
	TotalCount   int        `json:"total_count"`
	WorkflowRuns []workflow `json:"workflow_runs"`
}

// workflow is one workflow run, reduced to what decides whether it is read:
// its id and attempt name the artifact, its head repository says whether it
// is a fork, and its status/conclusion say whether it completed.
type workflow struct {
	ID           int64  `json:"id"`
	RunAttempt   int    `json:"run_attempt"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	CreatedAt    string `json:"created_at"`
	HeadBranch   string `json:"head_branch"`
	HeadRepo     *repo  `json:"head_repository"`
	RepositoryID int64  `json:"repository_id"`
}

// repo is the head repository of a run: a fork run has a different full_name
// from this repository, and is skipped rather than read.
type repo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Fork     bool   `json:"fork"`
}

// artifactListPage is one page of GET /actions/runs/{id}/artifacts.
type artifactListPage struct {
	TotalCount int        `json:"total_count"`
	Artifacts  []artifact `json:"artifacts"`
}

// artifact is one run artifact. name is untrusted and is matched, never
// echoed; expired artifacts are skipped because their download would 410.
type artifact struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	SizeInBytes int64     `json:"size_in_bytes"`
	Expired     bool      `json:"expired"`
	WorkflowRun *workflow `json:"workflow_run"`
}

// runModelStats implements `harness model-stats [--from-dir DIR] [--hours N]`.
// With --from-dir it reads that directory and needs no token; without it, it
// fetches the recent runs and their artifacts through the GitHub API. Both
// paths end in modelStatsReport, so the same rows render the same comment.
func runModelStats() error {
	return runModelStatsWith(os.Args[2:]...)
}

// modelStatsLoadConfig reads the config the command and the routine work
// from. It is a variable so a test can inject a small config of its own
// (AGENTS.md: a test builds its own config rather than hard-coding models.json
// values) without the command reaching for the repository's models.json.
var modelStatsLoadConfig = func() (Config, error) { return loadConfig(configPath()) }

// runModelStatsWith is runModelStats with its arguments passed in, so a test
// drives the command without touching os.Args.
func runModelStatsWith(args ...string) error {
	dir, hours, err := parseModelStatsArgs(args)
	if err != nil {
		return err
	}
	cfg, err := modelStatsLoadConfig()
	if err != nil {
		return fmt.Errorf("model-stats: %w", err)
	}
	now := time.Now().UTC()
	win := modelStatsWindow{Hours: hours, From: now.Add(-time.Duration(hours) * time.Hour), To: now}

	var in modelStatsInput
	if dir != "" {
		in, err = readModelStatsDir(dir, cfg)
		if err != nil {
			return err
		}
	} else {
		in, err = readModelStatsFromRunsConfig(cfg, win)
		if err != nil {
			return err
		}
	}
	fmt.Print(modelStatsReport(in, cfg, win, now))
	return nil
}

// parseModelStatsArgs reads the command's flags by hand, the way the rest of
// the CLI does: --from-dir DIR and --hours N, in either order, with
// --from-dir=DIR and --hours=N accepted too. hours is validated into the
// same bound the alert config uses, so a typo cannot ask for a decade of
// history or a window of no length.
func parseModelStatsArgs(args []string) (dir string, hours int, err error) {
	hours = modelStatsDefaultHours
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var value string
		var hasValue bool
		for _, name := range []string{"--from-dir", "--hours"} {
			if strings.HasPrefix(arg, name+"=") {
				value, hasValue = strings.TrimPrefix(arg, name+"="), true
				break
			}
			if arg == name {
				if i+1 >= len(args) {
					return "", 0, fmt.Errorf("model-stats: %s needs a value", name)
				}
				i++
				value, hasValue = args[i], true
				break
			}
		}
		if !hasValue {
			return "", 0, fmt.Errorf("model-stats: unknown argument %q", arg)
		}
		switch {
		case strings.HasPrefix(arg, "--from-dir"):
			dir = value
		default:
			n, convErr := strconv.Atoi(value)
			if convErr != nil || n <= 0 || n > modelStatsMaxHours {
				return "", 0, fmt.Errorf("model-stats: --hours %q is not an hour count in 1..%d", value, modelStatsMaxHours)
			}
			hours = n
		}
	}
	return dir, hours, nil
}

// modelStatsMaxHours bounds --hours the way the alert config bounds its own
// windows: a year, so a window is always a window.
const modelStatsMaxHours = 24 * 365

// ---------------------------------------------------------------- live path

// readModelStatsFromRuns is the routine path: the codeaf-output zips of the
// completed codeaf.yml runs whose start falls in the window, read in memory.
// A run is skipped -- never failed on -- when it is a fork, has no
// codeaf-output artifact, or its artifact cannot be read; one bad run must
// not sink the report. When no run yields a zip the error says so, rather
// than printing a comment about nothing.
func readModelStatsFromRunsConfig(cfg Config, win modelStatsWindow) (modelStatsInput, error) {
	g := newGitHub()
	if g.repo == "" {
		return modelStatsInput{}, errors.New("model-stats: GITHUB_REPOSITORY is required without --from-dir")
	}
	zips, err := fetchModelStatsZips(g, cfg, win)
	if err != nil {
		return modelStatsInput{}, err
	}
	if len(zips) == 0 {
		return modelStatsInput{}, fmt.Errorf("model-stats: no codeaf-output artifact among the %s runs of the last %d hours", modelStatsWorkflow, win.Hours)
	}
	return parseModelStatsZips(zips, cfg), nil
}

// fetchModelStatsZips lists the window's runs and downloads each one's
// codeaf-output zip. The run listing pages with an explicit page= parameter
// until a page comes back short or the total is reached: an implicit
// "start after the last id" cursor is not reliable across a filtered list.
func fetchModelStatsZips(g github, cfg Config, win modelStatsWindow) ([]*zip.Reader, error) {
	var zips []*zip.Reader
	perPage := 100
	limited := !win.From.IsZero()
	for page := 1; page <= modelStatsMaxRunPages; page++ {
		var runs runListPage
		path := fmt.Sprintf("/actions/workflows/%s/runs?status=completed&per_page=%d&page=%d",
			modelStatsWorkflow, perPage, page)
		if err := g.get(path, &runs); err != nil {
			return nil, err
		}
		if len(runs.WorkflowRuns) == 0 {
			break
		}
		for _, run := range runs.WorkflowRuns {
			if !runInWindow(run, win, limited) {
				continue
			}
			if run.HeadRepo != nil && run.RepositoryID != 0 && run.HeadRepo.ID != run.RepositoryID {
				continue // a fork run: its artifacts are not this repo's
			}
			if run.HeadRepo != nil && run.HeadRepo.FullName != "" && !strings.EqualFold(run.HeadRepo.FullName, g.repo) {
				continue
			}
			data, ok, err := downloadCodeafOutput(g, run)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if zr, zipErr := zip.NewReader(bytes.NewReader(data), int64(len(data))); zipErr == nil {
				zips = append(zips, zr)
			}
		}
		if page*perPage >= runs.TotalCount && limited {
			break
		}
		if len(runs.WorkflowRuns) < perPage {
			break
		}
	}
	return zips, nil
}

// runInWindow reports whether a run started inside the window. A window with
// no From edge (unbounded) accepts every run the listing returns; a run whose
// timestamp will not parse is refused rather than read.
func runInWindow(run workflow, win modelStatsWindow, limited bool) bool {
	if !limited {
		return true
	}
	if run.CreatedAt == "" {
		return false
	}
	ts, err := time.Parse(time.RFC3339, run.CreatedAt)
	if err != nil {
		return false
	}
	return !ts.Before(win.From) && !ts.After(win.To)
}

// downloadCodeafOutput finds the run's codeaf-output artifact and returns its
// zip bytes. ok is false when the run has none -- the routine's ordinary
// reason to skip a run -- while an error means the API call itself failed.
// The artifact name carries the run id and attempt the workflow sets:
// codeaf-output-<id>-<attempt>.
func downloadCodeafOutput(g github, run workflow) (data []byte, ok bool, err error) {
	want := fmt.Sprintf("codeaf-output-%d-%d", run.ID, run.RunAttempt)
	for page := 1; page <= 2; page++ {
		var arts artifactListPage
		path := fmt.Sprintf("/actions/runs/%d/artifacts?per_page=100&page=%d", run.ID, page)
		if err := g.get(path, &arts); err != nil {
			return nil, false, err
		}
		for _, art := range arts.Artifacts {
			if art.Name != want || art.Expired {
				continue
			}
			// The zip is untrusted and can be large: read it under the same
			// bound readModelStatsDir uses, so one artifact cannot exhaust
			// memory. A larger artifact is skipped, not truncated.
			if art.SizeInBytes > modelStatsMaxEntryUncompressed*int64(modelStatsMaxZips) {
				return nil, false, nil
			}
			blob, err := g.getBytes(fmt.Sprintf("/actions/artifacts/%d/zip", art.ID))
			if err != nil {
				return nil, false, err
			}
			return blob, true, nil
		}
		if len(arts.Artifacts) < 100 {
			break
		}
	}
	return nil, false, nil
}

// getBytes fetches an API path whose body is binary -- an artifact zip --
// following GitHub's redirect to the blob store by hand, because the
// Authorization header must be replayed only to the API host: sending the
// bearer token to objects.githubusercontent.com (or anywhere else a redirect
// points) would leak it. The comparison is on scheme and host, not on a
// string prefix, so a redirect to another port or a look-alike hostname is
// not mistaken for the API.
func (g github) getBytes(path string) ([]byte, error) {
	url := g.api + "/repos/" + g.repo + path
	apiScheme, apiHost := splitURLHost(g.api)
	// A client that does not follow redirects itself: Go's own redirect
	// handling re-sends the request headers to a redirect target on the same
	// domain, and the API host and the blob host share one, so the bearer
	// token would reach the blob store. Every hop is made (and authorized)
	// here instead.
	client := *g.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for hop := 0; hop < 5; hop++ {
		request, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if scheme, host := splitURLHost(url); scheme == apiScheme && host == apiHost {
			request.Header.Set("Authorization", "Bearer "+g.token)
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, modelStatsMaxEntryUncompressed*int64(modelStatsMaxZips)+1))
		response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		switch response.StatusCode {
		case http.StatusOK:
			return body, nil
		case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusSeeOther:
			location := response.Header.Get("Location")
			if location == "" {
				return nil, fmt.Errorf("GET %s: %s without a Location", path, response.Status)
			}
			url = location
		default:
			return nil, fmt.Errorf("GET %s: %s: %.300s", path, response.Status, body)
		}
	}
	return nil, fmt.Errorf("GET %s: too many redirects", path)
}

// splitURLHost returns a URL's lowercased scheme and host, or empty strings
// when the URL will not parse. An unparsable URL never matches the API host,
// so the token is not sent.
func splitURLHost(raw string) (scheme, host string) {
	u, err := neturl.Parse(raw)
	if err != nil {
		return "", ""
	}
	return strings.ToLower(u.Scheme), strings.ToLower(u.Host)
}

// ---------------------------------------------------------------- routine path

// runModelStatsRoutine is the report the routine mechanism prints:
// `harness routine-check model-stats` (routines.go) when no
// CODEAF_MODEL_STATS_DIR is set. It is the live path, so the check reads the
// runs itself rather than a directory a workflow prepared: the job needs only
// actions: read and issues: write. checkModelStats (modelstats.go) and this
// share runModelStatsRoutineAt.
func runModelStatsRoutine() (string, error) {
	return runModelStatsRoutineAt(time.Now().UTC())
}
