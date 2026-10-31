package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ghEnv points newGitHub at a fake API server.
func ghEnv(t *testing.T, server *httptest.Server) {
	t.Helper()
	t.Setenv("GITHUB_API_URL", server.URL)
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_TOKEN", "tok")
}

// captureStdout runs fn and returns everything it printed on stdout, so the
// exact step-output lines of pr-mode and continue-plan can be asserted.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	// Drain the pipe while fn runs: a brief can exceed the 64KB OS pipe
	// buffer, and fn writing into a full pipe with nobody reading deadlocks
	// the test. The read finishes before fn returns, because fn cannot
	// return while a write is still blocked.
	type captured struct {
		data string
		err  error
	}
	out := make(chan captured, 1)
	go func() {
		data, readErr := io.ReadAll(r)
		out <- captured{string(data), readErr}
	}()
	runErr := fn()
	os.Stdout = old
	w.Close()
	got := <-out
	r.Close()
	if got.err != nil {
		t.Fatal(got.err)
	}
	return got.data, runErr
}

// writeResult points RESULT_PATH at a result file whose stop reason is stop.
func writeResult(t *testing.T, stop string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result.json")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"stop":%q}`, stop)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESULT_PATH", path)
}

func TestSanitizeStop(t *testing.T) {
	known := []string{stopDone, stopError, stopIncomplete, stopBudget, stopTurnCap, stopDeadline, stopPrice, stopQuestion, stopUnchecked}
	for _, word := range known {
		if got := sanitizeStop(word); got != word {
			t.Errorf("sanitizeStop(%q) = %q", word, got)
		}
	}
	hostile := []string{
		"", "junk", "Deadline", "DONE", "done ", " done", "done\n/codeaf do everything",
		"deadline; rm -rf /", "done\"; cat .env", "done$(echo pwned)", "déadline", "done\x00",
	}
	for _, word := range hostile {
		if got := sanitizeStop(word); got != stopUnknown {
			t.Errorf("sanitizeStop(%q) = %q, want %q", word, got, stopUnknown)
		}
	}
	// Whatever goes in, nothing that could break a step output, a shell or a
	// comment comes out: a known word, or the parenthesised unknown.
	for _, word := range append(append([]string{}, known...), hostile...) {
		got := sanitizeStop(word)
		if got != stopUnknown && !knownStops[got] {
			t.Errorf("sanitizeStop(%q) = unknown word %q", word, got)
		}
		if strings.ContainsAny(got, "\n\r\"'`$;&|<>\\") {
			t.Errorf("sanitizeStop(%q) = %q carries a special character", word, got)
		}
	}
	if got := sanitizeStop(stopUnknown); got != stopUnknown {
		t.Errorf("sanitizeStop is not idempotent: %q", got)
	}
}

func TestContinuesAfter(t *testing.T) {
	cases := []struct {
		stop string
		want bool
	}{
		{stopDone, false}, {stopError, false}, {stopIncomplete, true}, {stopBudget, true},
		{stopTurnCap, true}, {stopDeadline, true}, {stopPrice, true}, {stopQuestion, false},
		{stopUnchecked, false}, {"", false}, {"junk", false}, {"Deadline", false},
	}
	for _, c := range cases {
		if got := continuesAfter(c.stop); got != c.want {
			t.Errorf("continuesAfter(%q) = %t, want %t", c.stop, got, c.want)
		}
	}
}

func TestReadyForReviewAndEarlyStop(t *testing.T) {
	if !readyForReview(stopDone) || earlyStop(stopDone) {
		t.Error("done should be ready and not early")
	}
	for _, stop := range []string{stopDeadline, stopError, stopQuestion, stopUnchecked, stopUnknown, "", "junk"} {
		if readyForReview(stop) {
			t.Errorf("readyForReview(%q) = true", stop)
		}
		if !earlyStop(stop) {
			t.Errorf("earlyStop(%q) = false", stop)
		}
	}
}

func TestContinuationAccounting(t *testing.T) {
	for reports, want := range map[int]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 3} {
		if got := continuationsUsed(reports); got != want {
			t.Errorf("continuationsUsed(%d) = %d, want %d", reports, got, want)
		}
	}
	for used, want := range map[int]bool{0: false, 1: false, 2: true, 3: true} {
		if got := continuationCapReached(used); got != want {
			t.Errorf("continuationCapReached(%d) = %t, want %t", used, got, want)
		}
	}
}

func TestCountReports(t *testing.T) {
	bot := user{Login: planReportBotLogin}
	ordinary := []issueComment{
		{ID: 1, Body: "just a comment, no marker"},
		{ID: 2, Body: continueBody(stopDeadline, 0)}, // a continue comment is not a report
		{ID: 3, Body: capBody(stopDeadline)},
		{ID: 4, Body: ""},
	}
	if got := countReports(ordinary); got != 0 {
		t.Errorf("countReports(ordinary) = %d, want 0", got)
	}
	mixed := append(ordinary,
		issueComment{ID: 5, User: bot, Body: reportMarker + "\n### CodeAF run — done"},
		issueComment{ID: 6, User: bot, Body: "text " + reportMarker + " more text"},
		issueComment{ID: 7, User: bot, Body: reportMarker},
	)
	if got := countReports(mixed); got != 3 {
		t.Errorf("countReports(mixed) = %d, want 3", got)
	}

	// The same marker on a comment by anyone but the workflow bot is not a
	// report (issue #219): a forged marker can neither inflate the count
	// (spending the cap early) nor suppress it (a comment that reads like a
	// failed publish is still not counted either way).
	forged := append(mixed,
		issueComment{ID: 8, User: user{Login: "someone"}, Body: reportMarker + "\n### CodeAF run — done"},
		issueComment{ID: 9, User: user{Login: "github-actions[bot]-fan"}, Body: failedPublishHeading},
	)
	if got := countReports(forged); got != 3 {
		t.Errorf("countReports(forged) = %d, want 3", got)
	}

	// A bot report whose publish failed did not land its patch: it does not
	// spend a continuation slot.
	failed := append(mixed,
		issueComment{ID: 10, User: bot, Body: failedReportHeadings[0] + "\n| stop / ok | done / `true` |\n"},
	)
	if got := countReports(failed); got != 3 {
		t.Errorf("countReports(failed publish) = %d, want 3", got)
	}
	// A near miss of either heading is an ordinary report: the match is an
	// exact prefix, so a heading that merely starts like one of them counts.
	nearMiss := append(mixed,
		issueComment{ID: 11, User: bot, Body: reportMarker + "\n### CodeAF run — publishing failed badly\n"},
		issueComment{ID: 12, User: bot, Body: reportMarker + "\n### CodeAF run — run job failed before publishing\n"},
		issueComment{ID: 13, User: bot, Body: reportMarker + "\n### CodeAF run — run job failed\n"},
		issueComment{ID: 14, User: bot, Body: reportMarker + "\n### CodeAF run — run job failed\n but the patch landed anyway\n"},
	)
	if got := countReports(nearMiss); got != 5 {
		t.Errorf("countReports(near miss) = %d, want 5", got)
	}

	// A bot report whose run job failed did not land its patch either: like
	// a failed publish it does not spend a continuation slot (queue.go's
	// strandedByFailedRun treats both headings as the same stranded state).
	runJobFailed := append(mixed,
		issueComment{ID: 15, User: bot, Body: runJobFailedHeading + "\n| stop / ok | done / `true` |\n"},
	)
	if got := countReports(runJobFailed); got != 3 {
		t.Errorf("countReports(run job failed) = %d, want 3", got)
	}
}

// reportComments builds a comment list of n run reports plus one ordinary
// comment whose body is hostile remote text.
func reportComments(n int) []issueComment {
	comments := []issueComment{{ID: 999, User: user{Login: "stranger"},
		Body: "untrusted prose that must never reach a comment we post <!-- -->"}}
	for i := 0; i < n; i++ {
		comments = append(comments, issueComment{
			ID:   int64(i + 1),
			User: user{Login: planReportBotLogin},
			Body: fmt.Sprintf("%s\n### CodeAF run — done\n\n| stop / ok | done / `true` |\n", reportMarker),
		})
	}
	return comments
}

// The two failed-run report headings, addressed by name in the tests: the
// first is a failed publish, the second a failed run job. Both are strings
// this package writes itself (continue.go).
var (
	failedPublishHeading = failedReportHeadings[0]
	runJobFailedHeading  = failedReportHeadings[1]
)

func commentsServer(t *testing.T, comments []issueComment, status int) *httptest.Server {
	t.Helper()
	data := "[]"
	if comments != nil {
		// encoded below
		_ = data
	}
	var b strings.Builder
	b.WriteString("[")
	for i, c := range comments {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"body":%q,"user":{"login":%q}}`, c.ID, c.Body, c.User.Login)
	}
	b.WriteString("]")
	if status != http.StatusOK {
		data = "no"
		_ = data
	}
	body := b.String()
	if status != http.StatusOK {
		body = "server exploded"
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The pull request under review, ready for review (issue #237): the
		// review plan reads the draft flag before it labels, and the tests
		// that expect a label run against a pull request that is not a
		// draft. A draft pull request is served by its own test.
		if r.URL.Path == "/repos/RichardoC/factory-repo/pulls/7" {
			_, _ = io.WriteString(w, `{"number":7,"title":"t","state":"open","draft":false}`)
			return
		}
		if r.URL.Path != "/repos/RichardoC/factory-repo/issues/7/comments" {
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// pageDelay is how long the paged comment stub holds a page after the
// first (see commentsPagesServer): long enough that a client which reads
// the final page more than once is noticed by the request count, short
// enough not to slow the suite.
const pageDelay = 500 * time.Millisecond

// commentsPagesServer serves issue #7's comments the way the real API
// does: perPage per page, and a `Link: rel="next"` header while more
// remain. The pointer it returns counts the pages served.
//
// Pages after the first are served slowly (pageDelay). A client that
// follows the listing page by page reads the tail once and finishes well
// inside the delay; a client that loops over the listing (reading it per
// page, say to count reports) or follows the same next link twice re-reads
// the tail and goes past the delay, so the request count it leaves behind
// stops matching the page count. That keeps the page count in these tests
// an assertion about the read, not about a single pass that happens to
// append.
func commentsPagesServer(t *testing.T, perPage int, comments []issueComment) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRepoID(w, r) {
			return
		}
		if r.URL.Path != "/repos/RichardoC/factory-repo/issues/7/comments" {
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
			return
		}
		requests.Add(1)
		page := 1
		if s := r.URL.Query().Get("page"); s != "" {
			if v, err := strconv.Atoi(s); err == nil {
				page = v
			}
		}
		if page > 1 {
			time.Sleep(pageDelay) // the tail page is slow, as a second page in the real API can be
		}
		start := (page - 1) * perPage
		if start >= len(comments) {
			http.Error(w, "page past the end", http.StatusBadRequest)
			return
		}
		end := start + perPage
		if end > len(comments) {
			end = len(comments)
		}
		var b strings.Builder
		b.WriteString("[")
		for i := start; i < end; i++ {
			if i > start {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%d,"body":%q,"user":{"login":%q}}`, comments[i].ID, comments[i].Body, comments[i].User.Login)
		}
		b.WriteString("]")
		if end < len(comments) {
			w.Header().Set("Link", idNextLink(r, fmt.Sprintf("/issues/7/comments?per_page=%d&page=%d", perPage, page+1)))
		}
		_, _ = io.WriteString(w, b.String())
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// stubRepoID is the numeric id GitHub answers for RichardoC/factory-repo —
// the id segment GitHub's real next links carry (issue #390).
const stubRepoID = 1420977662

// stubRepoPath is the /repos/<owner>/<repo> prefix the fixtures' list reads
// live under.
const stubRepoPath = "/repos/RichardoC/factory-repo"

// serveRepoID answers GET /repos/RichardoC/factory-repo with this
// repository's numeric id — the lookup listNext makes the first time a next
// link uses GitHub's real /repositories/<id>/ form (issue #390). It reports
// whether it served the request, so a stub handler can fall through to its
// own routes otherwise.
func serveRepoID(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != stubRepoPath {
		return false
	}
	_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%d}`, stubRepoID))
	return true
}

// idNextLink renders a next link in GitHub's real numeric form (issue
// #390): <base>/repositories/<id>/tail. tail is the request path below the
// repository with its query, e.g. the comments listing with its page param.
func idNextLink(r *http.Request, tail string) string {
	return fmt.Sprintf("<%s/repositories/%d%s>; rel=\"next\"", serverURL(r), stubRepoID, tail)
}

// idTail strips the /repos/<owner>/<repo> prefix from the request's own URI,
// leaving the path below the repository with its query — the tail
// idNextLink wraps.
func idTail(r *http.Request) string {
	return strings.TrimPrefix(r.URL.RequestURI(), stubRepoPath)
}

// serverURL rebuilds the request's own base URL, so the served next link
// points back at the stub the way the real API's links point at itself.
func serverURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// straddleComments builds the pull request the continuation cap straddles
// (issue #223), in the shape the pagination has to read whole: `filler`
// ordinary comments, then `page1Reports` of the run reports, then
// `pastReports` more of them, so the API's page boundary falls in the
// middle of the reports rather than before or after them. "More than 100
// report comments across two pages" only means what it says when some
// report is on page 1 and at least one more sits past index 100: a listing
// that stops at the first page then sees a below-cap count, and only a read
// that follows the next link sees every report.
//
// It starts with one ordinary comment whose body is hostile remote text, so
// the assertions can show it never reaches a comment we post.
func straddleComments(filler, page1Reports, pastReports int) []issueComment {
	comments := []issueComment{{ID: 999, Body: "untrusted prose that must never reach a comment we post <!-- -->"}}
	reports := 0
	addReports := func(n int) {
		for i := 0; i < n; i++ {
			reports++
			comments = append(comments, issueComment{
				ID:   int64(reports),
				Body: fmt.Sprintf("%s\n### CodeAF run — done\n\n| stop / ok | done / `true` |\n", reportMarker),
				User: user{Login: planReportBotLogin},
			})
		}
	}
	addReports(page1Reports)
	for i := 0; i < filler; i++ {
		comments = append(comments, issueComment{
			ID:   int64(1000 + i),
			Body: fmt.Sprintf("an ordinary reviewer comment, number %d", i),
		})
	}
	addReports(pastReports)
	return comments
}

// TestCountReportsAcrossPages is the issue #223 stub: the stub serves more
// than 100 report comments across two pages — 20 of them, one on the first
// page and 19 past index 100 — and a read that follows the next link must
// reach them whole, so countReports sees all 20 and the cap fires.
//
// Its shape is what makes the test discriminate, and it is stated here so
// the numbers stay checkable: page 1 is indices 0..99 and holds 1 ordinary
// leading comment + the first report + 98 ordinary filler comments; the
// LAST report is the 20th, at index 118. Index 100 — the first comment of
// page 2 — is the second report, so every report past the first is on
// page 2. Page 1 therefore carries 1 report comment and 19 report comments
// sit past index 100, both non-zero, which is the straddle the bullet
// describes.
//
// A one-page read cannot pass this test: the pre-change reply (a single GET
// of the first page, no next link followed) sees 1 report, and used = 1
// (continuePlan counts every earlier report, issue #219) is below
// maxContinuations = 2, so countReports under-counts,
// continuationCapReached is false and continuePlan answers "continue" with
// used = 1 instead of firing the cap. `reportComments`, the
// other stub, puts the reports first, so 100 of those fit inside page 1 and
// the one-page read already counted 99 and fired the cap — assertion green,
// harm untested. That is why the reports here straddle the boundary instead.
func TestCountReportsAcrossPages(t *testing.T) {
	perPage := 100
	// The stub's shape in numbers, so a change to either constant still
	// reads as the straddle it is: 20 report comments in all, 1 of them on
	// page 1 and 19 past index 100. Below index 100 sit 1 ordinary leading
	// comment, that report and 98 ordinary filler comments.
	const page1Reports, page2Reports = 1, 19
	comments := straddleComments(perPage-1-page1Reports, page1Reports, page2Reports)
	if len(comments) <= perPage {
		t.Fatalf("the stub serves %d comments, a first page of %d or less: no next link, nothing paged", len(comments), perPage)
	}
	if got := countReports(comments); got != page1Reports+page2Reports {
		t.Fatalf("the stub serves %d report comments, want %d: more than 100 report comments across two pages", got, page1Reports+page2Reports)
	}
	// The straddle itself: reports on both sides of the boundary at index
	// 100, so neither a one-page read nor a two-page read that drops the
	// first page can see them all.
	if got := countReports(comments[:perPage]); got != page1Reports {
		t.Fatalf("the first page (%d comments) carries %d report comments, want %d: the reports must straddle the boundary, some on page 1", perPage, got, page1Reports)
	}
	if got := countReports(comments[perPage:]); got != page2Reports {
		t.Fatalf("past index %d (%d comments) carry %d report comments, want %d: the reports must straddle the boundary, most past it", perPage, len(comments)-perPage, got, page2Reports)
	}
	// Discriminating: a page-1-only read is below the cap, so the
	// pre-change, unpaged read cannot pass the assertions below.
	if got := continuationCapReached(countReports(comments[:perPage])); got {
		t.Fatalf("the first page alone reaches the cap (its %d reports give used = %d); the shape must straddle the boundary, not straddle the cap", page1Reports, page1Reports)
	}
	server, requests := commentsPagesServer(t, perPage, comments)
	ghEnv(t, server)
	gh := newGitHub()
	started := time.Now()
	action, body, used, err := continuePlan(gh, 7, stopDeadline)
	if err != nil {
		t.Fatal(err)
	}
	if want := page1Reports + page2Reports; used != want {
		t.Fatalf("used = %d, want %d: the read must see all %d report comments, the %d on page 1 and the %d past it", used, want, page1Reports+page2Reports, page1Reports, page2Reports)
	}
	if action != "cap" {
		t.Fatalf("action = %q, used = %d; want the cap fired (used >= %d) — a one-page read leaves used = %d here", action, used, maxContinuations, page1Reports)
	}
	if want := capBody(stopDeadline); body != want {
		t.Errorf("body = %q, want the cap comment %q", body, want)
	}
	// Both pages: page 2 holds every report past index 100, so a read that
	// stops at the first page both counts too few and makes one request.
	if n := requests.Load(); n != 2 {
		t.Errorf("pages read = %d, want 2: the second page holds the %d reports past index %d", n, page2Reports, perPage)
	}
	if elapsed := time.Since(started); elapsed < pageDelay {
		t.Errorf("continuePlan answered in %s, faster than the tail page's own %s and request count unmatched", elapsed, pageDelay)
	}
}

// TestCountReportsSinglePage is the other issue #223 stub: a reply with no
// `Link` header answers after one request, with nothing re-read, so the
// single-page case is unchanged by the pagination.
func TestCountReportsSinglePage(t *testing.T) {
	server, requests := commentsPagesServer(t, 100, reportComments(1))
	ghEnv(t, server)
	gh := newGitHub()
	action, body, used, err := continuePlan(gh, 7, stopDeadline)
	if err != nil {
		t.Fatal(err)
	}
	// One earlier report means one earlier run (issue #219): used = 1.
	if action != "continue" || used != 1 {
		t.Fatalf("with one report, action = %q, used = %d; want continue and one used", action, used)
	}
	if want := continueBody(stopDeadline, 1); body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1: a reply with no next link needs no extra request", n)
	}
}

// TestBriefDegradesOnListPageCap is the brief-builder half of the issue #223
// follow-up: a list read that stops at the page cap must not kill the brief.
// The stub below is a comments listing that always offers a next link, so a
// getList on it can never finish reading the listing and comes back with the
// page-cap error alone; before the fix runBrief returned that error and wrote
// no brief at all, while its sibling reads (previousReportSection,
// reviewFindingsSection) degraded to a note. Here the brief must still be
// written, carry the note, and be a whole brief — the sections after the
// conversation are still there.
func TestBriefDegradesOnListPageCap(t *testing.T) {
	// The stub serves the pr-mode brief (prRoutes' paths), and the comments
	// listing always claims another page. Every other path is served as
	// usual, so the failure under test is the page cap and nothing else.
	comments := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRepoID(w, r) {
			return
		}
		routes := prRoutes()
		body, ok := routes[r.URL.Path]
		if !ok {
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.URL.Path == "/repos/RichardoC/factory-repo/issues/7/comments" {
			comments++
			w.Header().Set("Link", idNextLink(r, fmt.Sprintf("/issues/7/comments?per_page=100&page=%d", comments+1)))
			body = `[{"id":1,"body":"a comment","user":{"login":"alice","type":"User"},"author_association":"MEMBER"}]`
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	briefPath := briefEnv(t, server, "pr", prEvent)

	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = devnull
	runErr := runBrief()
	os.Stdout = old
	_ = devnull.Close()

	if runErr != nil {
		t.Fatalf("runBrief with a capped comments listing: %v, want the brief to degrade instead", runErr)
	}
	data, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(data)
	if !strings.Contains(brief, "(conversation unavailable: ") {
		t.Errorf("the capped conversation is not degraded in the brief:\n%s", briefSection(brief, "Conversation"))
	}
	// The brief must still be a brief: the sections the builder appends
	// after the conversation are there, so a capped read degraded rather
	// than truncated the build.
	if !strings.Contains(brief, "## Conversation") {
		t.Error("the Conversation heading is missing from the brief")
	}
	if briefSection(brief, "Open review findings (after the last CodeAF push)") == "" {
		t.Error("the brief stops after the capped read: the review-findings section is missing")
	}
}

// TestBriefConversationDegradesOnlyOnPageCap pins the other half of the
// contract thread() now has: the page cap degrades to a note, while a next
// link that is not a link this client may follow still fails. Only the cap
// means "truncated, not broken"; a malformed header is a read that did not
// happen, and the brief says so instead of pretending it has the thread.
func TestBriefConversationDegradesOnlyOnPageCap(t *testing.T) {
	// nextLink is the one place the header's shape is decided, so this is
	// where a chain of next links becomes a path (pages capped) and a
	// malformed header becomes the error thread() re-raises.
	t.Run("chain of next links is a page-cap path", func(t *testing.T) {
		server, requests := nextPageForeverServer(t)
		gh := github{api: server.URL, repo: "RichardoC/factory-repo", token: "tok", client: server.Client()}
		conversation, err := gh.thread(7)
		if err != nil {
			t.Fatalf("thread on a capped listing: %v, want a note and no error", err)
		}
		if !strings.HasPrefix(conversation, "(conversation unavailable: ") {
			t.Errorf("thread = %q, want the cap note", conversation)
		}
		if n := requests.Load(); n != maxListPages {
			t.Errorf("pages requested = %d, want exactly maxListPages (%d)", n, maxListPages)
		}
	})
	t.Run("bad next link still fails", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", "<:not a url>; rel=\"next\"")
			_, _ = io.WriteString(w, `[]`)
		}))
		t.Cleanup(server.Close)
		gh := github{api: server.URL, repo: "RichardoC/factory-repo", token: "tok", client: server.Client()}
		_, err := gh.thread(7)
		if err == nil {
			t.Error("thread on a malformed next link: no error, want the read to fail")
		}
		if isPageCapError(err) {
			t.Errorf("a malformed next link is not a page cap: %v", err)
		}
	})
}

// nextPageForeverServer serves a comments listing that always offers a next
// link: a getList reading it stops at the page cap, on purpose. The counter
// is how many pages were asked for.
func nextPageForeverServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveRepoID(w, r) {
			return
		}
		requests.Add(1)
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		n, err := strconv.Atoi(page)
		if err != nil {
			n = 1
		}
		w.Header().Set("Link", idNextLink(r, fmt.Sprintf("%s?per_page=100&page=%d", strings.TrimPrefix(r.URL.Path, stubRepoPath), n+1)))
		_, _ = io.WriteString(w, `[{"id":1,"body":"a comment","user":{"login":"alice","type":"User"},"author_association":"MEMBER"}]`)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestContinuePlan(t *testing.T) {
	// Issue #219: continue-plan runs before the stopping run has posted its
	// own report, so every report comment is a run that came before and
	// used is the plain report count. Two reports (the opener and one
	// continuation) have spent the cap of 2.
	stop := stopDeadline
	for reports, wantAction := range map[int]string{0: "continue", 1: "continue", 2: "cap", 3: "cap", 4: "cap"} {
		t.Run(fmt.Sprintf("reports=%d", reports), func(t *testing.T) {
			server := commentsServer(t, reportComments(reports), http.StatusOK)
			ghEnv(t, server)
			gh := newGitHub()
			action, body, used, err := continuePlan(gh, 7, stop)
			if err != nil {
				t.Fatal(err)
			}
			if action != wantAction {
				t.Fatalf("continuePlan(reports=%d) action = %q, want %q", reports, action, wantAction)
			}
			if used != reports {
				t.Fatalf("used = %d, want %d (issue #219: the count includes the opener's report)", used, reports)
			}
			if action == "continue" {
				if want := continueBody(stop, used); body != want {
					t.Errorf("body = %q, want %q", body, want)
				}
				if !strings.HasPrefix(body, "/codeaf continue: deadline\n") {
					t.Errorf("body does not start with the continue command: %q", body)
				}
				if !strings.Contains(body, fmt.Sprintf("Continuation %d of %d", used+1, maxContinuations)) {
					t.Errorf("body does not name the continuation number: %q", body)
				}
			} else {
				if want := capBody(stop); body != want {
					t.Errorf("body = %q, want %q", body, want)
				}
				for _, want := range []string{"cap (2 per issue) is reached", "stopped early again", "stays a draft", "/codeaf comment"} {
					if !strings.Contains(body, want) {
						t.Errorf("cap body missing %q: %q", want, body)
					}
				}
			}
			// The body is built from fixed text, the sanitized stop word and
			// counters only: none of the hostile comment text may appear.
			if strings.Contains(body, "untrusted prose") || strings.Contains(body, "<!--") {
				t.Errorf("remote text leaked into the comment body: %q", body)
			}
			if strings.Contains(body, reportMarker) {
				t.Errorf("the posted comment must not carry the report marker: %q", body)
			}
		})
	}

	// Stops that must not be continued return no action at all, and a
	// failing API is an error, not an empty plan. A done stop is decided by
	// the mergeable_state probe instead: an unconflicted PR gets no action,
	// and its fetch errors surface (TestContinuePlanContinuesADirtyPR
	// covers the conflicted and failing sides).
	cleanServer := prStateServer(t, "clean", reportComments(1))
	ghEnv(t, cleanServer)
	if action, body, used, err := continuePlan(newGitHub(), 7, stopDone); err != nil || action != "" || body != "" || used != 0 {
		t.Errorf("continuePlan(stop=done, clean) = (%q, %q, %d, %v), want no action", action, body, used, err)
	}
	for _, stop := range []string{stopError, stopQuestion, stopUnchecked, "", "junk", stopUnknown} {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		action, body, used, err := continuePlan(newGitHub(), 7, stop)
		if err != nil || action != "" || body != "" || used != 0 {
			t.Errorf("continuePlan(stop=%q) = (%q, %q, %d, %v), want no action", stop, action, body, used, err)
		}
	}
	server := commentsServer(t, nil, http.StatusInternalServerError)
	ghEnv(t, server)
	if _, _, _, err := continuePlan(newGitHub(), 7, stopDeadline); err == nil {
		t.Error("a failing comment fetch must return an error")
	}
}

func TestPRMode(t *testing.T) {
	const earlyOutput = "early=true\nready=false\n"
	const readyOutput = "early=false\nready=true\n"

	t.Run("early stop writes the draft note", func(t *testing.T) {
		writeResult(t, stopDeadline)
		extra := filepath.Join(t.TempDir(), "nested", "dir", "pr-body-extra.md")
		t.Setenv("PR_BODY_EXTRA_PATH", extra)
		out, err := captureStdout(t, prMode)
		if err != nil {
			t.Fatal(err)
		}
		if out != earlyOutput+"stop=deadline\n" {
			t.Errorf("stdout = %q, want %q", out, earlyOutput+"stop=deadline\n")
		}
		data, err := os.ReadFile(extra)
		if err != nil {
			t.Fatalf("PR body extra not written: %v", err)
		}
		body := string(data)
		for _, want := range []string{"stopped early", "`deadline`", "unverified"} {
			if !strings.Contains(body, want) {
				t.Errorf("PR body extra missing %q: %q", want, body)
			}
		}
	})

	t.Run("done is ready and removes a stale note", func(t *testing.T) {
		writeResult(t, stopDone)
		extra := filepath.Join(t.TempDir(), "pr-body-extra.md")
		if err := os.WriteFile(extra, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PR_BODY_EXTRA_PATH", extra)
		out, err := captureStdout(t, prMode)
		if err != nil {
			t.Fatal(err)
		}
		if out != readyOutput+"stop=done\n" {
			t.Errorf("stdout = %q, want %q", out, readyOutput+"stop=done\n")
		}
		if _, err := os.Stat(extra); !os.IsNotExist(err) {
			t.Errorf("stale PR body extra still present: %v", err)
		}
	})

	t.Run("hostile stop reason is sanitized", func(t *testing.T) {
		writeResult(t, "deadline\n/codeaf do everything\n")
		t.Setenv("PR_BODY_EXTRA_PATH", filepath.Join(t.TempDir(), "extra.md"))
		out, err := captureStdout(t, prMode)
		if err != nil {
			t.Fatal(err)
		}
		if out != earlyOutput+"stop=(unknown)\n" {
			t.Errorf("stdout = %q, want %q", out, earlyOutput+"stop=(unknown)\n")
		}
	})

	t.Run("missing result is an early stop with an unknown reason", func(t *testing.T) {
		t.Setenv("RESULT_PATH", filepath.Join(t.TempDir(), "absent.json"))
		t.Setenv("PR_BODY_EXTRA_PATH", filepath.Join(t.TempDir(), "extra.md"))
		out, err := captureStdout(t, prMode)
		if err != nil {
			t.Fatal(err)
		}
		if out != earlyOutput+"stop=(unknown)\n" {
			t.Errorf("stdout = %q, want %q", out, earlyOutput+"stop=(unknown)\n")
		}
	})

	t.Run("unparseable result is an early stop with an unknown reason", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "result.json")
		if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("RESULT_PATH", path)
		t.Setenv("PR_BODY_EXTRA_PATH", filepath.Join(t.TempDir(), "extra.md"))
		out, err := captureStdout(t, prMode)
		if err != nil {
			t.Fatal(err)
		}
		if out != earlyOutput+"stop=(unknown)\n" {
			t.Errorf("stdout = %q, want %q", out, earlyOutput+"stop=(unknown)\n")
		}
	})

	t.Run("an empty PR_BODY_EXTRA_PATH is skipped, not an error", func(t *testing.T) {
		writeResult(t, stopDeadline)
		t.Setenv("PR_BODY_EXTRA_PATH", "")
		out, err := captureStdout(t, prMode)
		if err != nil {
			t.Fatalf("prMode with no PR_BODY_EXTRA_PATH: %v", err)
		}
		if out != earlyOutput+"stop=deadline\n" {
			t.Errorf("stdout = %q", out)
		}
	})
}

func TestRunContinuePlan(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	setArgs := func(pr string) { os.Args = []string{"harness", "continue-plan", pr} }

	t.Run("continues under the cap", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeResult(t, stopDeadline)
		comment := filepath.Join(t.TempDir(), "continue-comment.md")
		t.Setenv("CONTINUE_COMMENT_PATH", comment)
		out, err := captureStdout(t, runContinuePlan)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=continue\n" {
			t.Errorf("stdout = %q, want action=continue", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if want := continueBody(stopDeadline, 1); string(data) != want {
			t.Errorf("comment = %q, want %q", data, want)
		}
	})

	t.Run("cap reached", func(t *testing.T) {
		server := commentsServer(t, reportComments(3), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeResult(t, stopDeadline)
		comment := filepath.Join(t.TempDir(), "cap-comment.md")
		t.Setenv("CONTINUE_COMMENT_PATH", comment)
		out, err := captureStdout(t, runContinuePlan)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=cap\n" {
			t.Errorf("stdout = %q, want action=cap", out)
		}
		data, err := os.ReadFile(comment)
		if err != nil {
			t.Fatalf("comment not written: %v", err)
		}
		if want := capBody(stopDeadline); string(data) != want {
			t.Errorf("comment = %q, want %q", data, want)
		}
	})

	t.Run("missing result means no action", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		t.Setenv("RESULT_PATH", filepath.Join(t.TempDir(), "absent.json"))
		comment := filepath.Join(t.TempDir(), "never-written.md")
		t.Setenv("CONTINUE_COMMENT_PATH", comment)
		out, err := captureStdout(t, runContinuePlan)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=none\n" {
			t.Errorf("stdout = %q, want action=none", out)
		}
		if _, err := os.Stat(comment); !os.IsNotExist(err) {
			t.Errorf("a comment was written despite no action: %v", err)
		}
	})

	t.Run("a finished run means no action", func(t *testing.T) {
		// The done stop probes the pull request (issue #137): PR 7 is not
		// conflicted, so the run finishes with no action and no comment.
		server := prStateServer(t, "clean", reportComments(1))
		ghEnv(t, server)
		setArgs("7")
		writeResult(t, stopDone)
		t.Setenv("CONTINUE_COMMENT_PATH", filepath.Join(t.TempDir(), "never-written.md"))
		out, err := captureStdout(t, runContinuePlan)
		if err != nil {
			t.Fatal(err)
		}
		if out != "action=none\n" {
			t.Errorf("stdout = %q, want action=none", out)
		}
	})

	t.Run("a non-numeric PR argument is refused", func(t *testing.T) {
		setArgs("seven; rm -rf /")
		if _, err := captureStdout(t, runContinuePlan); err == nil {
			t.Error("runContinuePlan accepted a non-numeric PR argument")
		}
		setArgs("")
		if _, err := captureStdout(t, runContinuePlan); err == nil {
			t.Error("runContinuePlan accepted an empty PR argument")
		}
	})

	t.Run("an empty CONTINUE_COMMENT_PATH is an error", func(t *testing.T) {
		server := commentsServer(t, reportComments(1), http.StatusOK)
		ghEnv(t, server)
		setArgs("7")
		writeResult(t, stopDeadline)
		t.Setenv("CONTINUE_COMMENT_PATH", "")
		if _, err := captureStdout(t, runContinuePlan); err == nil {
			t.Error("runContinuePlan accepted an empty CONTINUE_COMMENT_PATH")
		}
	})
}

func TestDecideDispatchPR(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_RUN_ID", "42")
	labelled := []label{{Name: "codeaf"}}
	mkPR := func(number int, state string, labels []label, headRepo string) item {
		pr := item{Number: number, State: state, Labels: labels}
		pr.Head.Ref = "codeaf/issue-3"
		pr.Head.Repo = &struct {
			FullName string `json:"full_name"`
		}{FullName: headRepo}
		pr.Base.Ref = "main"
		return pr
	}
	openPR := mkPR(7, "open", labelled, "RichardoC/factory-repo")

	cases := []struct {
		name       string
		prInput    string
		issueInput string
		pr         item
		fetch      bool // whether lookupPR is called at all
		run        bool
		mode       string
	}{
		{"labelled open pr", "7", "", openPR, true, true, "pr"},
		{"unlabelled pr", "7", "", mkPR(7, "open", nil, "RichardoC/factory-repo"), true, false, ""},
		{"closed pr", "7", "", mkPR(7, "closed", labelled, "RichardoC/factory-repo"), true, false, ""},
		{"fork head", "7", "", mkPR(7, "open", labelled, "evil/factory-repo"), true, false, ""},
		{"non-numeric pr input", "seven", "", openPR, false, false, ""},
		{"empty pr input falls back to the issue path", "", "3", openPR, false, true, "issue"},
		{"no numeric input at all", "", "", openPR, false, false, ""},
	}
	for _, c := range cases {
		t.Run("dispatch pr "+c.name, func(t *testing.T) {
			called := false
			lookupPR := func(int) (item, error) {
				called = true
				return c.pr, nil
			}
			lookupIssue := func(int) (item, error) { return item{Number: 3, State: "open", Labels: labelled}, nil }
			ev := event{Inputs: map[string]string{}}
			if c.prInput != "" {
				ev.Inputs["pr"] = c.prInput
			}
			if c.issueInput != "" {
				ev.Inputs["issue"] = c.issueInput
			}
			got, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, lookupIssue)
			if err != nil {
				t.Fatal(err)
			}
			if called != c.fetch {
				t.Fatalf("lookupPR called = %t, want %t", called, c.fetch)
			}
			if got.Run != c.run || got.Mode != c.mode {
				t.Fatalf("Decide = %+v", got)
			}
			if c.run && c.mode == "pr" {
				if got.Reason != "workflow dispatch" || got.Number != 7 || got.Branch != openPR.Head.Ref || got.Base != openPR.Base.Ref {
					t.Fatalf("Decide = %+v, want the PR's number, head branch and base branch", got)
				}
			}
			if c.run && c.mode == "issue" && got.Branch != "codeaf/issue-3-42" {
				t.Fatalf("issue path changed: %+v", got)
			}
		})
	}

	t.Run("a failing PR lookup is an error", func(t *testing.T) {
		lookupPR := func(int) (item, error) { return item{}, fmt.Errorf("500") }
		lookupIssue := func(int) (item, error) { t.Fatal("unexpected issue lookup"); return item{}, nil }
		ev := event{Inputs: map[string]string{"pr": "7"}}
		if _, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, lookupIssue); err == nil {
			t.Fatal("a failing PR lookup must be returned as an error")
		}
	})

	t.Run("the issue path still dispatches as before", func(t *testing.T) {
		called := false
		lookupPR := func(int) (item, error) { t.Fatal("unexpected PR lookup"); return item{}, nil }
		lookupIssue := func(int) (item, error) {
			called = true
			return item{Number: 3, State: "open", Labels: labelled}, nil
		}
		ev := event{Inputs: map[string]string{"issue": "3"}}
		got, err := Decide("workflow_dispatch", ev, "codeaf", lookupPR, lookupIssue)
		if err != nil {
			t.Fatal(err)
		}
		if !called || !got.Run || got.Mode != "issue" || got.Number != 3 {
			t.Fatalf("Decide = %+v (lookupIssue called = %t)", got, called)
		}
	})
}

func TestLinkedIssue(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{"Closes #31", 31},
		{"Resolves: #7", 7},
		{"Fixes #3", 3},
		{"Fixed   #12", 12},
		{"resolve #2", 2},
		{"closed #9", 9},
		{"closes #5", 5},
		{"some text\n\nCloses #31\n", 31},
		{"Closes #31 and Fixes #2", 31},
		{"no reference here", 0},
		{"#31 without a keyword", 0},
		{"unclosed #5 is inside a word", 0},
		{"Closes #31abc", 0},
		{"Closes #99999999999999999999", 0},
		{"Closes #2147483648", 0},
		{"Closes #2147483647", 2147483647},
		{"Closes: #0", 0},
		{"Closes #", 0},
		{"preposethat closes #5", 5}, // mid-sentence: GitHub links this too
		{"fix #12", 12},              // GitHub's missing keyword
		{"resolved #12", 12},         // GitHub's missing keyword
		{"prefix #12", 0},            // "fix" inside a word must not match
	}
	for _, c := range cases {
		if got := linkedIssue(c.body); got != c.want {
			t.Errorf("linkedIssue(%q) = %d, want %d", c.body, got, c.want)
		}
	}
}

func TestBriefLinkedIssue(t *testing.T) {
	issue := `{"number":31,"title":"Continue early stops","body":"Publish the unfinished work as a draft PR.","state":"open"}`
	routes := prRoutes()
	routes["/repos/RichardoC/factory-repo/issues/31"] = issue
	routes["/repos/RichardoC/factory-repo/pulls/7"] = `{"number":7,"title":"The PR","body":"A body\n\nCloses #31","state":"open","head":{"ref":"codeaf-7"}}`
	brief := runBriefInto(t, routes, "pr", prEvent)
	section := briefSection(brief, "The issue this PR closes (#31)")
	if section == "" {
		t.Fatal("no issue section in the continuation brief")
	}
	for _, want := range []string{"Continue early stops", "Publish the unfinished work as a draft PR."} {
		if !strings.Contains(section, want) {
			t.Errorf("issue section missing %q: %q", want, section)
		}
	}

	// No close keyword in the PR body: no section, and no issue fetch.
	routes = prRoutes()
	brief = runBriefInto(t, routes, "pr", prEvent)
	if strings.Contains(brief, "The issue this PR closes") {
		t.Errorf("a PR that closes nothing must not have an issue section: %q", brief)
	}

	// The issue API failing degrades to a note; the brief is still written.
	routes = prRoutes()
	routes["/repos/RichardoC/factory-repo/pulls/7"] = `{"number":7,"title":"The PR","body":"A body\n\nCloses #31","state":"open","head":{"ref":"codeaf-7"}}`
	brief = runBriefInto(t, routes, "pr", prEvent)
	if !strings.Contains(brief, "(the issue this PR closes, #31, is unavailable:") {
		t.Errorf("no degradation note for a failing issue fetch: %q", brief)
	}
}

// ---------------------------------------------------------------- issue #137

// prStateServer serves the comments and the pull request of PR 7: comments
// carries n run reports, and the pull request's mergeable_state is state.
// status < 0 makes the pull request fetch fail with a server error.
func prStateServer(t *testing.T, state string, comments []issueComment) *httptest.Server {
	t.Helper()
	var b strings.Builder
	b.WriteString("[")
	for i, c := range comments {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"body":%q,"user":{"login":%q}}`, c.ID, c.Body, c.User.Login)
	}
	b.WriteString("]")
	commentBody := b.String()
	prBody := fmt.Sprintf(`{"number":7,"mergeable_state":%q}`, state)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/RichardoC/factory-repo/issues/7/comments":
			_, _ = io.WriteString(w, commentBody)
		case "/repos/RichardoC/factory-repo/pulls/7":
			_, _ = io.WriteString(w, prBody)
		default:
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// pullsServer serves the pull request of PR 7 with mergeable_state state,
// and fails the comments path with status: the mirror of commentsServer,
// for the error branch of the comments fetch.
func pullsServer(t *testing.T, state string, status int) *httptest.Server {
	t.Helper()
	prBody := fmt.Sprintf(`{"number":7,"mergeable_state":%q}`, state)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/RichardoC/factory-repo/issues/7/comments":
			http.Error(w, "comments unavailable", status)
		case "/repos/RichardoC/factory-repo/pulls/7":
			_, _ = io.WriteString(w, prBody)
		default:
			http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// stubSleep replaces the injectable continueSleep with a recorder, so the
// mergeable_state retries are observed without waiting. The returned slice
// holds one entry per call.
func stubSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	calls := []time.Duration{}
	old := continueSleep
	continueSleep = func(d time.Duration) { calls = append(calls, d) }
	t.Cleanup(func() { continueSleep = old })
	return &calls
}

func TestContinuePlanContinuesADirtyPR(t *testing.T) {
	t.Run("dirty under the cap continues with the fixed body", func(t *testing.T) {
		sleeps := stubSleep(t)
		server := prStateServer(t, "dirty", reportComments(1))
		ghEnv(t, server)
		action, body, used, err := continuePlan(newGitHub(), 7, stopDone)
		if err != nil {
			t.Fatal(err)
		}
		if action != "continue" {
			t.Fatalf("action = %q, want continue", action)
		}
		if want := "The branch conflicts with its base branch; a continuation brings the base in and resolves the conflicts\n"; body != want {
			t.Errorf("body = %q, want the exact fixed text %q", body, want)
		}
		if used != 1 {
			t.Errorf("used = %d, want 1 (issue #219)", used)
		}
		if len(*sleeps) != 0 {
			t.Errorf("a dirty PR must not sleep, got %v", *sleeps)
		}
	})

	t.Run("dirty with the cap reached hands over with the cap comment", func(t *testing.T) {
		sleeps := stubSleep(t)
		server := prStateServer(t, "dirty", reportComments(3))
		ghEnv(t, server)
		action, body, used, err := continuePlan(newGitHub(), 7, stopDone)
		if err != nil {
			t.Fatal(err)
		}
		if action != "cap" {
			t.Fatalf("action = %q, want cap", action)
		}
		if want := conflictCapBody(); body != want {
			t.Errorf("body = %q, want the fixed cap body %q", body, want)
		}
		if used != 3 {
			t.Errorf("used = %d, want 3 (issue #219)", used)
		}
		if len(*sleeps) != 0 {
			t.Errorf("unexpected sleeps: %v", *sleeps)
		}
	})

	t.Run("a state other than dirty does nothing", func(t *testing.T) {
		for _, state := range []string{"clean", "merged", "blocked", "unstable", ""} {
			sleeps := stubSleep(t)
			server := prStateServer(t, state, reportComments(1))
			ghEnv(t, server)
			action, body, _, err := continuePlan(newGitHub(), 7, stopDone)
			if err != nil || action != "" || body != "" {
				t.Errorf("mergeable_state %q: continuePlan = (%q, %q, %v), want no action", state, action, body, err)
			}
			if len(*sleeps) != 0 {
				t.Errorf("mergeable_state %q: unexpected sleeps: %v", state, *sleeps)
			}
		}
	})

	t.Run("a failing pull request fetch fails the plan", func(t *testing.T) {
		sleeps := stubSleep(t)
		// This server serves the comments and nothing else, so the pull
		// request fetch fails; commentsServer cannot stand in for it since
		// it serves a ready pull request too (the review plan reads the
		// draft flag, issue #237).
		var b strings.Builder
		b.WriteString("[")
		for i, c := range reportComments(1) {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%d,"body":%q}`, c.ID, c.Body)
		}
		b.WriteString("]")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/RichardoC/factory-repo/issues/7/comments" {
				http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, b.String())
		}))
		t.Cleanup(server.Close)
		ghEnv(t, server)
		action, body, _, err := continuePlan(newGitHub(), 7, stopDone)
		if err == nil {
			t.Fatalf("continuePlan = (%q, %q, nil), want the fetch error", action, body)
		}
		if action != "" || body != "" {
			t.Errorf("continuePlan = (%q, %q), want no action beside the error", action, body)
		}
		if len(*sleeps) != 0 {
			t.Errorf("unexpected sleeps: %v", *sleeps)
		}
	})

	t.Run("a failing comments fetch fails the plan", func(t *testing.T) {
		sleeps := stubSleep(t)
		server := pullsServer(t, "dirty", http.StatusInternalServerError) // 500s the comments
		ghEnv(t, server)
		action, body, _, err := continuePlan(newGitHub(), 7, stopDone)
		if err == nil {
			t.Fatalf("continuePlan = (%q, %q, nil), want the fetch error", action, body)
		}
		if action != "" || body != "" {
			t.Errorf("continuePlan = (%q, %q), want no action beside the error", action, body)
		}
		if len(*sleeps) != 0 {
			t.Errorf("unexpected sleeps: %v", *sleeps)
		}
	})

	t.Run("an early stop never reaches the mergeable_state path", func(t *testing.T) {
		sleeps := stubSleep(t)
		server := prStateServer(t, "dirty", reportComments(1))
		ghEnv(t, server)
		action, body, used, err := continuePlan(newGitHub(), 7, stopDeadline)
		if err != nil {
			t.Fatal(err)
		}
		if action != "continue" || body != continueBody(stopDeadline, 1) || used != 1 {
			t.Errorf("continuePlan(deadline) = (%q, %q, %d), want the early-stop continuation", action, body, used)
		}
		if len(*sleeps) != 0 {
			t.Errorf("unexpected sleeps: %v", *sleeps)
		}
	})
}

func TestContinuePlanWaitsForUnknownMergeable(t *testing.T) {
	t.Run("unknown is retried 3 times, 5 seconds apart, then does nothing", func(t *testing.T) {
		sleeps := stubSleep(t)
		server := prStateServer(t, "unknown", reportComments(1))
		ghEnv(t, server)
		action, body, _, err := continuePlan(newGitHub(), 7, stopDone)
		if err != nil || action != "" || body != "" {
			t.Errorf("continuePlan = (%q, %q, %v), want no action after the retries", action, body, err)
		}
		if len(*sleeps) != 3 {
			t.Fatalf("sleep calls = %d (%v), want 3 retries", len(*sleeps), *sleeps)
		}
		for i, d := range *sleeps {
			if d != 5*time.Second {
				t.Errorf("sleep %d = %v, want 5s", i, d)
			}
		}
	})

	t.Run("unknown that turns dirty on a retry continues", func(t *testing.T) {
		sleeps := stubSleep(t)
		states := []string{"unknown", "unknown", "dirty"}
		var b strings.Builder
		commentBody := "[]"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/RichardoC/factory-repo/issues/7/comments":
				_, _ = io.WriteString(w, commentBody)
			case "/repos/RichardoC/factory-repo/pulls/7":
				state := "dirty"
				if len(states) > 0 {
					state, states = states[0], states[1:]
				}
				fmt.Fprintf(&b, `{"number":7,"mergeable_state":%q}`, state)
				_, _ = io.WriteString(w, b.String())
				b.Reset()
			default:
				http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
			}
		}))
		_ = commentBody
		t.Cleanup(server.Close)
		ghEnv(t, server)
		action, body, _, err := continuePlan(newGitHub(), 7, stopDone)
		if err != nil {
			t.Fatal(err)
		}
		if action != "continue" || body != conflictBody {
			t.Errorf("continuePlan = (%q, %q), want a continuation with the fixed body", action, body)
		}
		if len(*sleeps) != 2 {
			t.Errorf("sleep calls = %v, want 2 before the dirty answer", *sleeps)
		}
	})

	t.Run("the probe never runs past its budget", func(t *testing.T) {
		sleeps := stubSleep(t)
		fetches := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/RichardoC/factory-repo/issues/7/comments":
				_, _ = io.WriteString(w, "[]")
			case "/repos/RichardoC/factory-repo/pulls/7":
				fetches++
				_, _ = io.WriteString(w, `{"number":7,"mergeable_state":"unknown"}`)
			default:
				http.Error(w, "not served: "+r.URL.Path, http.StatusNotFound)
			}
		}))
		t.Cleanup(server.Close)
		ghEnv(t, server)
		old := mergeableStateBudget
		mergeableStateBudget = -time.Second // already past: no retry may start
		t.Cleanup(func() { mergeableStateBudget = old })
		action, body, _, err := continuePlan(newGitHub(), 7, stopDone)
		if err != nil || action != "" || body != "" {
			t.Errorf("continuePlan = (%q, %q, %v), want no action past the budget", action, body, err)
		}
		if fetches != 1 {
			t.Errorf("/pulls fetches = %d, want 1 (no retry past the budget)", fetches)
		}
		if len(*sleeps) != 0 {
			t.Errorf("sleeps = %v, want none past the budget", *sleeps)
		}
	})

	t.Run("clean and merged answer at once with zero sleeps", func(t *testing.T) {
		for _, state := range []string{"clean", "merged"} {
			sleeps := stubSleep(t)
			server := prStateServer(t, state, reportComments(1))
			ghEnv(t, server)
			action, body, _, err := continuePlan(newGitHub(), 7, stopDone)
			if err != nil || action != "" || body != "" {
				t.Errorf("mergeable_state %q: continuePlan = (%q, %q, %v), want no action", state, action, body, err)
			}
			if len(*sleeps) != 0 {
				t.Errorf("mergeable_state %q: sleeps = %v, want none", state, *sleeps)
			}
		}
	})
}

func TestRunContinuePlanDirtyPR(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	server := prStateServer(t, "dirty", reportComments(1))
	ghEnv(t, server)
	os.Args = []string{"harness", "continue-plan", "7"}
	writeResult(t, stopDone)
	comment := filepath.Join(t.TempDir(), "continue-comment.md")
	t.Setenv("CONTINUE_COMMENT_PATH", comment)
	out, err := captureStdout(t, runContinuePlan)
	if err != nil {
		t.Fatal(err)
	}
	if out != "action=continue\n" {
		t.Errorf("stdout = %q, want action=continue", out)
	}
	data, err := os.ReadFile(comment)
	if err != nil {
		t.Fatalf("comment not written: %v", err)
	}
	if want := conflictBody; string(data) != want {
		t.Errorf("comment = %q, want %q", data, want)
	}
}

// ---------------------------------------------------------------- issue #219

// The stub PR carries the opener's report and continuation 1's report: two
// report comments, both already posted, while the stopping run's own report
// is not posted yet. The cap of 2 is spent, so the plan must hand over with
// a cap comment instead of dispatching a third continuation.
func TestContinuePlanCountsOwnReport(t *testing.T) {
	server := commentsServer(t, reportComments(2), http.StatusOK)
	ghEnv(t, server)
	action, body, used, err := continuePlan(newGitHub(), 7, stopDeadline)
	if err != nil {
		t.Fatal(err)
	}
	if action != "cap" {
		t.Fatalf("action = %q, want cap (2 reports, cap 2)", action)
	}
	if used != 2 {
		t.Errorf("used = %d, want 2", used)
	}
	if want := capBody(stopDeadline); body != want {
		t.Errorf("body = %q, want the fixed cap body %q", body, want)
	}
}

// The 16:11Z run of PR #209 failed at publish: its report says so, and its
// patch never landed, so its report must not spend a continuation slot. The
// stub PR carries the opener's report plus one publishing-failed report;
// the plan continues and labels the next run "Continuation 2 of 2".
func TestContinuePlanIgnoresFailedPublish(t *testing.T) {
	comments := append(reportComments(1), issueComment{
		ID:   500,
		User: user{Login: planReportBotLogin},
		Body: failedPublishHeading + "\n| stop / ok | done / `true` |\n",
	})
	server := commentsServer(t, comments, http.StatusOK)
	ghEnv(t, server)
	action, body, used, err := continuePlan(newGitHub(), 7, stopDeadline)
	if err != nil {
		t.Fatal(err)
	}
	if action != "continue" {
		t.Fatalf("action = %q, want continue", action)
	}
	if used != 1 {
		t.Errorf("used = %d, want 1 (the publishing-failed report is not counted)", used)
	}
	if !strings.Contains(body, "Continuation 2 of 2") {
		t.Errorf("body does not label the continuation 2 of 2: %q", body)
	}
}

// A report headed "run job failed" is the same stranded state as a failed
// publish (queue.go's strandedByFailedRun treats both alike): the run job
// died, no patch landed, so the report must not spend a continuation slot.
// The stub PR carries the opener's report plus one such report; the plan
// continues and labels the next run "Continuation 2 of 2".
func TestContinuePlanIgnoresFailedRunJob(t *testing.T) {
	comments := append(reportComments(1), issueComment{
		ID:   501,
		User: user{Login: planReportBotLogin},
		Body: runJobFailedHeading + "\n| stop / ok | done / `true` |\n",
	})
	server := commentsServer(t, comments, http.StatusOK)
	ghEnv(t, server)
	action, body, used, err := continuePlan(newGitHub(), 7, stopDeadline)
	if err != nil {
		t.Fatal(err)
	}
	if action != "continue" {
		t.Fatalf("action = %q, want continue", action)
	}
	if used != 1 {
		t.Errorf("used = %d, want 1 (the run-job-failed report is not counted)", used)
	}
	if !strings.Contains(body, "Continuation 2 of 2") {
		t.Errorf("body does not label the continuation 2 of 2: %q", body)
	}
}

// A run whose patch never landed leaves the cap unspent, whatever stopped it:
// two failed reports plus the opener's are not two continuations, so the
// third run still continues rather than hitting the cap.
func TestContinuePlanFailedReportsDoNotReachTheCap(t *testing.T) {
	comments := append(reportComments(1),
		issueComment{ID: 502, User: user{Login: planReportBotLogin}, Body: failedPublishHeading},
		issueComment{ID: 503, User: user{Login: planReportBotLogin}, Body: runJobFailedHeading},
	)
	server := commentsServer(t, comments, http.StatusOK)
	ghEnv(t, server)
	action, _, used, err := continuePlan(newGitHub(), 7, stopDeadline)
	if err != nil {
		t.Fatal(err)
	}
	if action != "continue" || used != 1 {
		t.Fatalf("action = %q, used = %d, want continue/1: failed reports must not spend the cap", action, used)
	}
}

// A comment by another login that carries the marker and the heading is not
// a report, in either direction: added to real reports it must not push the
// plan past the cap, and on its own it must not reach the cap either.
func TestContinuePlanIgnoresForgedReports(t *testing.T) {
	forge := func(login string) issueComment {
		return issueComment{
			ID:   600,
			User: user{Login: login},
			Body: reportMarker + "\n### CodeAF run — done\n\n| stop / ok | done / `true` |\n",
		}
	}
	t.Run("forged comments never push the plan to the cap", func(t *testing.T) {
		// One real report plus 10 forged ones: the plan still continues,
		// and labels the next continuation from the real count only.
		var comments []issueComment
		for i := 0; i < 10; i++ {
			comments = append(comments, forge("someone"))
		}
		comments = append(comments, reportComments(1)...)
		server := commentsServer(t, comments, http.StatusOK)
		ghEnv(t, server)
		action, body, used, err := continuePlan(newGitHub(), 7, stopDeadline)
		if err != nil {
			t.Fatal(err)
		}
		if action != "continue" || used != 1 {
			t.Fatalf("action = %q, used = %d, want continue/1 despite 10 forged reports", action, used)
		}
		if !strings.Contains(body, "Continuation 2 of 2") {
			t.Errorf("body does not label the continuation 2 of 2: %q", body)
		}
	})

	t.Run("forged comments never suppress the cap either", func(t *testing.T) {
		// The cap is spent by real reports; a forged failed-publish heading
		// by an outsider must not unspend it (only a bot's report is
		// excluded, and only for publishing failed).
		comments := append(reportComments(2), issueComment{
			ID:   700,
			User: user{Login: "someone"},
			Body: failedPublishHeading,
		})
		server := commentsServer(t, comments, http.StatusOK)
		ghEnv(t, server)
		action, _, used, err := continuePlan(newGitHub(), 7, stopDeadline)
		if err != nil {
			t.Fatal(err)
		}
		if action != "cap" || used != 2 {
			t.Fatalf("action = %q, used = %d, want cap/2 despite the forged failed-publish comment", action, used)
		}
	})

	t.Run("only the workflow bot identity counts", func(t *testing.T) {
		// A lookalike login is not the bot: its reports count for nothing.
		comments := []issueComment{forge("github-actions[bot] "), forge("GitHub-actions[bot]"), forge("github-actions[bot2]")}
		server := commentsServer(t, comments, http.StatusOK)
		ghEnv(t, server)
		action, _, used, err := continuePlan(newGitHub(), 7, stopDeadline)
		if err != nil {
			t.Fatal(err)
		}
		if action != "continue" || used != 0 {
			t.Fatalf("action = %q, used = %d, want continue/0 for lookalike logins", action, used)
		}
	})
}
