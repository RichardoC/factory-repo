// Command harness is the glue between GitHub Actions, CodeAF and synthetic.new.
// It is standard library only, and built fresh by the workflow on every run.
//
//	harness proxy    serve CodeAF on 127.0.0.1: an OpenRouter-shaped model catalog
//	                 at synthetic.new's prices, and /chat/completions forwarded
//	                 upstream with usage.cost added and spend caps enforced
//	harness gate     decide whether this GitHub event may run CodeAF; writes step
//	                 outputs (run, reason, mode, number, branch, base)
//	harness brief    write the task brief for `codeaf do` to $BRIEF_PATH
//	harness summary  print CodeAF's JSON result without its long answer
//	harness state    the state shared between runs (state.go): load the shared
//	                 bundle into $CODEAF_HOME, pack what a run left, merge packed
//	                 runs into a new bundle
//	harness report   print the Markdown run report posted on the issue/PR
//
// Trust model: the `codeaf` label can only be added by people with triage access
// or above, and /codeaf commands are obeyed only from OWNER/MEMBER/COLLABORATOR.
// Issue and PR text is the task for CodeAF and never reaches a shell.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("harness: ")
	if len(os.Args) < 2 {
		log.Fatal("usage: harness proxy|gate|brief|summary|state|report")
	}
	commands := map[string]func() error{
		"proxy":   runProxy,
		"gate":    runGate,
		"brief":   runBrief,
		"summary": runSummary,
		"state":   runState,
		"report":  runReport,
	}
	command, ok := commands[os.Args[1]]
	if !ok {
		log.Fatalf("unknown command %q", os.Args[1])
	}
	if err := command(); err != nil {
		log.Fatal(err)
	}
}

// ---------------------------------------------------------------- models

// Model is one row of models.json. Prices are USD per million tokens.
type Model struct {
	ID              string   `json:"id"`       // what CodeAF sees
	Upstream        string   `json:"upstream"` // the synthetic.new model id
	Name            string   `json:"name"`
	Role            string   `json:"role"`
	Input           float64  `json:"input"`
	Output          float64  `json:"output"`
	CacheRead       float64  `json:"cache_read"`
	ContextLength   int      `json:"context_length"`
	InputModalities []string `json:"input_modalities"`
	Efforts         []string `json:"efforts"`
}

// Limits are the spend caps the proxy enforces, in USD.
type Limits struct {
	RunUSD float64 `json:"run_usd"` // per workflow run
	DayUSD float64 `json:"day_usd"` // per UTC day, across runs
}

// Config is models.json: the spend caps and the models CodeAF may use.
type Config struct {
	Limits Limits  `json:"limits"`
	Models []Model `json:"models"`
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Limits.RunUSD <= 0 || cfg.Limits.DayUSD <= 0 {
		return cfg, fmt.Errorf("%s: limits.run_usd and limits.day_usd must be positive", path)
	}
	if len(cfg.Models) == 0 {
		return cfg, fmt.Errorf("%s: no models", path)
	}
	for _, m := range cfg.Models {
		if m.ID == "" || m.Upstream == "" || len(m.Efforts) == 0 {
			return cfg, fmt.Errorf("%s: model %q needs id, upstream and efforts", path, m.ID)
		}
	}
	return cfg, nil
}

func configPath() string {
	return envOr("PROXY_MODELS", filepath.Join(harnessDir(), "models.json"))
}

func harnessDir() string {
	if dir := os.Getenv("HARNESS_DIR"); dir != "" {
		return dir
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// Usage is the OpenAI usage block, plus the cost field CodeAF reads.
type Usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens,omitempty"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails json.RawMessage `json:"completion_tokens_details,omitempty"`
	Cost                    float64         `json:"cost"`
}

func (u Usage) cached() int {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return min(u.PromptTokensDetails.CachedTokens, u.PromptTokens)
}

// CostOf prices one call: uncached input, cached input and output (which
// includes reasoning tokens) at the model's per-million rates.
func CostOf(m Model, u Usage) float64 {
	cached := u.cached()
	return (float64(u.PromptTokens-cached)*m.Input + float64(cached)*m.CacheRead + float64(u.CompletionTokens)*m.Output) / 1e6
}

// ---------------------------------------------------------------- proxy

type proxy struct {
	upstream string
	apiKey   string
	ledger   string
	runCap   float64
	dayCap   float64
	daySpent float64
	models   []Model
	client   *http.Client

	mu       sync.Mutex
	runSpent float64
}

func envFloat(name string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func runProxy() error {
	p := &proxy{
		upstream: strings.TrimRight(envOr("PROXY_UPSTREAM", "https://api.synthetic.new/openai/v1"), "/"),
		apiKey:   os.Getenv("SYNTHETIC_API_KEY"),
		ledger:   envOr("PROXY_LEDGER", "codeaf-ledger.jsonl"),
		client:   &http.Client{Timeout: 15 * time.Minute},
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		return err
	}
	p.models, p.runCap, p.dayCap = cfg.Models, cfg.Limits.RunUSD, cfg.Limits.DayUSD
	if p.daySpent, err = spentToday("PROXY_DAY_SPENT_USD"); err != nil {
		return err
	}
	// Preferred: the key arrives on stdin, so it is not in /proc/<pid>/environ
	// where the CodeAF job (same user) could read it.
	if p.apiKey == "" {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		p.apiKey = strings.TrimSpace(line)
	}
	if p.apiKey == "" {
		return errors.New("no SYNTHETIC_API_KEY in the environment or on stdin")
	}
	addr := "127.0.0.1:" + envOr("PROXY_PORT", "8787")
	log.Printf("proxy on %s -> %s (run cap $%.2f, day cap $%.2f, day spent $%.4f)", addr, p.upstream, p.runCap, p.dayCap, p.daySpent)
	return http.ListenAndServe(addr, p)
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	log.Printf("%s %s", r.Method, path)
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/models"):
		writeJSON(w, http.StatusOK, catalog(p.models))
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/healthz"):
		p.mu.Lock()
		spent := p.runSpent
		p.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "run_spent_usd": spent})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/chat/completions"):
		p.chat(w, r)
	default:
		writeError(w, http.StatusNotFound, "not served by the codeaf proxy: "+r.Method+" "+path)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	log.Printf("-> %d %s", status, message)
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "code": status}})
}

// catalogRow is one row of OpenRouter's /models listing, in the fields CodeAF
// reads (internal/catalog modelWire). Prices are USD per token, as strings.
type catalogRow struct {
	ID            string `json:"id"`
	CanonicalSlug string `json:"canonical_slug"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Created       int64  `json:"created"`
	Architecture  struct {
		Input  []string `json:"input_modalities"`
		Output []string `json:"output_modalities"`
	} `json:"architecture"`
	Pricing struct {
		Prompt         string `json:"prompt"`
		Completion     string `json:"completion"`
		Request        string `json:"request"`
		InputCacheRead string `json:"input_cache_read"`
	} `json:"pricing"`
	SupportedParameters []string `json:"supported_parameters"`
	Reasoning           struct {
		Mandatory        bool     `json:"mandatory"`
		DefaultEnabled   bool     `json:"default_enabled"`
		SupportedEfforts []string `json:"supported_efforts"`
		DefaultEffort    string   `json:"default_effort"`
	} `json:"reasoning"`
	HuggingFaceID string `json:"hugging_face_id"`
}

func perToken(perMillion float64) string {
	return strconv.FormatFloat(perMillion/1e6, 'f', 12, 64)
}

func catalog(models []Model) map[string][]catalogRow {
	rows := make([]catalogRow, 0, len(models))
	for _, m := range models {
		var row catalogRow
		row.ID, row.CanonicalSlug, row.Name = m.ID, m.ID, m.Name
		row.ContextLength = m.ContextLength
		row.Created = 1760000000
		row.Architecture.Input = m.InputModalities
		row.Architecture.Output = []string{"text"}
		row.Pricing.Prompt = perToken(m.Input)
		row.Pricing.Completion = perToken(m.Output)
		row.Pricing.Request = "0"
		row.Pricing.InputCacheRead = perToken(m.CacheRead)
		row.SupportedParameters = []string{"tools", "tool_choice", "reasoning", "include_reasoning",
			"response_format", "structured_outputs", "temperature", "top_p", "max_tokens", "stop", "seed"}
		row.Reasoning.DefaultEnabled = true
		row.Reasoning.SupportedEfforts = m.Efforts
		row.Reasoning.DefaultEffort = m.Efforts[0]
		row.HuggingFaceID = strings.TrimPrefix(m.Upstream, "hf:")
		rows = append(rows, row)
	}
	return map[string][]catalogRow{"data": rows}
}

var effortSuffixes = []string{":none", ":minimal", ":low", ":medium", ":high", ":xhigh", ":max"}

// Resolve maps whatever model id CodeAF sends to a models.json row.
func Resolve(models []Model, requested string) (Model, bool) {
	id := strings.TrimPrefix(strings.TrimSpace(requested), "~")
	for _, suffix := range effortSuffixes {
		id = strings.TrimSuffix(id, suffix)
	}
	for _, m := range models {
		if strings.EqualFold(id, m.ID) || strings.EqualFold(id, m.Upstream) ||
			strings.EqualFold(id, strings.TrimPrefix(m.Upstream, "hf:")) {
			return m, true
		}
	}
	return Model{}, false
}

var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// reasoningRequest is OpenRouter's reasoning block, as CodeAF sends it.
type reasoningRequest struct {
	Effort  string `json:"effort"`
	Enabled *bool  `json:"enabled"`
}

// EffortFor turns OpenRouter's reasoning block into an OpenAI reasoning_effort
// the model supports: the requested one, else the next one up, else the
// model's highest. A request to switch reasoning off becomes the model's lowest
// effort. An empty result means "send nothing".
func EffortFor(m Model, reasoning reasoningRequest) string {
	wanted := reasoning.Effort
	if reasoning.Enabled != nil && !*reasoning.Enabled {
		wanted = "none"
	}
	if wanted == "" {
		return ""
	}
	if slices.Contains(m.Efforts, wanted) {
		return wanted
	}
	rank := slices.Index(effortOrder, wanted)
	if rank < 0 {
		rank = slices.Index(effortOrder, "high")
	}
	for _, effort := range effortOrder[rank:] {
		if slices.Contains(m.Efforts, effort) {
			return effort
		}
	}
	return m.Efforts[len(m.Efforts)-1]
}

// openRouterOnly are request fields synthetic.new does not understand.
var openRouterOnly = []string{"provider", "transforms", "route", "models", "usage", "plugins", "include_reasoning", "reasoning"}

// RewriteRequest rewrites a chat request for synthetic.new. Fields it does not
// know about pass through untouched.
func RewriteRequest(body map[string]json.RawMessage, m Model) (stream bool, err error) {
	if raw, ok := body["reasoning"]; ok {
		var reasoning reasoningRequest
		if json.Unmarshal(raw, &reasoning) == nil {
			if effort := EffortFor(m, reasoning); effort != "" {
				body["reasoning_effort"], _ = json.Marshal(effort)
			}
		}
	}
	for _, key := range openRouterOnly {
		delete(body, key)
	}
	body["model"], _ = json.Marshal(m.Upstream)
	if raw, ok := body["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	if stream {
		options := map[string]json.RawMessage{}
		if raw, ok := body["stream_options"]; ok {
			if err := json.Unmarshal(raw, &options); err != nil {
				return false, fmt.Errorf("stream_options: %w", err)
			}
		}
		options["include_usage"] = json.RawMessage("true")
		body["stream_options"], _ = json.Marshal(options)
	}
	return stream, nil
}

// LedgerLine is one call, as appended to the ledger and read by report.
type LedgerLine struct {
	Time             time.Time `json:"ts"`
	Model            string    `json:"model"`
	Upstream         string    `json:"upstream"`
	PromptTokens     int       `json:"prompt_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	Stream           bool      `json:"stream"`
	UsageSeen        bool      `json:"usage_seen"`
}

func (p *proxy) record(m Model, usage *Usage, stream bool) {
	line := LedgerLine{Time: time.Now().UTC(), Model: m.ID, Upstream: m.Upstream, Stream: stream}
	if usage != nil {
		line.UsageSeen = true
		line.PromptTokens, line.CachedTokens, line.CompletionTokens = usage.PromptTokens, usage.cached(), usage.CompletionTokens
		line.CostUSD = usage.Cost
	} else {
		log.Printf("warning: %s returned no usage; this call is unpriced", m.Upstream)
	}
	data, _ := json.Marshal(line)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runSpent += line.CostUSD
	f, err := os.OpenFile(p.ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("ledger: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

func (p *proxy) overCap() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runSpent >= p.runCap {
		return fmt.Sprintf("per-run spend cap $%.2f reached ($%.4f spent)", p.runCap, p.runSpent)
	}
	if p.daySpent+p.runSpent >= p.dayCap {
		return fmt.Sprintf("daily spend cap $%.2f reached ($%.4f spent today)", p.dayCap, p.daySpent+p.runSpent)
	}
	return ""
}

func (p *proxy) chat(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "request body: "+err.Error())
		return
	}
	var requested string
	_ = json.Unmarshal(body["model"], &requested)
	m, ok := Resolve(p.models, requested)
	if !ok {
		allowed := make([]string, len(p.models))
		for i, model := range p.models {
			allowed[i] = model.ID
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf("model %q is not allowed; allowed: %s", requested, strings.Join(allowed, ", ")))
		return
	}
	if reason := p.overCap(); reason != "" {
		writeError(w, http.StatusPaymentRequired, reason)
		return
	}
	stream, err := RewriteRequest(body, m)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	payload, _ := json.Marshal(body)
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstream+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	request.Header.Set("Authorization", "Bearer "+p.apiKey)
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	response, err := p.client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream unreachable: "+err.Error())
		return
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		log.Printf("upstream %d for %s: %.300s", response.StatusCode, m.Upstream, data)
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(data)
		return
	}
	if !stream {
		p.relayJSON(w, response.Body, m)
		return
	}
	p.relayStream(w, response.Body, m)
}

func (p *proxy) relayJSON(w http.ResponseWriter, upstream io.Reader, m Model) {
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(upstream).Decode(&payload); err != nil {
		writeError(w, http.StatusBadGateway, "upstream body: "+err.Error())
		return
	}
	var usage *Usage
	if raw, ok := payload["usage"]; ok && string(raw) != "null" {
		usage = new(Usage)
		if err := json.Unmarshal(raw, usage); err == nil {
			usage.Cost = CostOf(m, *usage)
			payload["usage"], _ = json.Marshal(usage)
		} else {
			usage = nil
		}
	}
	payload["model"], _ = json.Marshal(m.ID)
	p.record(m, usage, false)
	writeJSON(w, http.StatusOK, payload)
}

// RewriteChunk rewrites one SSE data payload: the model id back to CodeAF's,
// and usage.cost added when the chunk carries usage. It returns the usage it
// found, if any.
func RewriteChunk(data []byte, m Model) ([]byte, *Usage) {
	var chunk map[string]json.RawMessage
	if json.Unmarshal(data, &chunk) != nil {
		return data, nil
	}
	chunk["model"], _ = json.Marshal(m.ID)
	var usage *Usage
	if raw, ok := chunk["usage"]; ok && string(raw) != "null" {
		usage = new(Usage)
		if json.Unmarshal(raw, usage) == nil {
			usage.Cost = CostOf(m, *usage)
			chunk["usage"], _ = json.Marshal(usage)
		} else {
			usage = nil
		}
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return data, usage
	}
	return out, usage
}

func (p *proxy) relayStream(w http.ResponseWriter, upstream io.Reader, m Model) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	var usage *Usage
	reader := bufio.NewReaderSize(upstream, 1<<16)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if data, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
				data = bytes.TrimSpace(data)
				if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
					rewritten, found := RewriteChunk(data, m)
					if found != nil {
						usage = found
					}
					trimmed = append([]byte("data: "), rewritten...)
				}
			}
			_, _ = w.Write(append(trimmed, '\n'))
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("stream from %s cut: %v", m.Upstream, err)
			}
			break
		}
	}
	p.record(m, usage, true)
}

// ---------------------------------------------------------------- GitHub

const command = "/codeaf"

var trusted = []string{"OWNER", "MEMBER", "COLLABORATOR"}

type user struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type label struct {
	Name string `json:"name"`
}

// item is the part of an issue or pull request the gate and brief read.
type item struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	Labels      []label         `json:"labels"`
	Comments    int             `json:"comments"`
	PullRequest json.RawMessage `json:"pull_request"` // present on issues that are PRs
	Head        struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (i item) labelled(name string) bool {
	return slices.ContainsFunc(i.Labels, func(l label) bool { return l.Name == name })
}

// comment is an issue comment or a pull request review.
type comment struct {
	Body              string `json:"body"`
	AuthorAssociation string `json:"author_association"`
	User              user   `json:"user"`
	State             string `json:"state"` // reviews only
}

type event struct {
	Label       *label   `json:"label"`
	Issue       *item    `json:"issue"`
	PullRequest *item    `json:"pull_request"`
	Comment     *comment `json:"comment"`
	Review      *comment `json:"review"`
	Repository  struct {
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
}

func (e event) trigger() *comment {
	if e.Comment != nil {
		return e.Comment
	}
	return e.Review
}

type github struct {
	api, repo, token string
	client           *http.Client
}

func newGitHub() github {
	return github{
		api:    strings.TrimRight(envOr("GITHUB_API_URL", "https://api.github.com"), "/"),
		repo:   os.Getenv("GITHUB_REPOSITORY"),
		token:  os.Getenv("GITHUB_TOKEN"),
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (g github) get(path string, into any) error {
	request, err := http.NewRequest(http.MethodGet, g.api+"/repos/"+g.repo+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+g.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := g.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		return fmt.Errorf("GET %s: %s: %.300s", path, response.Status, data)
	}
	return json.NewDecoder(response.Body).Decode(into)
}

func readEvent() (event, error) {
	var ev event
	data, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return ev, err
	}
	return ev, json.Unmarshal(data, &ev)
}

// Decision is the gate's verdict, written as step outputs.
type Decision struct {
	Run    bool
	Reason string
	Mode   string // "issue" or "pr"
	Number int
	Branch string
	Base   string
}

func skip(reason string) Decision { return Decision{Reason: reason} }

// runID reads GITHUB_RUN_ID, which names this workflow run: issue runs use it
// as a branch suffix so every run gets its own branch, and the report links to
// it. A missing or non-numeric run id fails the gate.
func runID() (string, error) {
	id := os.Getenv("GITHUB_RUN_ID")
	if id == "" {
		return "", errors.New("GITHUB_RUN_ID is empty")
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("GITHUB_RUN_ID %q is not a run id of digits", id)
		}
	}
	return id, nil
}

// Decide applies the trust rules to one event. lookupPR fetches a PR when the
// event only carries its issue form; it is only called once the event has
// passed every check that needs no network.
func Decide(name string, ev event, labelName string, lookupPR func(int) (item, error)) (Decision, error) {
	issueRun := func(i item, reason string) (Decision, error) {
		id, err := runID()
		if err != nil {
			return Decision{}, err
		}
		return Decision{Run: true, Reason: reason, Mode: "issue", Number: i.Number,
			Branch: fmt.Sprintf("codeaf/issue-%d-%s", i.Number, id), Base: ev.Repository.DefaultBranch}, nil
	}
	var target *item
	var trigger *comment
	body := ""
	switch name {
	case "issues":
		if ev.Label == nil || ev.Label.Name != labelName || ev.Issue == nil {
			return skip("label is not " + labelName), nil
		}
		if len(ev.Issue.PullRequest) > 0 {
			return skip("labelled a PR; PRs act on " + command + " commands"), nil
		}
		return issueRun(*ev.Issue, "issue labelled")
	case "issue_comment":
		target, trigger = ev.Issue, ev.Comment
		if trigger != nil {
			body = strings.TrimSpace(trigger.Body)
		}
	case "pull_request_review":
		target, trigger = ev.PullRequest, ev.Review
		if trigger != nil {
			body = strings.TrimSpace(trigger.Body)
			if trigger.State == "changes_requested" && !strings.HasPrefix(body, command) {
				body = command + " " + body
			}
		}
	default:
		return skip("unhandled event " + name), nil
	}
	switch {
	case target == nil || trigger == nil:
		return skip("event has no item or comment"), nil
	case !target.labelled(labelName):
		return skip("item is not labelled " + labelName), nil
	case !strings.HasPrefix(body, command):
		return skip("comment is not a " + command + " command"), nil
	case !slices.Contains(trusted, trigger.AuthorAssociation):
		return skip(fmt.Sprintf("author %s is %s, not trusted", trigger.User.Login, trigger.AuthorAssociation)), nil
	case trigger.User.Type == "Bot":
		return skip("bot comment"), nil
	}
	if name == "issue_comment" && len(target.PullRequest) == 0 {
		return issueRun(*target, "command on issue")
	}
	pr, err := lookupPR(target.Number)
	if err != nil {
		return Decision{}, err
	}
	repo := os.Getenv("GITHUB_REPOSITORY")
	switch {
	case pr.State != "open":
		return skip("PR is not open"), nil
	case pr.Head.Repo == nil || pr.Head.Repo.FullName != repo:
		return skip("PR head is in a fork; CodeAF only pushes to branches in " + repo), nil
	}
	return Decision{Run: true, Reason: "command on PR", Mode: "pr", Number: pr.Number, Branch: pr.Head.Ref, Base: pr.Base.Ref}, nil
}

func runGate() error {
	ev, err := readEvent()
	if err != nil {
		return err
	}
	// GITHUB_RUN_ID names this run; refuse to go on without a valid one.
	if _, err := runID(); err != nil {
		return err
	}
	gh := newGitHub()
	decision, err := Decide(os.Getenv("GITHUB_EVENT_NAME"), ev, envOr("CODEAF_LABEL", "codeaf"), func(n int) (item, error) {
		var pr item
		return pr, gh.get(fmt.Sprintf("/pulls/%d", n), &pr)
	})
	if err != nil {
		return err
	}
	outputs := fmt.Sprintf("run=%t\nreason=%s\n", decision.Run, decision.Reason)
	if decision.Run {
		outputs += fmt.Sprintf("mode=%s\nnumber=%d\nbranch=%s\nbase=%s\n", decision.Mode, decision.Number, decision.Branch, decision.Base)
	}
	fmt.Print(outputs)
	if path := os.Getenv("GITHUB_OUTPUT"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(outputs)
		return err
	}
	return nil
}

type issueComment struct {
	Body              string `json:"body"`
	AuthorAssociation string `json:"author_association"`
	User              user   `json:"user"`
}

type reviewComment struct {
	Path         string `json:"path"`
	Line         *int   `json:"line"`
	OriginalLine *int   `json:"original_line"`
	Body         string `json:"body"`
	User         user   `json:"user"`
}

const reportMarker = "<!-- codeaf-report -->"

func lastN[T any](items []T, n int) []T {
	if len(items) > n {
		return items[len(items)-n:]
	}
	return items
}

func (g github) thread(number int) (string, error) {
	var comments []issueComment
	if err := g.get(fmt.Sprintf("/issues/%d/comments?per_page=100", number), &comments); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range lastN(comments, 30) {
		body := c.Body
		if c.User.Type == "Bot" && strings.Contains(body, reportMarker) {
			body = "(CodeAF run report omitted)"
		}
		fmt.Fprintf(&b, "**@%s** (%s):\n%s\n\n", c.User.Login, c.AuthorAssociation, body)
	}
	if b.Len() == 0 {
		return "(no comments)", nil
	}
	return b.String(), nil
}

func runBrief() error {
	number, err := strconv.Atoi(os.Getenv("NUMBER"))
	if err != nil {
		return fmt.Errorf("NUMBER: %w", err)
	}
	ev, err := readEvent()
	if err != nil {
		return err
	}
	gh := newGitHub()
	standing, err := os.ReadFile(filepath.Join(harnessDir(), "instructions.md"))
	if err != nil {
		return err
	}
	parts := []string{string(standing)}

	if os.Getenv("MODE") == "issue" {
		var issue item
		if err := gh.get(fmt.Sprintf("/issues/%d", number), &issue); err != nil {
			return err
		}
		parts = append(parts, fmt.Sprintf("# Task: issue #%d — %s\n\n%s", number, issue.Title, issue.Body))
		if issue.Comments > 0 {
			thread, err := gh.thread(number)
			if err != nil {
				return err
			}
			parts = append(parts, "## Discussion on the issue\n\n"+thread)
		}
	} else {
		var pr item
		if err := gh.get(fmt.Sprintf("/pulls/%d", number), &pr); err != nil {
			return err
		}
		parts = append(parts, fmt.Sprintf("# Follow-up on pull request #%d — %s\n\nYou are continuing work on an open pull request. "+
			"The working tree is its head branch `%s`. Address the request below; do not redo work that is already on the branch.\n\n"+
			"## PR description\n\n%s", number, pr.Title, pr.Head.Ref, pr.Body))
		var reviews []reviewComment
		if err := gh.get(fmt.Sprintf("/pulls/%d/comments?per_page=100", number), &reviews); err != nil {
			return err
		}
		if len(reviews) > 0 {
			var b strings.Builder
			b.WriteString("## Inline review comments\n\n")
			for _, c := range lastN(reviews, 40) {
				line := c.Line
				if line == nil {
					line = c.OriginalLine
				}
				where := "?"
				if line != nil {
					where = strconv.Itoa(*line)
				}
				fmt.Fprintf(&b, "- `%s` line %s, @%s: %s\n", c.Path, where, c.User.Login, c.Body)
			}
			parts = append(parts, b.String())
		}
		thread, err := gh.thread(number)
		if err != nil {
			return err
		}
		parts = append(parts, "## Conversation\n\n"+thread)
	}

	if trigger := ev.trigger(); trigger != nil {
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(trigger.Body), command))
		if text == "" {
			text = "(no text: continue / fix what the reviews ask)"
		}
		parts = append(parts, fmt.Sprintf("## The request you are acting on now (from @%s)\n\n%s", trigger.User.Login, text))
	}
	brief := strings.Join(parts, "\n\n") + "\n"
	fmt.Print(brief)
	return os.WriteFile(os.Getenv("BRIEF_PATH"), []byte(brief), 0o644)
}

// ---------------------------------------------------------------- results

// Result is the part of `codeaf do --json` the report reads.
type Result struct {
	Stop       string  `json:"stop"`
	OK         *bool   `json:"ok"`
	Verdict    string  `json:"verdict"`
	Error      any     `json:"error"`
	Model      string  `json:"model"`
	PlanModel  string  `json:"plan_model"`
	CheckModel string  `json:"check_model"`
	SpendUSD   float64 `json:"spend_usd"`
	EstUSD     float64 `json:"est_usd"`
	Answer     string  `json:"answer"`
}

func readResult() (Result, error) {
	var result Result
	data, err := os.ReadFile(os.Getenv("RESULT_PATH"))
	if err != nil {
		return result, err
	}
	return result, json.Unmarshal(data, &result)
}

func runSummary() error {
	data, err := os.ReadFile(os.Getenv("RESULT_PATH"))
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		fmt.Printf("%.4000s\n", data) // not JSON: show what CodeAF printed
		return nil
	}
	delete(fields, "answer")
	pretty, _ := json.MarshalIndent(fields, "", " ")
	fmt.Printf("%.4000s\n", pretty)
	return nil
}

func readLedger(path string) ([]LedgerLine, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []LedgerLine
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var line LedgerLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		lines = append(lines, line)
	}
	return lines, scanner.Err()
}

func ledgerTotal(lines []LedgerLine) float64 {
	total := 0.0
	for _, l := range lines {
		total += l.CostUSD
	}
	return total
}

// spentToday reads the day's earlier spend, which comes from the shared state
// bundle. That total is built from numbers run jobs reported, so it is treated
// as untrusted: a negative, infinite or unparseable total would loosen the
// daily cap, and is refused rather than repaired.
func spentToday(name string) (float64, error) {
	value, err := envFloat(name, 0)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, fmt.Errorf("%s: %v is not a valid spend total", name, value)
	}
	return value, nil
}

// meteredRunSpend asks the running proxy what this run cost. The proxy keeps
// the total in memory, out of reach of the agent's file writes.
func meteredRunSpend() (float64, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + envOr("PROXY_PORT", "8787") + "/healthz")
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	var health struct {
		RunSpentUSD *float64 `json:"run_spent_usd"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		return 0, err
	}
	if health.RunSpentUSD == nil {
		return 0, errors.New("proxy health has no run_spent_usd")
	}
	return *health.RunSpentUSD, nil
}

// ---------------------------------------------------------------- router ledger

// RouterEntry is one rating in CodeAF's router-ledger.json (internal/router).
type RouterEntry struct {
	Model   string    `json:"model"`
	Class   string    `json:"class"`
	Rating  float64   `json:"rating"`
	Count   int       `json:"count"`
	Updated time.Time `json:"updated"`
}

// RouterLedger is CodeAF's router-ledger.json.
type RouterLedger struct {
	Entries []RouterEntry     `json:"entries"`
	Aliases map[string]string `json:"aliases,omitempty"`
}

const (
	maxLedgerBytes   = 1 << 20
	maxLedgerEntries = 256
	maxRating        = 20.0
	maxCount         = 1_000_000
)

func validClass(class string) bool {
	if len(class) > 32 {
		return false
	}
	for _, r := range class {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// SanitizeLedger rebuilds a router ledger from fields that can only carry
// numbers about allowed models. The file is carried between runs in the shared
// state bundle and the agent can write to it, so nothing else survives: no
// unknown model ids, no free text, no out-of-range numbers.
func SanitizeLedger(data []byte, models []Model) (RouterLedger, int) {
	allowed := map[string]bool{}
	for _, m := range models {
		allowed[m.ID] = true
	}
	var in RouterLedger
	clean := RouterLedger{Entries: []RouterEntry{}}
	if len(data) > maxLedgerBytes || json.Unmarshal(data, &in) != nil {
		return clean, len(in.Entries) + len(in.Aliases) + 1
	}
	dropped := 0
	seen := map[string]bool{}
	for _, e := range in.Entries {
		key := e.Model + "\x00" + e.Class
		if !allowed[e.Model] || !validClass(e.Class) || seen[key] || len(clean.Entries) >= maxLedgerEntries ||
			math.IsNaN(e.Rating) || math.IsInf(e.Rating, 0) || math.Abs(e.Rating) > maxRating ||
			e.Count < 0 || e.Count > maxCount {
			dropped++
			continue
		}
		seen[key] = true
		clean.Entries = append(clean.Entries, RouterEntry{Model: e.Model, Class: e.Class, Rating: e.Rating, Count: e.Count, Updated: e.Updated.UTC()})
	}
	for from, to := range in.Aliases {
		if !allowed[from] || !allowed[to] {
			dropped++
			continue
		}
		if clean.Aliases == nil {
			clean.Aliases = map[string]string{}
		}
		clean.Aliases[from] = to
	}
	return clean, dropped
}

func money(x float64) string { return fmt.Sprintf("$%.4f", x) }

func trimNumber(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

type modelTotals struct {
	calls, in, cached, out, unpriced int
	cost                             float64
}

// Report renders the run report. It is a pure function of its inputs so it can
// be tested; runReport gathers them.
func Report(result Result, haveResult bool, lines []LedgerLine, cfg Config, env func(string) string) string {
	models := cfg.Models
	totals := map[string]*modelTotals{}
	for _, l := range lines {
		t := totals[l.Model]
		if t == nil {
			t = &modelTotals{}
			totals[l.Model] = t
		}
		t.calls++
		t.in += l.PromptTokens
		t.cached += l.CachedTokens
		t.out += l.CompletionTokens
		t.cost += l.CostUSD
		if !l.UsageSeen {
			t.unpriced++
		}
	}
	total := ledgerTotal(lines)
	dayBefore, _ := strconv.ParseFloat(env("DAY_SPENT_BEFORE"), 64)
	orUnknown := func(s string) string {
		if s == "" {
			return "?"
		}
		return s
	}
	outcome := env("OUTCOME")
	if outcome == "" {
		outcome = "finished"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n### CodeAF run — %s\n\n| | |\n|---|---|\n", reportMarker, outcome)
	if haveResult {
		ok := "?"
		if result.OK != nil {
			ok = strconv.FormatBool(*result.OK)
		}
		fmt.Fprintf(&b, "| stop / ok | `%s` / `%s` |\n", orUnknown(result.Stop), ok)
		if result.Verdict != "" {
			fmt.Fprintf(&b, "| verdict | `%s` |\n", result.Verdict)
		}
		if result.Error != nil && result.Error != "" {
			message := strings.ReplaceAll(fmt.Sprint(result.Error), "\n", " ")
			fmt.Fprintf(&b, "| error | %.300s |\n", message)
		}
	} else {
		b.WriteString("| result | CodeAF produced no JSON result; see the workflow logs |\n")
	}
	allowed := make([]string, len(models))
	for i, m := range models {
		allowed[i] = "`" + m.ID + "`"
	}
	fmt.Fprintf(&b, "| seats | worker `%s`, planner `%s`, checker `%s` (allowed: %s; no escalation) |\n",
		orUnknown(result.Model), orUnknown(result.PlanModel), orUnknown(result.CheckModel), strings.Join(allowed, ", "))
	fmt.Fprintf(&b, "| CodeAF's own spend figure | %s (plan estimate %s) |\n", money(result.SpendUSD), money(result.EstUSD))
	fmt.Fprintf(&b, "| metered by proxy (from its ledger) | **%s** this run · %s today (cap $%s/run, $%s/day) |\n",
		money(total), money(dayBefore+total), trimNumber(cfg.Limits.RunUSD), trimNumber(cfg.Limits.DayUSD))
	fmt.Fprintf(&b, "| files changed | %s |\n\n", orUnknown(env("CHANGED")))

	if len(totals) > 0 {
		prices := map[string]Model{}
		for _, m := range models {
			prices[m.ID] = m
		}
		ids := make([]string, 0, len(totals))
		for id := range totals {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return totals[ids[i]].cost > totals[ids[j]].cost })
		b.WriteString("| model | $/M in · cached · out | calls | input tok | cached tok | output tok | cost |\n")
		b.WriteString("|---|---|---:|---:|---:|---:|---:|\n")
		for _, id := range ids {
			t, p := totals[id], prices[id]
			cost := money(t.cost)
			if t.unpriced > 0 {
				cost += fmt.Sprintf(" (+%d unpriced)", t.unpriced)
			}
			fmt.Fprintf(&b, "| `%s` | %s · %s · %s | %d | %d | %d | %d | %s |\n", id,
				trimNumber(p.Input), trimNumber(p.CacheRead), trimNumber(p.Output), t.calls, t.in, t.cached, t.out, cost)
		}
		b.WriteString("\n")
	}
	if result.Answer != "" {
		answer := result.Answer
		if len(answer) > 6000 {
			answer = answer[:6000] + "…"
		}
		fmt.Fprintf(&b, "<details><summary>CodeAF's summary</summary>\n\n%s\n\n</details>\n\n", answer)
	}
	server := env("GITHUB_SERVER_URL")
	if server == "" {
		server = "https://github.com"
	}
	labelName := env("CODEAF_LABEL")
	if labelName == "" {
		labelName = "codeaf"
	}
	fmt.Fprintf(&b, "[Workflow run](%s/%s/actions/runs/%s) · reply `%s <instructions>` on a `%s`-labelled issue/PR to run again.\n",
		server, env("GITHUB_REPOSITORY"), env("GITHUB_RUN_ID"), command, labelName)
	return b.String()
}

func runReport() error {
	result, err := readResult()
	haveResult := err == nil
	if err != nil {
		log.Printf("no CodeAF result: %v", err)
	}
	lines, err := readLedger(os.Getenv("LEDGER_PATH"))
	if err != nil {
		return err
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		return err
	}
	fmt.Print(Report(result, haveResult, lines, cfg, os.Getenv))
	return nil
}
