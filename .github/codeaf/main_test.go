package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func testModels(t *testing.T) []Model {
	t.Helper()
	cfg, err := loadConfig("models.json")
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Models
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
	flash := mustResolve(t, models, "zai-org/glm-5.3-flash")              // high, max
	glm := mustResolve(t, models, "zai-org/glm-5.3")                      // medium, high, max
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
		{glm, reasoningRequest{Effort: "high"}, "high"},
		{glm, reasoningRequest{Effort: "medium"}, "medium"},
		{glm, reasoningRequest{Effort: "low"}, "medium"},
		{glm, reasoningRequest{Effort: "minimal"}, "medium"},
		{glm, reasoningRequest{Enabled: &off}, "medium"},
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

func TestModelsHaveTheSubscriptionsConcurrencyLimit(t *testing.T) {
	for _, m := range testModels(t) {
		if m.MaxConcurrent != 2 {
			t.Errorf("%s: max_concurrent %d, want 2 (synthetic.new allows 2 calls per model at once)", m.ID, m.MaxConcurrent)
		}
	}
}

// codeaf-config.json's seats are allowed models, and CodeAF never runs more
// tasks at once than the worker model has slots.
func TestCodeAFConfigFitsTheConcurrencyLimit(t *testing.T) {
	data, err := os.ReadFile("codeaf-config.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	models := testModels(t)
	seat := func(key string) Model {
		id, _ := cfg[key].(string)
		return mustResolve(t, models, id)
	}
	worker := seat("models.tiers.worker")
	seat("models.tiers.mastermind")
	seat("models.tiers.high")
	parallel, ok := cfg["task.parallel"].(float64)
	if !ok || parallel < 1 || int(parallel) > worker.MaxConcurrent {
		t.Errorf("task.parallel %v: want 1..%d, the worker model's max_concurrent", cfg["task.parallel"], worker.MaxConcurrent)
	}
}

// The proxy never has more than max_concurrent calls to one model in flight
// upstream; the rest wait, and other models aren't held up by them.
func TestProxyLimitsCallsPerModel(t *testing.T) {
	var (
		mu       sync.Mutex
		inFlight = map[string]int{}
		peak     = map[string]int{}
	)
	hold := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		inFlight[body.Model]++
		peak[body.Model] = max(peak[body.Model], inFlight[body.Model])
		mu.Unlock()
		if body.Model == "hf:zai-org/GLM-5.3-Flash" {
			<-hold
		}
		mu.Lock()
		inFlight[body.Model]--
		mu.Unlock()
		_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	models := testModels(t)
	p := &proxy{upstream: upstream.URL, apiKey: "k", ledger: filepath.Join(t.TempDir(), "l.jsonl"), runCap: 5, dayCap: 20,
		models: models, client: upstream.Client(), slots: newSlots(models)}
	server := httptest.NewServer(p)
	defer server.Close()

	call := func(model string) int {
		response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
		if err != nil {
			t.Error(err)
			return 0
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if status := call("zai-org/glm-5.3-flash"); status != 200 {
				t.Errorf("flash call: %d", status)
			}
		}()
	}
	// While five Flash calls are queued behind two held ones, another
	// model still gets through.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := inFlight["hf:zai-org/GLM-5.3-Flash"]
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status := call("deepseek-ai/deepseek-v4.1-flash"); status != 200 {
		t.Errorf("deepseek call while flash is full: %d", status)
	}
	close(hold)
	wg.Wait()
	if got := peak["hf:zai-org/GLM-5.3-Flash"]; got != 2 {
		t.Errorf("peak flash calls in flight upstream: %d, want 2", got)
	}
	// The three flash calls that queued behind the two held slots recorded
	// how long they waited for a slot.
	lines, err := readLedger(p.ledger)
	if err != nil {
		t.Fatal(err)
	}
	waited, slotWait := 0, 0.0
	for _, l := range lines {
		if l.Model != "zai-org/glm-5.3-flash" || l.SlotWaitS <= 0 {
			continue
		}
		waited++
		slotWait += l.SlotWaitS
	}
	if waited < 3 || slotWait <= 0 {
		t.Errorf("ledger records %d queued flash calls totalling %.3fs; want the 3 that waited", waited, slotWait)
	}
}

func TestAcquireGivesUpWhenTheCallerDoes(t *testing.T) {
	m := Model{Upstream: "u", MaxConcurrent: 1}
	p := &proxy{slots: newSlots([]Model{m})}
	release, waited, err := p.acquire(context.Background(), m)
	if err != nil || waited != 0 {
		t.Fatalf("first acquire: %v, waited %v", err, waited)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, waited, err := p.acquire(ctx, m); err == nil {
		t.Fatal("second acquire got a slot past max_concurrent 1")
	} else if waited < 10*time.Millisecond {
		t.Errorf("abandoned acquire reported %v of waiting, want at least the 10ms it waited", waited)
	}
	release()
	again, waited, err := p.acquire(context.Background(), m)
	if err != nil {
		t.Fatal("slot not given back:", err)
	}
	again()
	if _, waited, err := p.acquire(context.Background(), Model{Upstream: "unlimited"}); err != nil || waited != 0 {
		t.Fatalf("a model with no max_concurrent must not wait: %v, %v", err, waited)
	}
}

func TestDecide(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_RUN_ID", "42")
	labelled := []label{{Name: "codeaf"}}
	owner := &comment{Body: "/codeaf go", AuthorAssociation: "OWNER", User: user{Login: "RichardoC", Type: "User"}}
	prMarker := json.RawMessage(`{"url":"x"}`)
	openPR := item{Number: 7, State: "open", Labels: labelled}
	openPR.Head.Ref = "codeaf/issue-3"
	openPR.Head.Repo = &struct {
		FullName string `json:"full_name"`
	}{FullName: "RichardoC/factory-repo"}
	openPR.Base.Ref = "main"
	closedPR := openPR
	closedPR.State = "closed"
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
		{"issue labelled", "issues", with(func(e *event) { e.Label = &label{Name: "codeaf"}; e.Issue = &item{Number: 3, Labels: labelled} }), noLookup, true, "issue", "codeaf/issue-3-42"},
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
		{"command on issue", "issue_comment", with(func(e *event) { e.Issue = &item{Number: 3, Labels: labelled}; e.Comment = owner }), noLookup, true, "issue", "codeaf/issue-3-42"},
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
		{"approval carrying a command", "pull_request_review", with(func(e *event) {
			e.PullRequest = &openPR
			e.Review = &comment{Body: "/codeaf tidy the docs", State: "approved", AuthorAssociation: "OWNER", User: user{Type: "User"}}
		}), lookup(openPR), true, "pr", "codeaf/issue-3"},
		{"untrusted reviewer", "pull_request_review", with(func(e *event) {
			e.PullRequest = &openPR
			e.Review = &comment{Body: "do this", State: "changes_requested", AuthorAssociation: "CONTRIBUTOR", User: user{Login: "x", Type: "User"}}
		}), noLookup, false, "", ""},
		{"bot comment", "issue_comment", with(func(e *event) {
			e.Issue = &item{Number: 3, Labels: labelled}
			e.Comment = &comment{Body: "/codeaf go", AuthorAssociation: "MEMBER", User: user{Login: "some-bot[bot]", Type: "Bot"}}
		}), noLookup, false, "", ""},
		{"command on closed PR", "issue_comment", with(func(e *event) {
			e.Issue = &item{Number: 7, Labels: labelled, PullRequest: prMarker}
			e.Comment = owner
		}), lookup(closedPR), false, "", ""},
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

// TestRunID pins the run-id validation behind the issue branch name.
func TestRunID(t *testing.T) {
	for _, bad := range []string{"", "abc", "12a", " 42", "-1"} {
		t.Setenv("GITHUB_RUN_ID", bad)
		if _, err := runID(); err == nil {
			t.Errorf("runID accepted %q", bad)
		}
	}
	t.Setenv("GITHUB_RUN_ID", "42")
	if id, err := runID(); err != nil || id != "42" {
		t.Errorf("runID(42) = %q, %v", id, err)
	}
}

func TestDecideRefusesABadRunID(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
	t.Setenv("GITHUB_RUN_ID", "nope")
	base := event{}
	base.Repository.DefaultBranch = "main"
	ev := base
	ev.Label = &label{Name: "codeaf"}
	ev.Issue = &item{Number: 3, Labels: []label{{Name: "codeaf"}}}
	if _, err := Decide("issues", ev, "codeaf", func(int) (item, error) {
		t.Fatal("unexpected PR lookup")
		return item{}, nil
	}); err == nil {
		t.Fatal("Decide accepted a non-numeric GITHUB_RUN_ID")
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
	env := map[string]string{"DAY_SPENT_BEFORE": "1.2", "CHANGED": "3",
		"GITHUB_REPOSITORY": "RichardoC/factory-repo", "GITHUB_RUN_ID": "42"}
	cfg := Config{Limits: Limits{RunUSD: 5, DayUSD: 20}, Models: testModels(t)}
	report := Report(result, true, lines, cfg, nil, ReportInputs{}, func(k string) string { return env[k] })
	for _, want := range []string{
		reportMarker,
		"| stop / ok | `done` / `true` |",
		"**$0.0025** this run · $1.2025 today (cap $5/run, $20/day)",
		"| `zai-org/glm-5.3` | 1.4 · 0.26 · 4.4 | 2 | 1000 | 0 | 200 | — | $0.0023 (+1 unpriced) |",
		"| `zai-org/glm-5.3-flash` | 0.15 · 0.04 · 0.5 | 1 | 1000 | 400 | 200 | — | $0.0002 |",
		// Old-format ledger lines: the timing figures degrade to em-dashes.
		"| run job wall time | — |",
		"| time with a call in flight | — |",
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

// The proxy records completion_tokens_details.reasoning_tokens on the ledger
// for JSON and streamed responses, passes the details through to CodeAF
// untouched, records a known zero, and omits the field when upstream did not
// send it.
func TestProxyRecordsReasoningTokens(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls++ // the test posts one call at a time
		details := ""
		switch calls {
		case 1, 2: // one JSON call, one streamed call: reasoning reported
			details = `,"completion_tokens_details":{"reasoning_tokens":321}`
		case 3: // reasoning known to be zero
			details = `,"completion_tokens_details":{"reasoning_tokens":0}`
			// case 4: no completion_tokens_details at all
		}
		usage := `{"prompt_tokens":100,"completion_tokens":60` + details + `}`
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
	p := &proxy{upstream: upstream.URL, apiKey: "k", ledger: ledger, runCap: 5, dayCap: 20,
		models: testModels(t), client: upstream.Client()}
	server := httptest.NewServer(p)
	defer server.Close()

	post := func(body string) string {
		t.Helper()
		response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return string(data)
	}
	small := Usage{PromptTokens: 100, CompletionTokens: 60}
	var costDetails struct {
		Cost                    float64         `json:"cost"`
		CompletionTokensDetails json.RawMessage `json:"completion_tokens_details"`
	}

	// The JSON call keeps the raw details and gains usage.cost.
	got := post(`{"model":"zai-org/glm-5.3-flash","messages":[]}`)
	if err := json.Unmarshal([]byte(got), &struct {
		Usage *struct {
			Cost                    float64         `json:"cost"`
			CompletionTokensDetails json.RawMessage `json:"completion_tokens_details"`
		} `json:"usage"`
	}{Usage: &costDetails}); err != nil {
		t.Fatal(err)
	}
	if want := CostOf(mustResolve(t, testModels(t), "zai-org/glm-5.3-flash"), small); costDetails.Cost != want {
		t.Errorf("usage.cost = %v, want %v", costDetails.Cost, want)
	}
	if string(costDetails.CompletionTokensDetails) != `{"reasoning_tokens":321}` {
		t.Errorf("completion_tokens_details = %s", costDetails.CompletionTokensDetails)
	}

	// The streamed call: the usage chunk keeps the raw details and the cost.
	got = post(`{"model":"zai-org/glm-5.3","stream":true,"messages":[]}`)
	foundUsage := false
	for _, chunk := range strings.Split(got, "\n") {
		chunk = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(chunk), "data: "))
		if !strings.Contains(chunk, `"usage"`) {
			continue
		}
		var line struct {
			Usage *struct {
				Cost                    float64         `json:"cost"`
				CompletionTokensDetails json.RawMessage `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(chunk), &line); err != nil || line.Usage == nil {
			continue
		}
		foundUsage = true
		if want := CostOf(mustResolve(t, testModels(t), "zai-org/glm-5.3"), small); line.Usage.Cost != want {
			t.Errorf("streamed usage.cost = %v, want %v", line.Usage.Cost, want)
		}
		if string(line.Usage.CompletionTokensDetails) != `{"reasoning_tokens":321}` {
			t.Errorf("streamed completion_tokens_details = %s", line.Usage.CompletionTokensDetails)
		}
	}
	if !foundUsage {
		t.Errorf("no usage chunk in the stream: %s", got)
	}

	// A known zero records 0; an absent detail stays absent.
	post(`{"model":"zai-org/glm-5.3-flash","messages":[]}`)
	post(`{"model":"zai-org/glm-5.3-flash","messages":[]}`)
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	texts := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(texts) != 4 {
		t.Fatalf("ledger has %d lines:\n%s", len(texts), raw)
	}
	lines, err := readLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	want := []*int{&[]int{321}[0], &[]int{321}[0], &[]int{0}[0], nil}
	for i, w := range want {
		if (lines[i].ReasoningTokens == nil) != (w == nil) {
			t.Errorf("line %d reasoning = %v, want %v", i+1, lines[i].ReasoningTokens, w)
			continue
		}
		if w != nil && *lines[i].ReasoningTokens != *w {
			t.Errorf("line %d reasoning = %d, want %d", i+1, *lines[i].ReasoningTokens, *w)
		}
	}
	if !strings.Contains(texts[0], `"reasoning_tokens":321`) || !strings.Contains(texts[1], `"reasoning_tokens":321`) {
		t.Errorf("lines 1–2 lack the recorded count:\n%s\n%s", texts[0], texts[1])
	}
	if !strings.Contains(texts[2], `"reasoning_tokens":0`) {
		t.Errorf("line 3 should record a known zero: %s", texts[2])
	}
	if strings.Contains(texts[3], "reasoning_tokens") {
		t.Errorf("line 4 should not carry reasoning_tokens: %s", texts[3])
	}
}

// The proxy opens its ledger with one proxy_start marker line, which carries
// no start, tokens or cost.
func TestRecordMarker(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "l.jsonl")
	p := &proxy{ledger: ledger}
	p.recordMarker("proxy_start")
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if text := strings.TrimSpace(string(raw)); !strings.Contains(text, `"kind":"proxy_start"`) ||
		strings.Contains(text, "tokens") || strings.Contains(text, `"start"`) || strings.Contains(text, "cost") {
		t.Errorf("marker line = %s", text)
	}
	lines, err := readLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Kind != "proxy_start" || lines[0].Time.IsZero() {
		t.Fatalf("marker = %+v", lines)
	}
}

// The timing table: figures from crafted ledger lines, the proxy_start marker
// skipped as a non-call, and em-dashes when the ledger predates timing.
func TestReportTiming(t *testing.T) {
	result := Result{Stop: "done"}
	base := time.Now().Add(-time.Hour)
	reasoning := 321
	wall := 88 * time.Minute
	// Calls [base+1m, base+2m] and [base+90s, base+4m]: three minutes with a
	// call in flight, 2m30s of it alone and 30s of it two at once. The base
	// is relative to now because only spans that end by now are believed.
	lines := []LedgerLine{
		{Kind: "proxy_start", Time: base},
		{Model: "zai-org/glm-5.3-flash", Time: base.Add(2 * time.Minute), Start: base.Add(time.Minute),
			PromptTokens: 1000, CompletionTokens: 45003, ReasoningTokens: &reasoning, CostUSD: 0.0225, UsageSeen: true, SlotWaitS: 90},
		{Model: "zai-org/glm-5.3", Time: base.Add(4 * time.Minute), Start: base.Add(90 * time.Second),
			PromptTokens: 1000, CompletionTokens: 1000, CostUSD: 0.0044, UsageSeen: true},
	}
	cfg := Config{Limits: Limits{RunUSD: 5, DayUSD: 20}, Models: testModels(t)}
	report := Report(result, true, lines, cfg, &wall, ReportInputs{}, func(string) string { return "" })
	for _, want := range []string{
		"| run job wall time | 1h28m0s |",
		"| CodeAF session (proxy start → last call end) | 4m0s |",
		"| time with a call in flight | 3m0s |",
		"| idle (wall time minus in-flight) | 1h25m0s |",
		"| in-flight split (1 / 2 / 3+ concurrent) | 2m30s · 30s · 0s |",
		"255.6 tok/s", // (45003+1000) output tokens over the 3m in flight
		"| longest single call | 2m30s · 1000 tok · `zai-org/glm-5.3` |",
		"| queued at the model limit | 1m30s |",
		"| `zai-org/glm-5.3-flash` | 0.15 · 0.04 · 0.5 | 1 | 1000 | 0 | 45003 | 321 | $0.0225 |",
		"| `zai-org/glm-5.3` | 1.4 · 0.26 · 4.4 | 1 | 1000 | 0 | 1000 | — | $0.0044 |",
		"**$0.0269** this run", // marker line adds nothing to the metered total
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "| `` |") {
		t.Errorf("the proxy_start marker was counted as a call:\n%s", report)
	}

	// An old-format ledger (no start, no kind, no reasoning) degrades to
	// em-dashes, and a missing wall time does too.
	old := []LedgerLine{{Model: "zai-org/glm-5.3-flash", Time: base, PromptTokens: 1000,
		CompletionTokens: 200, CostUSD: 0.0001, UsageSeen: true}}
	report = Report(result, true, old, cfg, nil, ReportInputs{}, func(string) string { return "" })
	for _, want := range []string{
		"| run job wall time | — |",
		"| CodeAF session (proxy start → last call end) | — |",
		"| time with a call in flight | — |",
		"| idle (wall time minus in-flight) | — |",
		"| in-flight split (1 / 2 / 3+ concurrent) | — |",
		"| output tok/s while in flight | — |",
		"| longest single call | — |",
		"| queued at the model limit | — |",
		"| `zai-org/glm-5.3-flash` | 0.15 · 0.04 · 0.5 | 1 | 1000 | 0 | 200 | — | $0.0001 |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("old-format report lacks %q:\n%s", want, report)
		}
	}
}

// runWallTime picks the run job (latest attempt) out of the workflow's jobs
// and falls back to nil on anything it cannot parse.
func TestRunWallTime(t *testing.T) {
	var lastPath, lastQuery, auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath, lastQuery, auth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/runs/garbage/jobs"):
			_, _ = io.WriteString(w, "not json")
		case strings.HasSuffix(r.URL.Path, "/runs/error/jobs"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/runs/unfinished/jobs"):
			_, _ = io.WriteString(w, `{"jobs":[{"name":"run","run_attempt":1,"started_at":"2026-10-16T20:00:00Z"}]}`)
		default:
			_, _ = io.WriteString(w, `{"jobs":[
				{"name":"gate","run_attempt":1,"started_at":"2026-10-16T19:58:00Z","completed_at":"2026-10-16T19:59:00Z"},
				{"name":"run","run_attempt":1,"started_at":"2026-10-16T20:00:00Z","completed_at":"2026-10-16T20:10:00Z"},
				{"name":"run","run_attempt":2,"started_at":"2026-10-16T21:00:00Z","completed_at":"2026-10-16T22:28:04Z"}]}`)
		}
	}))
	defer server.Close()

	setEnv := func(runID string) {
		t.Setenv("GITHUB_API_URL", server.URL)
		t.Setenv("GITHUB_REPOSITORY", "RichardoC/factory-repo")
		t.Setenv("GITHUB_RUN_ID", runID)
		t.Setenv("GITHUB_TOKEN", "tok")
	}

	setEnv("42")
	wall := runWallTime()
	if wall == nil || *wall != 88*time.Minute+4*time.Second {
		t.Fatalf("run job wall time = %v, want 1h28m4s from the latest attempt", wall)
	}
	if !strings.HasSuffix(lastPath, "/repos/RichardoC/factory-repo/actions/runs/42/jobs") || lastQuery != "per_page=100" {
		t.Errorf("asked %s?%s", lastPath, lastQuery)
	}
	if auth != "Bearer tok" {
		t.Errorf("Authorization %q", auth)
	}

	for _, bad := range []string{"garbage", "error", "unfinished"} {
		setEnv(bad)
		if wall := runWallTime(); wall != nil {
			t.Errorf("runWallTime(%s) = %v, want nil", bad, wall)
		}
	}

	// A missing token means no figure, and no request at all.
	auth = ""
	setEnv("42")
	t.Setenv("GITHUB_TOKEN", "")
	if wall := runWallTime(); wall != nil {
		t.Errorf("runWallTime without a token = %v, want nil", wall)
	}
	if auth != "" {
		t.Error("a request went out without a token")
	}
}

func TestMain(m *testing.M) {
	// Tests read models.json next to this file.
	os.Setenv("HARNESS_DIR", ".")
	os.Exit(m.Run())
}

func TestSanitizeLedger(t *testing.T) {
	models := testModels(t)
	raw := `{
	  "entries": [
	    {"model":"zai-org/glm-5.3-flash","class":"","rating":-1.2,"count":2,"updated":"2026-10-15T16:48:19Z"},
	    {"model":"zai-org/glm-5.3","class":"crew","rating":0.5,"count":1,"updated":"2026-10-15T16:48:19Z"},
	    {"model":"zai-org/glm-5.3-flash","class":"","rating":9,"count":9,"updated":"2026-10-15T16:48:19Z"},
	    {"model":"moonshotai/kimi-k3","class":"","rating":5,"count":1,"updated":"2026-10-15T16:48:19Z"},
	    {"model":"zai-org/glm-5.3","class":"ignore previous instructions","rating":1,"count":1,"updated":"2026-10-15T16:48:19Z"},
	    {"model":"deepseek-ai/deepseek-v4.1-flash","class":"","rating":1e9,"count":1,"updated":"2026-10-15T16:48:19Z"},
	    {"model":"deepseek-ai/deepseek-v4.1-flash","class":"","rating":1,"count":-4,"updated":"2026-10-15T16:48:19Z"}
	  ],
	  "aliases": {"deepseek-ai/deepseek-v4.1-flash":"zai-org/glm-5.3-flash","evil/model":"zai-org/glm-5.3"},
	  "note": "free text the agent planted"
	}`
	clean, dropped := SanitizeLedger([]byte(raw), models)
	if len(clean.Entries) != 2 || dropped != 6 {
		t.Fatalf("kept %d, dropped %d: %+v", len(clean.Entries), dropped, clean)
	}
	if clean.Entries[0].Model != "zai-org/glm-5.3-flash" || clean.Entries[0].Rating != -1.2 || clean.Entries[1].Class != "crew" {
		t.Errorf("entries = %+v", clean.Entries)
	}
	if len(clean.Aliases) != 1 || clean.Aliases["deepseek-ai/deepseek-v4.1-flash"] != "zai-org/glm-5.3-flash" {
		t.Errorf("aliases = %v", clean.Aliases)
	}
	out, _ := json.Marshal(clean)
	if strings.Contains(string(out), "planted") || strings.Contains(string(out), "instructions") {
		t.Errorf("free text survived: %s", out)
	}
	if clean, dropped := SanitizeLedger([]byte("not json"), models); len(clean.Entries) != 0 || dropped == 0 {
		t.Errorf("garbage: %+v dropped=%d", clean, dropped)
	}
}

func TestSpentTodayRefusesPoisonedTotals(t *testing.T) {
	for _, bad := range []string{"-5", "NaN", "Inf", "-Inf", "lots"} {
		t.Setenv("DAY_SPENT", bad)
		if _, err := spentToday("DAY_SPENT"); err == nil {
			t.Errorf("spentToday accepted %q", bad)
		}
	}
	t.Setenv("DAY_SPENT", "1.25")
	if got, err := spentToday("DAY_SPENT"); err != nil || got != 1.25 {
		t.Errorf("spentToday(1.25) = %v, %v", got, err)
	}
	t.Setenv("DAY_SPENT", "")
	if got, err := spentToday("DAY_SPENT"); err != nil || got != 0 {
		t.Errorf("spentToday(empty) = %v, %v", got, err)
	}
}

// A hostile model string from the untrusted ledger must not reach the
// comment's markdown: only configured model IDs render, and everything else
// groups as "(other)", in the per-model table and in the longest-call row.
func TestReportGroupsUnknownModels(t *testing.T) {
	result := Result{Stop: "done"}
	hostile := "x`\n\n# INJECT @user [i](http://e/x)"
	base := time.Now().Add(-time.Hour)
	lines := []LedgerLine{
		{Model: hostile, Time: base.Add(2 * time.Minute), Start: base,
			PromptTokens: 100, CompletionTokens: 500, CostUSD: 0.5, UsageSeen: true},
		{Model: hostile + "2", Time: base.Add(time.Minute), Start: base,
			PromptTokens: 100, CostUSD: 0.25, UsageSeen: true},
		{Model: "zai-org/glm-5.3", Time: base.Add(30 * time.Second), Start: base,
			PromptTokens: 1000, CompletionTokens: 200, CostUSD: 0.00228, UsageSeen: true},
	}
	cfg := Config{Limits: Limits{RunUSD: 5, DayUSD: 20}, Models: testModels(t)}
	report := Report(result, true, lines, cfg, nil, ReportInputs{}, func(string) string { return "" })
	for _, banned := range []string{"INJECT", "@user", "http://e/x"} {
		if strings.Contains(report, banned) {
			t.Errorf("hostile model string leaked %q into the report:\n%s", banned, report)
		}
	}
	for _, want := range []string{
		"| `(other)` | — · — · — | 2 | 200 | 0 | 500 | — | $0.7500 |",
		"| `zai-org/glm-5.3` | 1.4 · 0.26 · 4.4 | 1 | 1000 | 0 | 200 | — | $0.0023 |",
		"| longest single call | 2m0s · 500 tok · `(other)` |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if n := strings.Count(report, "| `(other)` |"); n != 1 {
		t.Errorf("distinct hostile models did not group into one (other) row (%d):\n%s", n, report)
	}
}

// Absurd timestamps and counts from the hostile ledger must not render as
// nonsense: spans that end after now, start before the proxy_start marker, or
// last more than a day are dropped, and token counts and slot waits clamp to
// sane bounds.
func TestReportDropsAbsurdTiming(t *testing.T) {
	result := Result{Stop: "done"}
	now := time.Now()
	huge := 1 << 62
	lines := []LedgerLine{
		{Kind: "proxy_start", Time: now.Add(-time.Hour)},
		// Year 0001 to 9999: this overflowed to -2562047h47m16s.
		{Model: "zai-org/glm-5.3", Start: time.Date(1, 1, 1, 0, 0, 1, 0, time.UTC),
			Time: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)},
		// Ends after now.
		{Model: "zai-org/glm-5.3", Start: now.Add(-time.Minute), Time: now.Add(30 * time.Minute)},
		// Starts before the proxy_start marker.
		{Model: "zai-org/glm-5.3", Start: now.Add(-2 * time.Hour), Time: now.Add(-90 * time.Minute)},
		// The one believable call, with hostile counts and wait.
		{Model: "zai-org/glm-5.3", Start: now.Add(-30 * time.Minute), Time: now.Add(-29 * time.Minute),
			PromptTokens: huge, CompletionTokens: huge, ReasoningTokens: &huge,
			SlotWaitS: 1e9, CostUSD: 0.01, UsageSeen: true},
	}
	cfg := Config{Limits: Limits{RunUSD: 5, DayUSD: 20}, Models: testModels(t)}
	report := Report(result, true, lines, cfg, nil, ReportInputs{}, func(string) string { return "" })
	for _, banned := range []string{"2562047h", "4611686018427387904"} {
		if strings.Contains(report, banned) {
			t.Errorf("hostile timing reached the report (%q found):\n%s", banned, report)
		}
	}
	for _, want := range []string{
		"| time with a call in flight | 1m0s |",
		"| CodeAF session (proxy start → last call end) | 31m0s |",
		"| longest single call | 1m0s · 10000000 tok · `zai-org/glm-5.3` |",
		"| queued at the model limit | 24h0m0s |",
		"| `zai-org/glm-5.3` | 1.4 · 0.26 · 4.4 | 4 | 10000000 | 0 | 10000000 | 10000000 | $0.0100 (+3 unpriced) |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

// The metered total must not lose spend the hostile ledger marks with an
// unknown kind: only the proxy_start marker is skipped. state pack falls back
// to this total when the proxy is gone.
func TestLedgerTotalCountsUnknownKinds(t *testing.T) {
	lines := []LedgerLine{
		{Kind: "proxy_start"},
		{CostUSD: 0.001},
		{Kind: "x", CostUSD: 5},
		{Kind: "proxy_start", CostUSD: 100}, // a hostile duplicate marker
	}
	if got := ledgerTotal(lines); math.Abs(got-5.001) > 1e-9 {
		t.Errorf("ledgerTotal = %v, want 5.001", got)
	}
}

// A failed publish: the report carries the ::error:: lines the publish steps
// collected — the two most recent are the cause — with the prefix stripped,
// and a link to the run's diagnostics artifact.
func TestReportFailedPublish(t *testing.T) {
	ok := false
	result := Result{Stop: "done", OK: &ok}
	in := ReportInputs{
		PublishErrors: []string{
			"::error::patch touches .github/ outside .github/codeaf/ without a maintainer's approval; refusing it",
			"::error::GitHub refused the push of codeaf/issue-9-42",
			"remote: GH006: Refused to update hidden path .github/workflows/ci.yml",
		},
		DiagArtifact: "12345",
	}
	env := map[string]string{"GITHUB_REPOSITORY": "RichardoC/factory-repo", "GITHUB_RUN_ID": "42",
		"OUTCOME": "publishing failed"}
	cfg := Config{Limits: Limits{RunUSD: 5, DayUSD: 20}, Models: testModels(t)}
	report := Report(result, true, nil, cfg, nil, in, func(k string) string { return env[k] })
	for _, want := range []string{
		"### CodeAF run — publishing failed",
		"| publish error | GitHub refused the push of codeaf/issue-9-42 |",
		"| publish error | remote: GH006: Refused to update hidden path .github/workflows/ci.yml |",
		"[Diagnostics](https://github.com/RichardoC/factory-repo/actions/runs/42/artifacts/12345)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	for _, banned := range []string{
		"::error::",                       // the prefix is not shown
		"without a maintainer's approval", // only the two most recent lines show
	} {
		if strings.Contains(report, banned) {
			t.Errorf("report leaks %q:\n%s", banned, report)
		}
	}
}

// CodeAF stopped without an answer (the deadline case): the report says why,
// summarises the patch and the plan's tasks, and says plainly that the work
// is unfinished and unverified.
func TestReportEmptyAnswerFallback(t *testing.T) {
	cfg := Config{Limits: Limits{RunUSD: 5, DayUSD: 20}, Models: testModels(t)}
	env := map[string]string{"GITHUB_REPOSITORY": "RichardoC/factory-repo", "GITHUB_RUN_ID": "42"}
	envf := func(k string) string { return env[k] }
	in := ReportInputs{
		PlanTasks: []planTask{{"write the parser", "done"}, {"wire the report", "open"}},
		Diff:      []fileDiff{{Path: "app/x.go", Add: 12, Del: 3}, {Path: "img/logo.png", Binary: true}},
		DiffMore:  7,
	}
	ok := false
	report := Report(Result{Stop: "deadline", OK: &ok}, true, nil, cfg, nil, in, envf)
	for _, want := range []string{
		"### Unfinished work",
		"CodeAF stopped (`deadline`) without an answer: **what it did here is unfinished and unverified.**",
		"[diagnostics](https://github.com/RichardoC/factory-repo/actions/runs/42)",
		"| app/x.go | 12 | 3 |",
		"| img/logo.png (binary) | — | — |",
		"… and 7 more files",
		"| write the parser | `done` |",
		"| wire the report | `open` |",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "CodeAF's summary") {
		t.Errorf("an empty answer must not render a summary block:\n%s", report)
	}
	// The fallback is for runs CodeAF did not call ok: a run it called ok, or
	// one that answered, is not unfinished.
	ok = true
	if report = Report(Result{Stop: "done", OK: &ok}, true, nil, cfg, nil, in, envf); strings.Contains(report, "Unfinished work") {
		t.Errorf("a run CodeAF called ok is not unfinished:\n%s", report)
	}
	if report = Report(Result{Stop: "done", Answer: "Did it."}, true, nil, cfg, nil, in, envf); strings.Contains(report, "Unfinished work") {
		t.Errorf("a run with an answer is not unfinished:\n%s", report)
	}
	// No result at all: the fallback still says what the patch changed.
	report = Report(Result{}, false, nil, cfg, nil, in, envf)
	for _, want := range []string{"Unfinished work", "produced no result", "| app/x.go | 12 | 3 |"} {
		if !strings.Contains(report, want) {
			t.Errorf("no-result report lacks %q:\n%s", want, report)
		}
	}
}

// The strings of the result file are hostile: stop, error, the seat models and
// the answer render inert — no raw HTML, no mentions, no images, no headings,
// no tables, no breakout of the details block — and a non-numeric artifact id
// is not believed.
func TestReportHostileResult(t *testing.T) {
	result := Result{
		Stop:  "done`\n\n# INJECTED HEADING @user",
		Error: "<script>alert(1)</script> ![pwn](http://e/i) @admin | break |",
		Model: "zai-org/glm-5.3\n\n# INJECT",
		Answer: "wrote code</details><img src=x onerror=alert(1)> ![pwn](http://e/i) @admin" +
			"\n\n# INJECTED\n\n| x | y |\n|---|---|\n\n" + strings.Repeat("a", 8000),
	}
	in := ReportInputs{
		DiagArtifact: `42"><script>`,
		PlanTasks:    []planTask{{Title: "`title` @admin", State: "done\n\n# X"}},
	}
	env := map[string]string{"GITHUB_REPOSITORY": "RichardoC/factory-repo", "GITHUB_RUN_ID": "42"}
	cfg := Config{Limits: Limits{RunUSD: 5, DayUSD: 20}, Models: testModels(t)}
	report := Report(result, true, nil, cfg, nil, in, func(k string) string { return env[k] })
	for _, banned := range []string{
		"<script", "<img", // raw HTML cannot survive
		"@admin", "@user", // mentions cannot survive
		"![pwn]",                     // images cannot survive
		"\n# INJECT", "\n# INJECTED", // headings cannot survive
		"| x | y |",         // tables cannot survive
		"/artifacts/",       // a non-numeric artifact id is ignored
		"zai-org/glm-5.3\n", // a hostile seat name renders as (other)
	} {
		if strings.Contains(report, banned) {
			t.Errorf("hostile result leaked %q into the report:\n%s", banned, report)
		}
	}
	if n := strings.Count(report, "</details>"); n != 1 {
		t.Errorf("the answer broke out of the details block (%d closers):\n%s", n, report)
	}
	if strings.Contains(report, strings.Repeat("a", 6100)) {
		t.Error("the answer was not capped")
	}
}

// diffstat reads a patch the way `git diff --stat` would: hunk lines that look
// like headers stay hunk lines, binary sections are not counted, quoted paths
// are unquoted, renames show both ends, and a deleted file keeps its old path.
func TestDiffstat(t *testing.T) {
	patch := []byte(`diff --git a/app/new.go b/app/new.go
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ b/app/new.go
@@ -0,0 +1,3 @@
+one
+two
+three
diff --git a/app/mod.go b/app/mod.go
index 1111111..2222222 100644
--- a/app/mod.go
+++ b/app/mod.go
@@ -1,3 +1,3 @@
 context
--- a/fake header
+++ b/fake header
-old line
+new line
diff --git a/app/gone.go b/app/gone.go
deleted file mode 100644
index 2222222..0000000
--- a/app/gone.go
+++ /dev/null
@@ -1 +0,0 @@
-bye
diff --git a/renamed.go b/moved.go
similarity index 100%
rename from renamed.go
rename to moved.go
diff --git a/bin.dat b/bin.dat
index 3333333..4444444 100644
GIT binary patch
literal 8
McZ{+-;=1)2|3?

diff --git a/we ird.go b/we ird.go
index 5555555..6666666 100644
--- "a/we ird.go"
+++ "b/we ird.go"
@@ -1 +1 @@
-a
+b
`)
	diffs, more := diffstat(patch)
	if more != 0 {
		t.Fatalf("more = %d, want 0", more)
	}
	want := []fileDiff{
		{Path: "app/new.go", Add: 3},
		{Path: "app/mod.go", Add: 2, Del: 2}, // the fake header lines are hunk lines
		{Path: "app/gone.go", Del: 1},
		{Path: "renamed.go → moved.go"},
		{Path: "bin.dat", Binary: true},
		{Path: "we ird.go", Add: 1, Del: 1},
	}
	if !reflect.DeepEqual(diffs, want) {
		t.Fatalf("diffstat = %+v, want %+v", diffs, want)
	}
}

// A huge diffstat cannot grow the report: the parser keeps at most
// maxDiffFiles files, the report renders at most maxDiffShown of them, and
// one path renders capped.
func TestReportCapsHugeDiffstat(t *testing.T) {
	var patch strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&patch, "diff --git a/f/%03d.go b/f/%03d.go\n--- a/f/%03d.go\n+++ b/f/%03d.go\n@@ -1 +1 @@\n-a\n+b\n", i, i, i, i)
	}
	diffs, more := diffstat([]byte(patch.String()))
	if len(diffs) != maxDiffFiles || more != 100 {
		t.Fatalf("diffstat kept %d files (more %d), want %d and 100", len(diffs), more, maxDiffFiles)
	}
	diffs = append(diffs, fileDiff{Path: strings.Repeat("p", 100000), Add: 1})
	cfg := Config{Limits: Limits{RunUSD: 5, DayUSD: 20}, Models: testModels(t)}
	report := Report(Result{}, false, nil, cfg, nil, ReportInputs{Diff: diffs, DiffMore: more}, func(string) string { return "" })
	// 50 shown of 201 kept, plus the 100 the parser did not keep.
	if !strings.Contains(report, "… and 251 more files") {
		t.Errorf("report lacks the more-files row:\n%s", report)
	}
	if strings.Contains(report, strings.Repeat("p", 300)) {
		t.Error("a hostile path was not capped")
	}
	if n := strings.Count(report, "\n| f/"); n != maxDiffShown {
		t.Errorf("rendered %d file rows, want %d", n, maxDiffShown)
	}
}

// The plan's tasks come out of whatever shape the result JSON carries, capped,
// and a result without them yields nothing.
func TestPlanTasks(t *testing.T) {
	got := planTasks([]byte(`{"stop":"done","tasks":[{"title":"a","state":"done"},{"name":"b","status":"open"},{"title":""},{"state":"bare"}]}`))
	if want := []planTask{{"a", "done"}, {"b", "open"}, {"", "bare"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("planTasks = %+v, want %+v", got, want)
	}
	if got = planTasks([]byte(`{"plan":{"tasks":[{"title":"t","state":"open"}]}}`)); !reflect.DeepEqual(got, []planTask{{"t", "open"}}) {
		t.Fatalf("planTasks(plan.tasks) = %+v", got)
	}
	for _, raw := range []string{`{"stop":"done"}`, `not json`, `{"tasks":"no"}`, `{"tasks":[1,2]}`, `{"tasks":[{"title":42}]}`, `{"plan":{"tasks":7}}`, ""} {
		if got := planTasks([]byte(raw)); got != nil {
			t.Errorf("planTasks(%q) = %+v, want nil", raw, got)
		}
	}
	var many []string
	for i := 0; i < 500; i++ {
		many = append(many, fmt.Sprintf(`{"title":"t%d"}`, i))
	}
	if got := planTasks([]byte(`{"tasks":[` + strings.Join(many, ",") + `]}`)); len(got) != maxPlanTasks {
		t.Fatalf("planTasks kept %d tasks, want %d", len(got), maxPlanTasks)
	}
}

// The collected ::error:: file is read capped: the oldest lines drop, the
// prefix strips, and a missing file is no error.
func TestReadPublishErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("::error::x\n", 25)+"::error::the last one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readPublishErrors(path)
	if len(got) != 20 || got[0] != "x" || got[len(got)-1] != "the last one" {
		t.Fatalf("readPublishErrors = %q", got)
	}
	if readPublishErrors(filepath.Join(t.TempDir(), "absent")) != nil {
		t.Error("a missing file is not an error")
	}
}
