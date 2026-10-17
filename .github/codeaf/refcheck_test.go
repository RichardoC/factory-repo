package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	refTestPin  = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	refTestHead = "2222222222222222222222222222222222222222"
	refTestOld  = "3333333333333333333333333333333333333333"
)

// refCommitJSON builds one commit of the compare fixture.
func refCommitJSON(sha, message string) string {
	return fmt.Sprintf(`{"sha":%q,"commit":{"message":%q}}`, sha, message)
}

// refCompareJSON builds a compare response fixture inline.
func refCompareJSON(status string, ahead int, commits ...string) string {
	return fmt.Sprintf(`{"status":%q,"ahead_by":%d,"commits":[%s]}`,
		status, ahead, strings.Join(commits, ","))
}

// refIssueJSON builds one issue fixture inline.
func refIssueJSON(n int, body string, labels ...string) string {
	var ls []string
	for _, l := range labels {
		ls = append(ls, fmt.Sprintf(`{"name":%q}`, l))
	}
	return fmt.Sprintf(`{"number":%d,"title":"old title","body":%q,"labels":[%s]}`,
		n, body, strings.Join(ls, ","))
}

// refParseCompare and refParseIssues decode fixtures the way runRefCheck
// reads them.
func refParseCompare(t *testing.T, s string) compare {
	t.Helper()
	var c compare
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return c
}

func refParseIssues(t *testing.T, s string) []item {
	t.Helper()
	var its []item
	if err := json.Unmarshal([]byte(s), &its); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return its
}

func TestRefCheckDecision(t *testing.T) {
	moved := refCompareJSON("ahead", 2,
		refCommitJSON(refTestOld, "Old commit\n\nWith a body."),
		refCommitJSON(refTestHead, "New commit"),
	)
	atHeadIssue := refIssueJSON(7, "Upstream head: "+refTestHead+"\nPin: "+refTestPin,
		"codeaf-ref-update", "codeaf-queued")
	atOldIssue := refIssueJSON(7, "Upstream head: "+refTestOld+"\nPin: "+refTestPin,
		"codeaf-ref-update")
	queued := []string{"codeaf-ref-update", "codeaf-queued"}

	cases := []struct {
		name       string
		pin        string
		cmp        string
		issues     string
		wantAction string
		wantHead   string
		wantIssue  int
		wantTitle  string
		wantLabels []string
		wantReason []string // substrings the one-line reason must carry
	}{
		{
			name: "moved, no open issue: create",
			pin:  refTestPin, cmp: moved, issues: "[]",
			wantAction: "create", wantHead: refTestHead, wantTitle: refIssueTitle,
			wantLabels: queued, wantReason: []string{"2 commits past the pin"},
		},
		{
			name: "moved, issue already at head: none",
			pin:  refTestPin, cmp: moved, issues: "[" + atHeadIssue + "]",
			wantAction: "none", wantHead: refTestHead, wantReason: []string{"#7"},
		},
		{
			name: "moved, issue at an older head: update",
			pin:  refTestPin, cmp: moved, issues: "[" + atOldIssue + "]",
			wantAction: "update", wantHead: refTestHead, wantIssue: 7,
			wantTitle: refIssueTitle, wantLabels: queued,
		},
		{
			name: "marker line matched among other lines: none",
			pin:  refTestPin, cmp: moved,
			issues: "[" + refIssueJSON(7, "Preamble\n\nUpstream head: "+refTestHead+"\n\nTrailer",
				"codeaf-ref-update") + "]",
			wantAction: "none", wantHead: refTestHead,
		},
		{
			name: "an issue without the ref-update label is ignored: create",
			pin:  refTestPin, cmp: moved,
			issues:     "[" + refIssueJSON(4, "Upstream head: "+refTestOld, "codeaf") + "]",
			wantAction: "create", wantHead: refTestHead, wantTitle: refIssueTitle,
			wantLabels: queued,
		},
		{
			name: "first of several ref-update issues wins: update #5",
			pin:  refTestPin, cmp: moved,
			issues: "[" + refIssueJSON(5, "Upstream head: "+refTestOld, "codeaf-ref-update") + "," +
				refIssueJSON(6, "Upstream head: "+refTestOld, "codeaf-ref-update") + "]",
			wantAction: "update", wantHead: refTestHead, wantIssue: 5,
			wantTitle: refIssueTitle, wantLabels: queued,
		},
		{
			name:       "diverged counts as moved and the reason notes it",
			pin:        refTestPin,
			cmp:        refCompareJSON("diverged", 3, refCommitJSON(refTestHead, "New commit")),
			issues:     "[]",
			wantAction: "create", wantHead: refTestHead, wantTitle: refIssueTitle,
			wantLabels: queued, wantReason: []string{"diverged", "3 commits"},
		},
		{
			name: "identical, no open issue: none",
			pin:  refTestPin, cmp: `{"status":"identical","ahead_by":0}`, issues: "[]",
			wantAction: "none", wantHead: refTestPin,
		},
		{
			name: "identical, open issue: close",
			pin:  refTestPin, cmp: `{"status":"identical","ahead_by":0}`,
			issues:     "[" + refIssueJSON(9, "Upstream head: "+refTestOld, "codeaf-ref-update") + "]",
			wantAction: "close", wantHead: refTestPin, wantIssue: 9,
			wantLabels: []string{},
		},
		{
			name:       "head == pin, open issue: close",
			pin:        refTestPin,
			cmp:        refCompareJSON("ahead", 1, refCommitJSON(refTestPin, "At the pin")),
			issues:     "[" + refIssueJSON(9, "Upstream head: "+refTestOld, "codeaf-ref-update") + "]",
			wantAction: "close", wantHead: refTestPin, wantIssue: 9,
			wantLabels: []string{},
		},
		{
			name:       "head == pin, no open issue: none",
			pin:        refTestPin,
			cmp:        refCompareJSON("ahead", 1, refCommitJSON(refTestPin, "At the pin")),
			issues:     "[]",
			wantAction: "none", wantHead: refTestPin,
		},
		{
			name:       "ahead_by == 0 counts as identical even with commits listed",
			pin:        refTestPin,
			cmp:        refCompareJSON("ahead", 0, refCommitJSON(refTestHead, "New commit")),
			issues:     "[" + refIssueJSON(9, "Upstream head: "+refTestOld, "codeaf-ref-update") + "]",
			wantAction: "close", wantHead: refTestHead, wantIssue: 9,
			wantLabels: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := RefCheck(c.pin, refParseCompare(t, c.cmp), refParseIssues(t, c.issues))
			if err != nil {
				t.Fatalf("RefCheck: %v", err)
			}
			if d.Action != c.wantAction {
				t.Fatalf("action = %q, want %q", d.Action, c.wantAction)
			}
			if d.Pin != c.pin {
				t.Fatalf("pin = %q, want %q", d.Pin, c.pin)
			}
			if d.Head != c.wantHead {
				t.Fatalf("head = %q, want %q", d.Head, c.wantHead)
			}
			if d.IssueNumber != c.wantIssue {
				t.Fatalf("issue_number = %d, want %d", d.IssueNumber, c.wantIssue)
			}
			if d.Title != c.wantTitle {
				t.Fatalf("title = %q, want %q", d.Title, c.wantTitle)
			}
			if !slices.Equal(d.Labels, c.wantLabels) {
				t.Fatalf("labels = %v, want %v", d.Labels, c.wantLabels)
			}
			for _, s := range c.wantReason {
				if !strings.Contains(d.Reason, s) {
					t.Fatalf("reason %q does not mention %q", d.Reason, s)
				}
			}
			if strings.ContainsAny(d.Reason, "\n\r") {
				t.Fatalf("reason is not one line: %q", d.Reason)
			}
			switch d.Action {
			case "create", "update":
				lines := strings.SplitN(d.Body, "\n", 3)
				if lines[0] != "Upstream head: "+d.Head {
					t.Fatalf("first body line = %q, want the marker for %s", lines[0], d.Head)
				}
				if lines[1] != "Pin: "+c.pin {
					t.Fatalf("second body line = %q, want the pin", lines[1])
				}
				for _, want := range []string{
					"## Commits since the pin",
					"1. Read the upstream commits since the pin in " + upstreamRepo,
					".github/workflows/codeaf.yml",
					"3. Bump " + refPath,
					"4. Note that changes to .github/workflows/",
				} {
					if !strings.Contains(d.Body, want) {
						t.Fatalf("body is missing %q:\n%s", want, d.Body)
					}
				}
			default:
				if d.Body != "" {
					t.Fatalf("%s body = %q, want empty", d.Action, d.Body)
				}
			}
		})
	}
}

func TestRefCheckBodyCommitList(t *testing.T) {
	var commits []string
	for i := range 25 {
		msg := fmt.Sprintf("Commit number %02d", i)
		if i == 0 {
			// The first line is taken alone, trimmed, and capped at
			// refCommitMaxLen characters; the body must stay out.
			msg = " \t" + strings.Repeat("x", 150) + "\n\nBody that must not appear."
		}
		sha := fmt.Sprintf("abcdef60%02d%s", i, strings.Repeat("0", 30))
		commits = append(commits, refCommitJSON(sha, msg))
	}
	d, err := RefCheck(refTestPin, refParseCompare(t, refCompareJSON("ahead", 25, commits...)), nil)
	if err != nil {
		t.Fatalf("RefCheck: %v", err)
	}
	if d.Head != "abcdef6024"+strings.Repeat("0", 30) {
		t.Fatalf("head = %q, want the sha of the last commit", d.Head)
	}
	body := d.Body
	if !strings.Contains(body, "Commit number 19") {
		t.Fatal("the 20th commit is missing from the body")
	}
	if strings.Contains(body, "Commit number 20") {
		t.Fatal("more than 20 commits were listed")
	}
	if !strings.Contains(body, "- ... and 5 more") {
		t.Fatal("the truncation line is missing")
	}
	if !strings.Contains(body, "- `abcdef6` "+strings.Repeat("x", 97)+"...") {
		t.Fatal("the capped first line is missing")
	}
	if strings.Contains(body, "Body that must not appear") {
		t.Fatal("the commit body leaked into the issue")
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "- `") {
			continue
		}
		if got := utf8.RuneCountInString(strings.TrimPrefix(line, "- `abcdef6` ")); got > refCommitMaxLen {
			t.Fatalf("commit summary is %d characters: %q", got, line)
		}
	}
}

// runRefCheckStdout runs runRefCheck against the three files and returns
// what it printed on stdout, with its log output discarded.
func runRefCheckStdout(t *testing.T, pinPath, comparePath, issuesPath string) (string, error) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	oldLog := log.Writer()
	log.SetOutput(devnull)
	defer log.SetOutput(oldLog)

	oldArgs := os.Args
	os.Args = []string{"harness", "ref-check", pinPath, comparePath, issuesPath}
	defer func() { os.Args = oldArgs }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	out := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		out <- string(data)
	}()
	err = runRefCheck()
	os.Stdout = oldStdout
	w.Close()
	return <-out, err
}

func TestRunRefCheck(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	pinFile := write("pin", "  "+refTestPin+"\n") // surrounding whitespace is trimmed
	movedCompare := write("moved.json",
		refCompareJSON("ahead", 1, refCommitJSON(refTestHead, "New commit")))
	identicalCompare := write("identical.json", `{"status":"identical","ahead_by":0}`)
	closeIssues := write("close-issues.json",
		"["+refIssueJSON(9, "Upstream head: "+refTestOld, "codeaf-ref-update")+"]")
	emptyIssues := write("empty-issues.json", "[]")
	badCompare := write("bad-compare.json", "{not json")
	badIssues := write("bad-issues.json", `{"number":9}`)
	queued := []string{"codeaf-ref-update", "codeaf-queued"}

	cases := []struct {
		name       string
		pin        string
		cmp        string
		issues     string
		wantErr    string // substring of the expected error; "" on success
		wantAction string
		wantHead   string
		wantIssue  int
		wantLabels []string
	}{
		{
			name: "create", pin: pinFile, cmp: movedCompare, issues: emptyIssues,
			wantAction: "create", wantHead: refTestHead, wantLabels: queued,
		},
		{
			name: "close", pin: pinFile, cmp: identicalCompare, issues: closeIssues,
			wantAction: "close", wantHead: refTestPin, wantIssue: 9, wantLabels: []string{},
		},
		{
			name: "identical with no issue: none", pin: pinFile, cmp: identicalCompare, issues: emptyIssues,
			wantAction: "none", wantHead: refTestPin,
		},
		{
			name: "missing pin file", pin: filepath.Join(dir, "nope"), cmp: movedCompare, issues: emptyIssues,
			wantErr: "no such file",
		},
		{
			name: "short pin", pin: write("short-pin", refTestPin[:39]), cmp: movedCompare, issues: emptyIssues,
			wantErr: "40-character",
		},
		{
			name: "uppercase pin", pin: write("upper-pin", strings.ToUpper(refTestPin)),
			cmp: movedCompare, issues: emptyIssues,
			wantErr: "40-character",
		},
		{
			name: "pin with a space inside",
			pin:  write("space-pin", refTestPin[:20]+" "+refTestPin[20:]),
			cmp:  movedCompare, issues: emptyIssues,
			wantErr: "40-character",
		},
		{
			name: "malformed compare", pin: pinFile, cmp: badCompare, issues: emptyIssues,
			wantErr: "bad-compare.json",
		},
		{
			name: "malformed issues", pin: pinFile, cmp: movedCompare, issues: badIssues,
			wantErr: "bad-issues.json",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := runRefCheckStdout(t, c.pin, c.cmp, c.issues)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
				}
				if out != "" {
					t.Fatalf("printed %q on stdout; an error must print nothing", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("runRefCheck: %v", err)
			}
			if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
				t.Fatalf("stdout = %q, want exactly one JSON line", out)
			}
			var d decision
			if err := json.Unmarshal([]byte(out), &d); err != nil {
				t.Fatalf("output %q is not one JSON object: %v", out, err)
			}
			if d.Action != c.wantAction || d.IssueNumber != c.wantIssue || d.Head != c.wantHead {
				t.Fatalf("decision = action %q, issue %d, head %q; want %q, %d, %q",
					d.Action, d.IssueNumber, d.Head, c.wantAction, c.wantIssue, c.wantHead)
			}
			if d.Pin != refTestPin {
				t.Fatalf("pin = %q, want %q", d.Pin, refTestPin)
			}
			if !slices.Equal(d.Labels, c.wantLabels) {
				t.Fatalf("labels = %v, want %v", d.Labels, c.wantLabels)
			}
			if d.Action == "create" && (d.Title == "" || d.Body == "") {
				t.Fatalf("create decision has empty title or body: %q, %q", d.Title, d.Body)
			}
		})
	}

	// The wrong argument count is a usage error, before any file is read.
	oldArgs := os.Args
	os.Args = []string{"harness", "ref-check", pinFile}
	defer func() { os.Args = oldArgs }()
	if err := runRefCheck(); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("runRefCheck with two paths = %v, want a usage error", err)
	}
}
