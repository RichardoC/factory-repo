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
// not sink the report. That is true of an API failure too: a 5xx on one
// page, or one artifact download that 500s or expires mid-page, leaves the
// zips already collected intact and is stated as a bounded skip reason, so
// a report that could have rendered from the other runs still renders.
// An error is returned only when NOTHING was collected -- the listing failed
// on its first page with no zip yet, or no run yielded a zip at all -- which
// is the same empty-report refusal the missing artifact has always been.
func readModelStatsFromRunsConfig(cfg Config, win modelStatsWindow) (modelStatsInput, error) {
	g := newGitHub()
	if g.repo == "" {
		return modelStatsInput{}, errors.New("model-stats: GITHUB_REPOSITORY is required without --from-dir")
	}
	zips, skipped, fatal := fetchModelStatsZips(g, cfg, win)
	if len(zips) == 0 {
		if fatal != nil {
			return modelStatsInput{}, fmt.Errorf("model-stats: %w", fatal)
		}
		return modelStatsInput{}, fmt.Errorf("model-stats: no codeaf-output artifact among the %s runs of the last %d hours", modelStatsWorkflow, win.Hours)
	}
	return parseModelStatsZipsSkipped(zips, skipped, cfg), nil
}

// modelStatsRunIndexedZips is what the API walk hands back: the zips it did
// read, the artifacts it could not read (each with a bounded reason), and the
// failure that stopped the walk, if one did.
//
// fatal is set only on a failure that left NO zip at all: the very first
// page, with nothing collected yet. Any later failure -- a page that 5xxes,
// one artifact that will not download -- is a skip, not an end to the report.
type modelStatsRunIndexedZips struct {
	zips    []*zip.Reader
	skipped []skippedArtifact
	fatal   error
}

// fetchModelStatsZips lists the window's runs and downloads each one's
// codeaf-output zip. The run listing pages with an explicit page= parameter
// until a page comes back short or the total is reached: an implicit
// "start after the last id" cursor is not reliable across a filtered list.
//
// A page that fails while zips are already in hand stops the paging and is
// recorded as a skip: the runs already read are a real report of the window,
// and asking again would only turn one upstream hiccup into no report. A
// page that fails with nothing collected is returned as fatal, because there
// the alternative is an empty comment. There is no retry: one failure is
// either fatal (once) or skipped (once), never retried.
func fetchModelStatsZips(g github, cfg Config, win modelStatsWindow) ([]*zip.Reader, []skippedArtifact, error) {
	var out modelStatsRunIndexedZips
	perPage := 100
	limited := !win.From.IsZero()
	for page := 1; page <= modelStatsMaxRunPages; page++ {
		var runs runListPage
		path := fmt.Sprintf("/actions/workflows/%s/runs?status=completed&per_page=%d&page=%d",
			modelStatsWorkflow, perPage, page)
		if err := g.get(path, &runs); err != nil {
			if len(out.zips) == 0 {
				// Nothing has been collected, so there is no report to
				// protect: the listing failure is the report's failure.
				out.fatal = err
				return out.zips, out.skipped, out.fatal
			}
			// Runs are already in hand. Stop paging -- the next page is the
			// same upstream that just failed -- and state it, bounded: the
			// path carries the page number, which is this program's own
			// integer, and the API's error text is never copied.
			out.skipped = append(out.skipped, skippedArtifact{
				Reason: skipReason("run listing page %d: request failed (%d zip(s) already read)", page, len(out.zips)),
			})
			break
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
			data, reason, ok := downloadCodeafOutput(g, run)
			if !ok {
				// A run without a codeaf-output artifact is the routine's
				// ordinary reason to pass over a run, already counted by the
				// caller's run count; only an artifact that exists but could
				// not be read is a skip worth stating (reason != "").
				if reason != "" {
					out.skipped = append(out.skipped, skippedArtifact{Reason: reason})
				}
				continue
			}
			zr, zipErr := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if zipErr != nil {
				// The artifact downloaded but is not a zip -- a truncated
				// body, an expired blob, something that is not an archive at
				// all. Before this, zipErr was discarded and the artifact
				// vanished from the report entirely: it is now counted and
				// named, and the reason is built below, never taken from the
				// bytes that failed to parse.
				out.skipped = append(out.skipped, skippedArtifact{
					Reason: skipReason("run %d: not a zip (artifact %d bytes, unreadable)", run.ID, len(data)),
				})
				continue
			}
			out.zips = append(out.zips, zr)
		}
		if page*perPage >= runs.TotalCount && limited {
			break
		}
		if len(runs.WorkflowRuns) < perPage {
			break
		}
	}
	if len(out.zips) == 0 && len(out.skipped) > 0 && out.fatal == nil {
		// Every artifact there was failed to read. That is still nothing to
		// render, and the error names it rather than printing an empty
		// comment: the reasons travel back so the caller could report them.
		out.fatal = errors.New("every codeaf-output artifact in the window was unreadable")
	}
	return out.zips, out.skipped, out.fatal
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
// zip bytes. ok is false when the run's artifact cannot be had at all -- the
// run has none, the listing call failed, the download failed, the artifact is
// larger than any codeaf-output artifact should be -- and it does so WITHOUT
// an error, so one bad run is skipped and the runs around it still render.
//
// The two reasons a run is passed over are kept apart, because they are not
// the same fact: a run with no codeaf-output artifact is the routine's
// ordinary "this run has nothing to report" (reason == "", already counted by
// run count), while an artifact that exists and will not download is a skip
// worth stating, in a reason this file writes -- the API's response text is
// never copied into it.
//
// The artifact name carries the run id and attempt the workflow sets:
// codeaf-output-<id>-<attempt>.
func downloadCodeafOutput(g github, run workflow) (data []byte, reason string, ok bool) {
	want := fmt.Sprintf("codeaf-output-%d-%d", run.ID, run.RunAttempt)
	for page := 1; page <= 2; page++ {
		var arts artifactListPage
		path := fmt.Sprintf("/actions/runs/%d/artifacts?per_page=100&page=%d", run.ID, page)
		if err := g.get(path, &arts); err != nil {
			return nil, skipReason("run %d: artifact listing failed", run.ID), false
		}
		for _, art := range arts.Artifacts {
			if art.Name != want || art.Expired {
				continue
			}
			// The zip is untrusted and can be large: read it under the same
			// bound readModelStatsDir uses, so one artifact cannot exhaust
			// memory. A larger artifact is skipped, not truncated.
			if art.SizeInBytes > modelStatsMaxEntryUncompressed*int64(modelStatsMaxZips) {
				return nil, skipReason("run %d: artifact over the size cap", run.ID), false
			}
			blob, err := g.getBytes(fmt.Sprintf("/actions/artifacts/%d/zip", art.ID))
			if err != nil {
				return nil, skipReason("run %d: artifact download failed", run.ID), false
			}
			return blob, "", true
		}
		if len(arts.Artifacts) < 100 {
			break
		}
	}
	return nil, "", false
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
			// The response body of a failed API call is untrusted text, and
			// this error string ends up in a log or a task comment. It is not
			// echoed -- not even its first bytes: the note is structural and
			// bounded to what this program measured itself -- the status, the
			// content type the server declared, and how many bytes came back.
			// A body that said "@everyone, run this" therefore cannot travel
			// through here, and the byte count still tells a reader whether a
			// body arrived at all.
			return nil, fmt.Errorf("GET %s: %s (content-type %q, %d bytes read)",
				path, response.Status, contentTypeOf(response.Header.Get("Content-Type")), len(body))
		}
	}
	return nil, fmt.Errorf("GET %s: too many redirects", path)
}

// contentTypeOf reduces a Content-Type header to the media type alone, and
// to a fixed placeholder when the header is missing or is not one: like the
// status and the byte count it reports beside, it is a structural fact about
// the response, not text from it. The charset and any other parameter are
// dropped, and a value that will not parse is not repeated.
func contentTypeOf(header string) string {
	if header == "" {
		return "none"
	}
	media := strings.TrimSpace(strings.SplitN(header, ";", 2)[0])
	for _, r := range media {
		if r <= ' ' || r == '"' || r == '\\' || r > '\x7e' {
			return "unrecognized"
		}
	}
	if media == "" {
		return "none"
	}
	return media
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
