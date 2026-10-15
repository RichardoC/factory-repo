package main

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testModels(t *testing.T) []Model {
	t.Helper()
	models, err := loadModels("models.json")
	if err != nil {
		t.Fatal(err)
	}
	return models
}

func mustResolve(t *testing.T, models []Model, id string) Model {
	t.Helper()
	m, ok := Resolve(models, id)
	if !ok {
		t.Fatalf("Resolve(%q) found nothing", id)
	}
	return m
}

func TestCostOf(t *testing.T) {
	flash := mustResolve(t, testModels(t), "zai-org/glm-5.3-flash")
	usage := Usage{PromptTokens: 1000, CompletionTokens: 200}
	usage.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 400}
	// 600 uncached × $0.15 + 400 cached × $0.04 + 200 out × $0.50, per million.
	if got, want := CostOf(flash, usage), 0.000206; math.Abs(got-want) > 1e-12 {
		t.Fatalf("CostOf = %v, want %v", got, want)
	}
}

func TestResolve(t *testing.T) {
	models := testModels(t)
	for _, id := range []string{"zai-org/glm-5.3-flash", "~zai-org/glm-5.3-flash:high", "hf:zai-org/GLM-5.3-Flash", "ZAI-ORG/GLM-5.3-FLASH"} {
		if m := mustResolve(t, models, id); m.Upstream != "hf:zai-org/GLM-5.3-Flash" {
			t.Errorf("Resolve(%q) = %s", id, m.Upstream)
		}
	}
	for _, id := range []string{"moonshotai/kimi-k3", "hf:moonshotai/Kimi-K3", ""} {
		if _, ok := Resolve(models, id); ok {
			t.Errorf("Resolve(%q) should refuse a model outside models.json", id)
		}
	}
}

func TestEffortFor(t *testing.T) {
	models := testModels(t)
	flash := mustResolve(t, models, "zai-org/glm-5.3-flash")              // low, high, max
	deepseek := mustResolve(t, models, "deepseek-ai/deepseek-v4.1-flash") // none … max
	off := false
	cases := []struct {
		m    Model
		in   reasoningRequest
		want string
	}{
		{flash, reasoningRequest{Effort: "high"}, "high"},
		{flash, reasoningRequest{Effort: "medium"}, "high"},
		{flash, reasoningRequest{Effort: "low"}, "high"},
		{flash, reasoningRequest{Effort: "minimal"}, "high"},
		{flash, reasoningRequest{Enabled: &off}, "high"},
		{deepseek, reasoningRequest{Enabled: &off}, "none"},
		{flash, reasoningRequest{}, ""},
		{flash, reasoningRequest{Effort: "weird"}, "high"},
	}
	for _, c := range cases {
		if got := EffortFor(c.m, c.in); got != c.want {
			t.Errorf("EffortFor(%s, %+v) = %q, want %q", c.m.ID, c.in, got, c.want)
		}
	}
}

func TestRewriteRequest(t *testing.T) {
	flash := mustResolve(t, testModels(t), "zai-org/glm-5.3-flash")
	var body map[string]json.RawMessage
	raw := `{"model":"zai-org/glm-5.3-flash","stream":true,"reasoning":{"effort":"medium"},"provider":{"order":["x"]},"tools":[{"type":"function"}]}`
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	stream, err := RewriteRequest(body, flash)
	if err != nil || !stream {
		t.Fatalf("stream=%v err=%v", stream, err)
	}
	for key, want := range map[string]string{
		"model":            `"hf:zai-org/GLM-5.3-Flash"`,
		"reasoning_effort": `"high"`,
		"stream_options":   `{"include_usage":true}`,
		"tools":            `[{"type":"function"}]`,
	} {
		if got := string(body[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	for _, key := range []string{"reasoning", "provider"} {
		if _, ok := body[key]; ok {
			t.Errorf("%s should have been removed", key)
		}
	}
}

func TestCatalogIsParseableByCodeAF(t *testing.T) {
	rows := catalog(testModels(t))["data"]
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	// CodeAF's parsePrice is strconv.ParseFloat on these strings: no "$".
	if got := rows[0].Pricing.Completion; got != "0.000000500000" {
		t.Errorf("completion price = %q", got)
	}
}

// TestProxyEndToEnd runs the proxy against a fake upstream: prices are added
// to both JSON and streamed responses, the ledger records them, and the caps
// and the model allowlist are enforced.
func TestProxyEndToEnd(t *testing.T) {
	var seen []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("upstream got Authorization %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body)
		usage := `{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":400}}`
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}],"model":"x"}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"choices":[],"usage":`+usage+`}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"model":"x","usage":`+usage+`}`)
	}))
	defer upstream.Close()

	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	p := &proxy{upstream: upstream.URL, apiKey: "secret", ledger: ledger, runCap: 0.0005, dayCap: 20,
		models: testModels(t), client: upstream.Client()}
	server := httptest.NewServer(p)
	defer server.Close()

	post := func(body string) (*http.Response, string) {
		t.Helper()
		response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return response, string(data)
	}

	response, body := post(`{"model":"zai-org/glm-5.3-flash","messages":[]}`)
	if response.StatusCode != 200 || !strings.Contains(body, `"cost":0.000206`) || !strings.Contains(body, `"model":"zai-org/glm-5.3-flash"`) {
		t.Fatalf("JSON call: %d %s", response.StatusCode, body)
	}

	response, body = post(`{"model":"zai-org/glm-5.3","stream":true,"messages":[]}`)
	if response.StatusCode != 200 || !strings.Contains(body, `"cost":0.001824`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream call: %d %s", response.StatusCode, body)
	}
	if seen[1]["model"] != "hf:zai-org/GLM-5.3" {
		t.Errorf("upstream saw model %v", seen[1]["model"])
	}

	// $0.00203 spent > the $0.0005 run cap: the next call is refused.
	if response, body = post(`{"model":"zai-org/glm-5.3-flash","messages":[]}`); response.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("over cap: %d %s", response.StatusCode, body)
	}
	if response, body = post(`{"model":"moonshotai/kimi-k3","messages":[]}`); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("disallowed model: %d %s", response.StatusCode, body)
	}

	lines, err := readLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || math.Abs(ledgerTotal(lines)-0.00203) > 1e-12 || !lines[1].Stream || !lines[1].UsageSeen {
		t.Fatalf("ledger = %+v", lines)
	}
	models, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer models.Body.Close()
	if first, _ := bufio.NewReader(models.Body).ReadString('}'); !strings.Contains(first, "zai-org/glm-5.3-flash") {
		t.Errorf("catalog starts %q", first)
	}
}

func TestDecide(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	labelled := []label{{Name: "codeaf"}}
	owner := &comment{Body: "/codeaf go", AuthorAssociation: "OWNER", User: user{Login: "RichardoC", Type: "User"}}
	prMarker := json.RawMessage(`{"url":"x"}`)
	openPR := item{Number: 7, State: "open", Labels: labelled}
	openPR.Head.Ref = "codeaf/issue-3"
	openPR.Head.Repo = &struct {
		FullName string `json:"full_name"`
	}{FullName: "RichardoC/factory-repo"}
	openPR.Base.Ref = "main"
	forkPR := openPR
	forkPR.Head.Repo = &struct {
		FullName string `json:"full_name"`
	}{FullName: "evil/factory-repo"}
	lookup := func(pr item) func(int) (item, error) { return func(int) (item, error) { return pr, nil } }
	noLookup := func(int) (item, error) { t.Fatal("unexpected PR lookup"); return item{}, nil }
	base := event{}
	base.Repository.DefaultBranch = "main"

	with := func(f func(*event)) event { e := base; f(&e); return e }
	cases := []struct {
		name   string
		event  string
		ev     event
		lookup func(int) (item, error)
		run    bool
		mode   string
		branch string
	}{
		{"issue labelled", "issues", with(func(e *event) { e.Label = &label{Name: "codeaf"}; e.Issue = &item{Number: 3, Labels: labelled} }), noLookup, true, "issue", "codeaf/issue-3"},
		{"other label", "issues", with(func(e *event) { e.Label = &label{Name: "bug"}; e.Issue = &item{Number: 3} }), noLookup, false, "", ""},
		{"label on a PR", "issues", with(func(e *event) {
			e.Label = &label{Name: "codeaf"}
			e.Issue = &item{Number: 3, PullRequest: prMarker}
		}), noLookup, false, "", ""},
		{"untrusted commenter", "issue_comment", with(func(e *event) {
			e.Issue = &item{Number: 3, Labels: labelled}
			e.Comment = &comment{Body: "/codeaf go", AuthorAssociation: "NONE", User: user{Login: "x", Type: "User"}}
		}), noLookup, false, "", ""},
		{"unlabelled issue", "issue_comment", with(func(e *event) { e.Issue = &item{Number: 3}; e.Comment = owner }), noLookup, false, "", ""},
		{"not a command", "issue_comment", with(func(e *event) {
			e.Issue = &item{Number: 3, Labels: labelled}
			e.Comment = &comment{Body: "looks good", AuthorAssociation: "OWNER", User: user{Type: "User"}}
		}), noLookup, false, "", ""},
		{"command on issue", "issue_comment", with(func(e *event) { e.Issue = &item{Number: 3, Labels: labelled}; e.Comment = owner }), noLookup, true, "issue", "codeaf/issue-3"},
		{"command on PR", "issue_comment", with(func(e *event) {
			e.Issue = &item{Number: 7, Labels: labelled, PullRequest: prMarker}
			e.Comment = owner
		}), lookup(openPR), true, "pr", "codeaf/issue-3"},
		{"command on fork PR", "issue_comment", with(func(e *event) {
			e.Issue = &item{Number: 7, Labels: labelled, PullRequest: prMarker}
			e.Comment = owner
		}), lookup(forkPR), false, "", ""},
		{"changes requested", "pull_request_review", with(func(e *event) {
			e.PullRequest = &openPR
			e.Review = &comment{Body: "fix the test", State: "changes_requested", AuthorAssociation: "OWNER", User: user{Type: "User"}}
		}), lookup(openPR), true, "pr", "codeaf/issue-3"},
		{"plain approval", "pull_request_review", with(func(e *event) {
			e.PullRequest = &openPR
			e.Review = &comment{Body: "nice", State: "approved", AuthorAssociation: "OWNER", User: user{Type: "User"}}
		}), noLookup, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Decide(c.event, c.ev, "codeaf", c.lookup)
			if err != nil {
				t.Fatal(err)
			}
			if got.Run != c.run || got.Mode != c.mode || got.Branch != c.branch {
				t.Fatalf("Decide = %+v", got)
			}
		})
	}
}

func TestReport(t *testing.T) {
	ok := true
	result := Result{Stop: "done", OK: &ok, Model: "zai-org/glm-5.3-flash", SpendUSD: 0.0008, Answer: "Did it."}
	lines := []LedgerLine{
		{Model: "zai-org/glm-5.3-flash", PromptTokens: 1000, CachedTokens: 400, CompletionTokens: 200, CostUSD: 0.000206, UsageSeen: true},
		{Model: "zai-org/glm-5.3", PromptTokens: 1000, CompletionTokens: 200, CostUSD: 0.00228, UsageSeen: true},
		{Model: "zai-org/glm-5.3", Stream: true},
	}
	env := map[string]string{"DAY_SPENT_BEFORE": "1.2", "RUN_CAP": "5", "DAY_CAP": "20", "CHANGED": "3",
		"GITHUB_REPOSITORY": "RichardoC/factory-repo", "GITHUB_RUN_ID": "42"}
	report := Report(result, true, lines, testModels(t), func(k string) string { return env[k] })
	for _, want := range []string{
		reportMarker,
		"| stop / ok | `done` / `true` |",
		"**$0.0025** this run · $1.2025 today (cap $5/run, $20/day)",
		"| `zai-org/glm-5.3` | 1.4 · 0.26 · 4.4 | 2 | 1000 | 0 | 200 | $0.0023 (+1 unpriced) |",
		"| `zai-org/glm-5.3-flash` | 0.15 · 0.04 · 0.5 | 1 | 1000 | 400 | 200 | $0.0002 |",
		"Did it.",
		"https://github.com/RichardoC/factory-repo/actions/runs/42",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	// The dearer model sorts first.
	if strings.Index(report, "`zai-org/glm-5.3` |") > strings.Index(report, "`zai-org/glm-5.3-flash` |") {
		t.Error("rows not sorted by cost")
	}
}

func TestMain(m *testing.M) {
	// Tests read models.json next to this file.
	os.Setenv("HARNESS_DIR", ".")
	os.Exit(m.Run())
}
