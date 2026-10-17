// Command harness is the glue between GitHub Actions, CodeAF and synthetic.new.
// It is standard library only, and built fresh by the workflow on every run.
//
//	harness proxy    serve CodeAF on 127.0.0.1: an OpenRouter-shaped model catalog
//	                 at synthetic.new's prices, and /chat/completions forwarded
//	                 upstream with usage.cost added and spend caps enforced
//	harness gate     decide whether this GitHub event may run CodeAF; writes step
//	                 outputs (run, reason, mode, number, branch, base)
//	harness next     pick the queued issue CodeAF should work next and print
//	                 its number and lane, or nothing; the queue job labels it
//	                 to start
//	harness seat-config  print the CodeAF config for one lane: the worker seat
//	                 and task.parallel for that run's slice of the models
//	harness brief    write the task brief for `codeaf do` to $BRIEF_PATH
//	harness summary  print CodeAF's JSON result without its long answer
//	harness state    the state shared between runs (state.go): load the shared
//	                 bundle into $CODEAF_HOME, pack what a run left, merge packed
//	                 runs into a new bundle
//	harness report   print the Markdown run report posted on the issue/PR
//	harness review-brief   write the independent review's brief for `codeaf do`
//	                       to $BRIEF_PATH (issue #32)
//	harness review-config  print the CodeAF config for the review run, with
//	                       the worker tier set to the reviewer model
//	harness review-report  print the review's step outputs and write its
//	                       comment body: fix run, pass label, or report only
//	harness ref-check      decide what the weekly upstream-pin check does:
//	                       create, update or close the one issue tracking
//	                       commits past .github/codeaf/codeaf-ref (issue #35)
//	harness merge-default  bring a PR base branch in (issue #57): the run job
//	                       merges it into the branch before CodeAF works (a
//	                       conflict is committed with markers and named in
//	                       the brief); publish recreates that merge from the
//	                       same two parents, checked by tree hash, and
//	                       refuses a result that still carries conflict
//	                       markers
//
// Trust model: the `codeaf` label can only be added by people with triage access
// or above, and /codeaf commands are obeyed only from OWNER/MEMBER/COLLABORATOR.
// Issue and PR text is the task for CodeAF and never reaches a shell.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("harness: ")
	if len(os.Args) < 2 {
		log.Fatal("usage: harness proxy|gate|next|seat-config|brief|summary|state|report|pr-mode|continue-plan|review-brief|review-config|review-report|plan-check|plan-brief|plan-report|ref-check|merge-default|search")
	}
	commands := map[string]func() error{
		"proxy":         runProxy,
		"gate":          runGate,
		"next":          runNext,
		"brief":         runBrief,
		"summary":       runSummary,
		"state":         runState,
		"report":        runReport,
		"pr-mode":       prMode,
		"continue-plan": runContinuePlan,
		"seat-config":   runSeatConfig, // the lane's CodeAF config (issue #60)
		// The independent review pass (issue #32): brief, model config and
		// report of a read-only review run on a pull request.
		"review-brief":  runReviewBrief,
		"review-config": runReviewConfig,
		"review-report": runReviewReport,
		// The planner trigger (issue #61): does the queue job dispatch a
		// plan run this tick? plan-brief and plan-report, the run side of
		// the planner, are separate commands.
		"plan-check": runPlanCheck,
		// The planner run side (issue #61): the plan run brief, and the
		// publish-side report that parses its proposals.
		"plan-brief":  runPlanBrief,
		"plan-report": runPlanReport,
		// The weekly upstream-pin check (issue #35): what the scheduled job
		// should do about commits past .github/codeaf/codeaf-ref.
		"ref-check": runRefCheck,
		// Bringing a PR base branch in (issue #57): the run job merges it
		// into the PR branch before CodeAF works (a conflict is committed
		// with markers and named in the brief); publish recreates that merge
		// from the same two parents, checked by tree hash, and refuses a
		// result that still carries conflict markers (merge.go).
		"merge-default": runMergeDefault,
		// Web search through the proxy (issue #58): the CLI posts to the
		// proxy on 127.0.0.1; the key never leaves the proxy.
		"search": runSearch,
		// The deterministic gate on a workflow change published without a
		// maintainer's approval (workflowpolicy.go).
		"workflow-policy": runWorkflowPolicy,
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
// Input/Output/CacheRead are the synthetic.new SUBSCRIPTION rates: they price
// the credit a call draws down the weekly quota. The API* fields are the API
// list rates: what the same call costs once the quota is gone and overage
// begins.
type Model struct {
	ID              string   `json:"id"`       // what CodeAF sees
	Upstream        string   `json:"upstream"` // the synthetic.new model id
	Name            string   `json:"name"`
	Role            string   `json:"role"`
	Input           float64  `json:"input"`
	Output          float64  `json:"output"`
	CacheRead       float64  `json:"cache_read"`
	APIInput        float64  `json:"api_input"`
	APICached       float64  `json:"api_cached"`
	APIOutput       float64  `json:"api_output"`
	ContextLength   int      `json:"context_length"`
	InputModalities []string `json:"input_modalities"`
	Efforts         []string `json:"efforts"`
	// MaxConcurrent is how many calls to this model may be in flight at
	// once; 0 is no limit. The synthetic.new subscription allows 2 per
	// model, and the proxy queues calls past that rather than sending them.
	MaxConcurrent int `json:"max_concurrent"`
}

// Limits are the Big-plan spend caps the proxy enforces, in USD of credit
// (subscription-rate dollars) unless said otherwise. The weekly credit is
// shared by every run of the week and arrives per run through the shared
// state; OverageUSD prices extra usage past it at the API list rates.
type Limits struct {
	Plan            string  `json:"plan"`              // the synthetic.new plan the caps assume
	PlanNote        string  `json:"plan_note"`         // where the numbers came from
	WeeklyCreditUSD float64 `json:"weekly_credit_usd"` // per rolling week, across runs
	RunCreditUSD    float64 `json:"run_credit_usd"`    // per workflow run
	OverageUSD      float64 `json:"overage_usd"`       // API-price USD past the weekly credit; 0 = stop
	// MinCreditPercent is the floor on synthetic.new's own
	// weeklyTokenLimit.percentRemaining below which, with overage 0, the
	// proxy refuses calls. 0 means the default, 10.
	MinCreditPercent float64 `json:"min_credit_percent"`
	// SearchesPerRun caps how many web searches (POST /v2/search, issue #58)
	// one run may forward. Search is in the plan and draws no credit, but a
	// runaway loop still needs a wall; <=0 means defaultSearchesPerRun.
	SearchesPerRun int `json:"searches_per_run"`
}

// defaultMinCreditPercent is the floor used when min_credit_percent is unset.
const defaultMinCreditPercent = 10

// Queue is how many CodeAF runs the queue may have going at once. Every
// concurrent run gets its own slice of each model's account-wide
// max_concurrent (perRunSlots) and its own worker seat (its lane).
type Queue struct {
	MaxParallelRuns int `json:"max_parallel_runs"`
}

// Planner is when the queue dispatches a plan run (issue #61): the queue
// calls `harness plan-check` after its start-issues loop, and it fires only
// while the open queued backlog is under MinBacklog and no plan run has
// started within the last MinHours. A plan run itself proposes at most
// MaxProposals issues (read by plan-brief, the run-side of the planner).
type Planner struct {
	MinBacklog   int `json:"min_backlog"`   // queued issues below which a plan run fires
	MinHours     int `json:"min_hours"`     // hours since the last plan run before another
	MaxProposals int `json:"max_proposals"` // proposals one plan run may open
	// RetryMinHours is planner.retry_min_hours: the shorter cooldown after a
	// plan run that opened no proposals (issue #119). Such a run proposed
	// nothing, so waiting out the full min_hours only delays the retry.
	RetryMinHours int `json:"retry_min_hours"`
}

// Config is models.json: the spend caps and the models CodeAF may use.
type Config struct {
	Limits  Limits  `json:"limits"`
	Queue   Queue   `json:"queue"`
	Planner Planner `json:"planner"`
	Models  []Model `json:"models"`
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
	if cfg.Limits.Plan == "" {
		return cfg, fmt.Errorf("%s: limits.plan must be set", path)
	}
	if cfg.Limits.RunCreditUSD <= 0 || cfg.Limits.WeeklyCreditUSD <= 0 {
		return cfg, fmt.Errorf("%s: limits.run_credit_usd and limits.weekly_credit_usd must be positive", path)
	}
	if cfg.Limits.OverageUSD < 0 {
		return cfg, fmt.Errorf("%s: limits.overage_usd must not be negative", path)
	}
	if cfg.Limits.MinCreditPercent < 0 || cfg.Limits.MinCreditPercent >= 100 {
		return cfg, fmt.Errorf("%s: limits.min_credit_percent must be in [0, 100)", path)
	}
	if cfg.Limits.MinCreditPercent == 0 {
		cfg.Limits.MinCreditPercent = defaultMinCreditPercent
	}
	// The queue defaults to two parallel runs; a negative value is a
	// mistake, not a wish to run none.
	switch {
	case cfg.Queue.MaxParallelRuns == 0:
		cfg.Queue.MaxParallelRuns = 2
	case cfg.Queue.MaxParallelRuns < 0:
		return cfg, fmt.Errorf("%s: queue.max_parallel_runs must not be negative", path)
	}
	// The planner (issue #61) defaults to firing under a backlog of three
	// queued issues, at most once every twelve hours, and to five proposals
	// per plan run; a negative value is a mistake, not a wish.
	switch {
	case cfg.Planner.MinBacklog == 0:
		cfg.Planner.MinBacklog = 3
	case cfg.Planner.MinBacklog < 0:
		return cfg, fmt.Errorf("%s: planner.min_backlog must not be negative", path)
	}
	switch {
	case cfg.Planner.MinHours == 0:
		cfg.Planner.MinHours = 12
	case cfg.Planner.MinHours < 0:
		return cfg, fmt.Errorf("%s: planner.min_hours must not be negative", path)
	}
	switch {
	case cfg.Planner.MaxProposals == 0:
		cfg.Planner.MaxProposals = 5
	case cfg.Planner.MaxProposals < 0:
		return cfg, fmt.Errorf("%s: planner.max_proposals must not be negative", path)
	}
	switch {
	case cfg.Planner.RetryMinHours == 0:
		cfg.Planner.RetryMinHours = 2
	case cfg.Planner.RetryMinHours < 0:
		return cfg, fmt.Errorf("%s: planner.retry_min_hours must not be negative", path)
	}
	if len(cfg.Models) == 0 {
		return cfg, fmt.Errorf("%s: no models", path)
	}
	for _, m := range cfg.Models {
		if m.ID == "" || m.Upstream == "" || len(m.Efforts) == 0 {
			return cfg, fmt.Errorf("%s: model %q needs id, upstream and efforts", path, m.ID)
		}
		if m.MaxConcurrent < 0 {
			return cfg, fmt.Errorf("%s: model %q: max_concurrent must not be negative", path, m.ID)
		}
		// Each concurrent run gets a slice of the model's account-wide
		// limit, so the runs' slices must not sum past it.
		if m.MaxConcurrent > 0 && perRunSlots(m, cfg.Queue.MaxParallelRuns)*cfg.Queue.MaxParallelRuns > m.MaxConcurrent {
			return cfg, fmt.Errorf("%s: model %q: max_parallel_runs %d: per-run slots would sum past the account limit (%d x %d > max_concurrent %d)",
				path, m.ID, cfg.Queue.MaxParallelRuns, perRunSlots(m, cfg.Queue.MaxParallelRuns), cfg.Queue.MaxParallelRuns, m.MaxConcurrent)
		}
	}
	return cfg, nil
}

// perRunSlots is one run's slice of a model's account-wide max_concurrent:
// with max_parallel_runs runs possible at once, each may hold at most
// max(1, max_concurrent/max_parallel_runs) calls to the model, so the runs
// together never put more than max_concurrent calls on it.
func perRunSlots(m Model, maxParallelRuns int) int {
	if maxParallelRuns < 1 {
		maxParallelRuns = 1
	}
	return max(1, m.MaxConcurrent/maxParallelRuns)
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

// reasoningTokens reads completion_tokens_details.reasoning_tokens out of the
// raw usage block, if upstream sent it. The details stay raw on the response,
// so CodeAF sees exactly what upstream sent.
func (u Usage) reasoningTokens() (int, bool) {
	if len(u.CompletionTokensDetails) == 0 {
		return 0, false
	}
	var details struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	}
	if err := json.Unmarshal(u.CompletionTokensDetails, &details); err != nil || details.ReasoningTokens == nil {
		return 0, false
	}
	return *details.ReasoningTokens, true
}

// CostOf prices one call: uncached input, cached input and output (which
// includes reasoning tokens) at the model's per-million SUBSCRIPTION rates.
// This is the credit the call draws down the weekly quota, and the cost the
// ledger and the report carry.
func CostOf(m Model, u Usage) float64 {
	cached := u.cached()
	return (float64(u.PromptTokens-cached)*m.Input + float64(cached)*m.CacheRead + float64(u.CompletionTokens)*m.Output) / 1e6
}

// ApiCostOf prices the same call at the model's API list rates (api_*): what
// it costs once the weekly credit is exhausted and overage begins. Same
// cached-token split as CostOf.
func ApiCostOf(m Model, u Usage) float64 {
	cached := u.cached()
	return (float64(u.PromptTokens-cached)*m.APIInput + float64(cached)*m.APICached + float64(u.CompletionTokens)*m.APIOutput) / 1e6
}

// ---------------------------------------------------------------- proxy

type proxy struct {
	upstream   string
	apiKey     string
	ledger     string
	runCap     float64 // per-run credit cap (limits.run_credit_usd)
	weekCap    float64 // weekly credit cap, shared across runs (limits.weekly_credit_usd)
	weekSpent  float64 // credit spent earlier in the week, from the shared state
	overageCap float64 // API-price USD allowed past the weekly credit (limits.overage_usd)
	models     []Model
	client     *http.Client
	slots      map[string]chan struct{} // per upstream model, for max_concurrent
	// Issue #90: how long a streaming client may hear nothing before the
	// proxy proves the path with an SSE comment line; 0 means the 10s
	// default (shorter in tests).
	heartbeatEvery time.Duration
	// Web search (issue #58): where forwarded searches go and this run's
	// cap; searches counts this run's forwarded attempts under p.mu.
	searchURL      string
	searchesPerRun int

	mu           sync.Mutex
	runSpent     float64 // credit spent by this run
	overage      bool    // the weekly credit is exhausted; new calls are billed at API prices
	overageApi   float64 // API-price USD recorded past the weekly credit
	searches     int     // web searches forwarded this run (issue #58); capped by searchesPerRun
	quota        quota   // GET /v2/quotas at startup and every few minutes; an unknown quota never blocks
	minCreditPct float64 // floor on quota.PercentRemaining (limits.min_credit_percent)
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
	p.models = cfg.Models
	p.runCap = cfg.Limits.RunCreditUSD
	p.weekCap = cfg.Limits.WeeklyCreditUSD
	p.overageCap = cfg.Limits.OverageUSD
	p.minCreditPct = cfg.Limits.MinCreditPercent
	p.slots = newSlots(p.models, cfg.Queue.MaxParallelRuns)
	// Web search (issue #58): the endpoint to forward to and the per-run cap
	// from models.json (<=0 means defaultSearchesPerRun in the handler).
	p.searchURL = envOr("PROXY_SEARCH_URL", defaultSearchURL)
	p.searchesPerRun = cfg.Limits.SearchesPerRun
	if p.weekSpent, err = spentWeek("PROXY_WEEK_SPENT_USD"); err != nil {
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
	log.Printf("proxy on %s -> %s (run credit cap $%.2f, weekly credit cap $%.2f, week spent $%.4f, overage cap $%.2f)", addr, p.upstream, p.runCap, p.weekCap, p.weekSpent, p.overageCap)
	// One marker line at the top of the ledger; the report times the CodeAF
	// session from it to the last call's end.
	p.recordMarker("proxy_start")
	// Ask synthetic.new for the subscription state at start (issue #59
	// step 3), then every few minutes: several runs share one weekly credit,
	// so another run can use it up while this one works. /v2/quotas calls
	// don't count against the plan. Best-effort: never breaks the run.
	p.fetchQuotasStartup()
	go p.refreshQuotas(quotasRefreshEvery)
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
		spent, week, over := p.runSpent, p.weekSpent+p.runSpent, p.overageApi
		p.mu.Unlock()
		// run_spent_usd keeps its name (meteredRunSpend reads it) and now
		// carries credit: subscription-rate dollars.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "run_spent_usd": spent,
			"week_spent_credit_usd": week, "overage_api_usd": over})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/chat/completions"):
		p.chat(w, r)
	case r.Method == http.MethodPost && path == "/v2/search":
		// Web search (issue #58): the exact path only; the key stays with
		// the proxy (search.go).
		p.search(w, r)
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

// maxRepairBytes bounds the size of the messages array repairToolArgs will
// walk, so the repair cannot become a CPU sink on a very large request. It
// is several times the largest context these models accept.
const maxRepairBytes = 4 << 20

// repairToolArgs walks a chat request's messages and repairs assistant
// tool calls whose function.arguments is not valid JSON, replacing each with
// {"_invalid_arguments": "<original text>"}. synthetic.new validates the
// whole history and refuses a request that carries a malformed call, so one
// bad call would end every later request of the run (issue #50); wrapped
// this way the model still sees what it sent. Valid arguments are left
// byte-for-byte unchanged, and messages with nothing to repair keep their
// original bytes. It returns how many arguments were repaired. Requests
// whose messages are over maxRepairBytes are skipped. A repaired message is
// re-encoded, so its object keys come out sorted; the values are untouched.
func repairToolArgs(body map[string]json.RawMessage) int {
	raw, ok := body["messages"]
	if !ok || len(raw) > maxRepairBytes {
		return 0
	}
	var messages []json.RawMessage
	if json.Unmarshal(raw, &messages) != nil {
		return 0
	}
	repaired := 0
	for i, rawMsg := range messages {
		var msg map[string]json.RawMessage
		if json.Unmarshal(rawMsg, &msg) != nil {
			continue
		}
		var role string
		if json.Unmarshal(msg["role"], &role) != nil || role != "assistant" {
			continue
		}
		var calls []json.RawMessage
		if json.Unmarshal(msg["tool_calls"], &calls) != nil {
			continue
		}
		changed, fixed := false, 0
		for j, rawCall := range calls {
			var call map[string]json.RawMessage
			if json.Unmarshal(rawCall, &call) != nil {
				continue
			}
			var fn map[string]json.RawMessage
			if json.Unmarshal(call["function"], &fn) != nil {
				continue
			}
			// Unmarshalling a JSON null leaves args empty and reports no
			// error; empty text is not valid JSON either, so an empty (or
			// null) argument is repaired and keeps its original, empty text.
			var args string
			if json.Unmarshal(fn["arguments"], &args) != nil || json.Valid([]byte(args)) {
				continue
			}
			wrapped, err := json.Marshal(map[string]string{"_invalid_arguments": args})
			if err != nil {
				continue
			}
			// Back as a JSON string, the shape the field already had.
			if fn["arguments"], err = json.Marshal(string(wrapped)); err != nil {
				continue
			}
			if call["function"], err = json.Marshal(fn); err != nil {
				continue
			}
			if calls[j], err = json.Marshal(call); err != nil {
				continue
			}
			changed, fixed = true, fixed+1
		}
		if !changed {
			continue
		}
		msg["tool_calls"], _ = json.Marshal(calls)
		repairedMsg, err := json.Marshal(msg)
		if err != nil {
			continue // the original message is kept
		}
		messages[i] = repairedMsg
		repaired += fixed
	}
	if repaired > 0 {
		body["messages"], _ = json.Marshal(messages)
	}
	return repaired
}

// charCounts tallies the characters one call produced, split into the
// reasoning text and the visible output. GLM streams its reasoning as
// reasoning_content and reports no reasoning_tokens (issue #20), so the
// report estimates those models' reasoning share from these counts. len()
// counts UTF-8 bytes rather than runes; for a share estimate the proportion
// is the same, and counting bytes is free.
type charCounts struct {
	reasoning int
	output    int
}

// addPart reads reasoning_content, content and tool-call arguments out of one
// already-decoded delta (streamed) or message (JSON) object and adds their
// lengths. Missing keys and JSON nulls add nothing. Streamed tool-call
// arguments arrive in fragments, whose lengths add up to the whole.
func (c *charCounts) addPart(part map[string]json.RawMessage) {
	if part == nil {
		return
	}
	// A fresh string per field: unmarshalling a JSON null leaves the target
	// untouched, so a reused variable would carry the previous field's text
	// (a null content would re-count the reasoning) into the wrong bucket.
	var reasoning string
	if json.Unmarshal(part["reasoning_content"], &reasoning) == nil {
		c.reasoning += len(reasoning)
	}
	var content string
	if json.Unmarshal(part["content"], &content) == nil {
		c.output += len(content)
	}
	var calls []struct {
		Function struct {
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(part["tool_calls"], &calls) == nil {
		for _, call := range calls {
			c.output += len(call.Function.Arguments)
		}
	}
}

// addChoices walks the choices array of an already-decoded response and adds
// each choice's part ("delta" when streamed, "message" in a JSON response).
func (c *charCounts) addChoices(choices []map[string]json.RawMessage, part string) {
	for _, choice := range choices {
		if choice == nil {
			continue
		}
		var p map[string]json.RawMessage
		if json.Unmarshal(choice[part], &p) == nil {
			c.addPart(p)
		}
	}
}

// LedgerLine is one call, as appended to the ledger and read by report.
// Kind marks lines that are not calls: the proxy writes one "proxy_start"
// marker when it starts, which the report times the session from.
type LedgerLine struct {
	Time             time.Time `json:"ts"`
	Model            string    `json:"model"`
	Upstream         string    `json:"upstream"`
	PromptTokens     int       `json:"prompt_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	// ReasoningTokens is completion_tokens_details.reasoning_tokens. A nil
	// means upstream did not report it; a known zero records 0.
	ReasoningTokens *int `json:"reasoning_tokens,omitempty"`
	// ReasoningChars and OutputChars count the characters of reasoning_content
	// and of the visible content plus tool-call arguments. GLM sends no
	// reasoning_tokens (issue #20), so the report estimates its reasoning
	// share from these counts. Both stay 0 when the call produced no text.
	ReasoningChars int `json:"reasoning_chars,omitempty"`
	OutputChars    int `json:"output_chars,omitempty"`
	// RepairedToolArgs is how many of this request's assistant tool-call
	// arguments were not valid JSON and were replaced with valid JSON that
	// keeps the original text (issue #50); 0 on the usual call.
	RepairedToolArgs int     `json:"repaired_tool_args,omitempty"`
	CostUSD          float64 `json:"cost_usd"`
	// APICostUSD is the same call priced at the model's API list rates: what
	// it costs once the weekly credit is exhausted and overage begins. A line
	// written before the field existed parses back as 0 (json.Unmarshal of a
	// missing field), and omitempty keeps the common zero off the wire.
	APICostUSD float64 `json:"api_cost_usd,omitempty"`
	Stream     bool    `json:"stream"`
	UsageSeen  bool    `json:"usage_seen"`
	// Start is when the upstream request was issued, after any wait for a
	// slot; ts is when the call ended. SlotWaitS is how long the call queued
	// for a model slot. Old ledgers carry neither.
	Start     time.Time `json:"start,omitzero"`
	SlotWaitS float64   `json:"slot_wait_s,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	// Quotas carries the raw GET /v2/quotas response body on the "quotas"
	// marker line, capped (recordQuotas); the report re-parses it from here.
	Quotas string `json:"quotas,omitempty"`
	// Search lines (kind "search", issue #58) carry the query's length in
	// runes and how many results came back — never the query text, which
	// must not land in the ledger.
	QueryLen int `json:"query_len,omitempty"`
	Results  int `json:"results,omitempty"`
}

func (p *proxy) record(m Model, usage *Usage, stream bool, start time.Time, slotWait time.Duration, chars charCounts, repaired int) {
	line := LedgerLine{Time: time.Now().UTC(), Model: m.ID, Upstream: m.Upstream, Stream: stream,
		Start: start.UTC(), SlotWaitS: slotWait.Seconds(),
		ReasoningChars: chars.reasoning, OutputChars: chars.output, RepairedToolArgs: repaired}
	if usage != nil {
		line.UsageSeen = true
		line.PromptTokens, line.CachedTokens, line.CompletionTokens = usage.PromptTokens, usage.cached(), usage.CompletionTokens
		line.CostUSD = usage.Cost
		line.APICostUSD = ApiCostOf(m, *usage)
		if n, ok := usage.reasoningTokens(); ok {
			line.ReasoningTokens = &n
		}
	} else {
		log.Printf("warning: %s returned no usage; this call is unpriced", m.Upstream)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Overage accounting: a call that started once the weekly credit was
	// already exhausted is billed at API prices, and its whole API cost
	// counts against the overage cap — usage is not split mid-call. The
	// approximation: a call still in flight when the credit runs out is
	// recorded as credit, and a call recorded just after another call
	// crossed the line counts as overage in full, even if it started
	// seconds earlier. One run is short; that error is small.
	if p.overage {
		p.overageApi += line.APICostUSD
	}
	p.runSpent += line.CostUSD
	if p.weekSpent+p.runSpent >= p.weekCap {
		p.overage = true
	}
	p.appendLedger(line)
}

// recordMarker appends one non-call line, such as the proxy_start marker the
// report times the session from. Markers carry only their kind and ts.
func (p *proxy) recordMarker(kind string) {
	marker := struct {
		Kind string    `json:"kind"`
		Time time.Time `json:"ts"`
	}{Kind: kind, Time: time.Now().UTC()}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendLedger(marker)
}

// appendLedger writes one JSON line. Callers hold p.mu, which keeps the
// run-spend total and the file in step.
func (p *proxy) appendLedger(line any) {
	data, err := json.Marshal(line)
	if err != nil {
		log.Printf("ledger: %v", err)
		return
	}
	f, err := os.OpenFile(p.ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("ledger: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// ------------------------------------------------------------- quotas

const (
	// quotasMarkerCap caps the raw /v2/quotas body kept in the ledger marker
	// and in the proxy log: diagnostics, not data to serve calls from.
	quotasMarkerCap = 4 << 10
	// quotasReadCap bounds how much of the response body is read at all, so
	// a broken or hostile upstream cannot balloon the proxy's memory.
	quotasReadCap = 64 << 10
	// quotasTimeout bounds the startup fetch; the run must not wait on it.
	quotasTimeout = 15 * time.Second
)

// quota is the subscription state GET /v2/quotas reports. The response's
// units are undocumented (issue #59), so the parse is defensive: the numbers
// are believed only when both arrive as JSON numbers and are sane, and
// anything else — a body that does not parse, a missing subscription, quoted
// numbers, a negative or zero limit — leaves Known false. Nothing here may
// fail a run only because the fields are unfamiliar.
type quota struct {
	Limit    float64 // subscription.limit
	Requests float64 // subscription.requests
	RenewsAt string  // subscription.renewsAt, as the API sent it
	Known    bool    // both numbers parsed and passed the sanity checks
	// PercentRemaining is weeklyTokenLimit.percentRemaining: the share of
	// the weekly credit left, which is what actually runs out on the Big
	// plan (the subscription request counter stays at 0).
	PercentRemaining float64
	PercentKnown     bool
}

// parseQuotas parses a /v2/quotas body defensively. It returns no error, only
// Known false when the shape is not the expected one.
func parseQuotas(body []byte) quota {
	var doc struct {
		Subscription *struct {
			Limit    json.RawMessage `json:"limit"`
			Requests json.RawMessage `json:"requests"`
			RenewsAt json.RawMessage `json:"renewsAt"`
		} `json:"subscription"`
	}
	var q quota
	var weekly struct {
		WeeklyTokenLimit *struct {
			PercentRemaining json.RawMessage `json:"percentRemaining"`
		} `json:"weeklyTokenLimit"`
	}
	if json.Unmarshal(body, &weekly) == nil && weekly.WeeklyTokenLimit != nil {
		if pct, ok := jsonNumber(weekly.WeeklyTokenLimit.PercentRemaining); ok && pct >= 0 && pct <= 100 {
			q.PercentRemaining, q.PercentKnown = pct, true
		}
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Subscription == nil {
		return q
	}
	limit, okLimit := jsonNumber(doc.Subscription.Limit)
	requests, okRequests := jsonNumber(doc.Subscription.Requests)
	if !okLimit || !okRequests || limit <= 0 || requests < 0 {
		return q
	}
	q.Limit, q.Requests, q.Known = limit, requests, true
	if doc.Subscription.RenewsAt != nil {
		_ = json.Unmarshal(doc.Subscription.RenewsAt, &q.RenewsAt)
	}
	return q
}

// jsonNumber accepts exactly a JSON number; a quoted value is unfamiliar, not
// coerced, and out-of-range numbers are refused like any other.
func jsonNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// quotasURL derives the /v2/quotas endpoint from the chat upstream: the chat
// base is https://api.synthetic.new/openai/v1, and the quota endpoint lives
// at the API root, not under /openai/v1.
func quotasURL(upstream string) string {
	base := strings.TrimSuffix(strings.TrimRight(upstream, "/"), "/openai/v1")
	return base + "/v2/quotas"
}

// fetchQuotas GETs the subscription state. It returns the parsed quota and
// the raw response body (for the log and the ledger marker), and an error on
// any network or HTTP failure. The Authorization header carries the key and
// is never logged or written; the raw body never contains it. This call does
// not count against the subscription's limits.
// quotaNumberString matches a number written as a string, optionally with a
// leading dollar sign: the shape of synthetic.new's credit figures.
var quotaNumberString = regexp.MustCompile(`^\$?-?[0-9]{1,15}(\.[0-9]{1,12})?$`)

// redactQuotasBody keeps a /v2/quotas body's keys, numbers, booleans, nulls
// and RFC 3339 timestamps, and replaces every other string with
// "[redacted]". A body that is not JSON is reduced to its length.
func redactQuotasBody(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return fmt.Sprintf("[non-JSON body, %d bytes]", len(raw))
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				t[k] = walk(val)
			}
			return t
		case []any:
			for i, val := range t {
				t[i] = walk(val)
			}
			return t
		case string:
			if _, err := time.Parse(time.RFC3339Nano, t); err == nil {
				return t
			}
			// Credit figures arrive as strings ("12.34", "$12.34"); a number
			// is not personal data, and it is what calibrates the limits.
			if quotaNumberString.MatchString(t) {
				return t
			}
			return "[redacted]"
		default:
			return t
		}
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		return "[unrenderable body]"
	}
	return string(out)
}

func fetchQuotas(ctx context.Context, client *http.Client, url, key string) (quota, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return quota{}, "", err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := client.Do(request)
	if err != nil {
		return quota{}, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, quotasReadCap))
	if err != nil {
		return quota{}, string(body), err
	}
	if response.StatusCode != http.StatusOK {
		return quota{}, string(body), fmt.Errorf("status %d", response.StatusCode)
	}
	return parseQuotas(body), string(body), nil
}

// fetchQuotasStartup asks synthetic.new for the subscription state once, at
// proxy start, and keeps the parsed result for overCap. Best-effort by
// contract: any failure is logged and leaves the quota unknown, so startup
// never breaks and an unfamiliar shape never blocks a call. The raw body is
// logged (capped, no key) and appended to the ledger as the "quotas" marker
// the report reads.
func (p *proxy) fetchQuotasStartup() {
	ctx, cancel := context.WithTimeout(context.Background(), quotasTimeout)
	defer cancel()
	client := &http.Client{Timeout: quotasTimeout}
	url := quotasURL(p.upstream)
	q, raw, err := fetchQuotas(ctx, client, url, p.apiKey)
	switch {
	case err != nil:
		log.Printf("quotas: GET %s failed (%v); continuing with the quota unknown", url, err)
	case !q.Known:
		log.Printf("quotas: unfamiliar shape; continuing with the quota unknown")
	}
	if raw != "" {
		// The diagnostics and the ledger are public artifacts, so only the
		// body's shape, numbers, booleans and timestamps are kept: any other
		// string an account endpoint might return is redacted.
		redacted := redactQuotasBody(raw)
		log.Printf("quotas: body (redacted, capped): %s", capString(redacted, quotasMarkerCap))
		p.recordQuotas(redacted)
	}
	p.mu.Lock()
	p.quota = q
	p.mu.Unlock()
}

// quotasRefreshEvery is how often the proxy re-reads /v2/quotas mid-run.
const quotasRefreshEvery = 5 * time.Minute

// refreshQuotas re-reads /v2/quotas every interval and keeps the newest
// parsed result that knows the weekly credit. A failed or unfamiliar read
// keeps the previous one: a missing answer never blocks, and never unblocks.
func (p *proxy) refreshQuotas(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), quotasTimeout)
		q, _, err := fetchQuotas(ctx, &http.Client{Timeout: quotasTimeout}, quotasURL(p.upstream), p.apiKey)
		cancel()
		if err != nil || !q.PercentKnown {
			continue
		}
		log.Printf("quotas: %.1f%% of the weekly credit left", q.PercentRemaining)
		p.mu.Lock()
		p.quota = q
		p.mu.Unlock()
	}
}

// recordQuotas appends the quotas marker: kind, ts and the raw response body,
// capped. The report re-parses the body from the ledger.
func (p *proxy) recordQuotas(raw string) {
	line := struct {
		Kind   string    `json:"kind"`
		Time   time.Time `json:"ts"`
		Quotas string    `json:"quotas"`
	}{Kind: "quotas", Time: time.Now().UTC(), Quotas: capString(raw, quotasMarkerCap)}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendLedger(line)
}

// capString cuts s to at most max bytes, back to a UTF-8 rune boundary.
func capString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size != 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

func (p *proxy) overCap() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runSpent >= p.runCap {
		return fmt.Sprintf("per-run credit cap $%.2f reached ($%.4f spent)", p.runCap, p.runSpent)
	}
	// The subscription quota reported exhausted at startup (GET /v2/quotas)
	// and overage is disabled: refuse before a call is billed at API prices
	// the factory chose not to pay (issue #59 step 3). An unknown quota —
	// fetch failed or unfamiliar shape — never blocks.
	if p.quota.Known && p.quota.Limit > 0 && p.quota.Requests >= p.quota.Limit && p.overageCap <= 0 {
		return fmt.Sprintf("the subscription quota reports exhausted and overage is disabled (limit %s, used %s at proxy start)",
			trimNumber(p.quota.Limit), trimNumber(p.quota.Requests))
	}
	// synthetic.new's own count of the weekly credit, refreshed every few
	// minutes: below the floor, with overage disabled, stop before the
	// account starts billing at API prices.
	if p.quota.PercentKnown && p.overageCap <= 0 && p.quota.PercentRemaining < p.minCreditPct {
		return fmt.Sprintf("synthetic.new reports %.1f%% of the weekly credit left, under the %.0f%% floor; overage disabled, refusing", p.quota.PercentRemaining, p.minCreditPct)
	}
	if p.weekSpent+p.runSpent < p.weekCap {
		return ""
	}
	// The weekly credit is gone. Past it usage is billed at API prices, up
	// to the overage cap; at 0 (the default) the factory stops rather than
	// pay. The flag marks every later call as overage, so record() prices it
	// against the cap.
	p.overage = true
	if p.overageCap <= 0 {
		return fmt.Sprintf("weekly credit cap $%.2f reached ($%.4f spent this week); overage disabled, refusing", p.weekCap, p.weekSpent+p.runSpent)
	}
	if p.overageApi >= p.overageCap {
		return fmt.Sprintf("API overage cap $%.2f reached ($%.4f of API-priced usage past the weekly credit)", p.overageCap, p.overageApi)
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
	// Issue #90: a streaming call that cannot get a slot at once commits its
	// response — 200, text/event-stream — and heartbeats while it waits, so
	// CodeAF's watcher never reads the silence as a dead path. out stays nil
	// while the response is uncommitted and real status codes stay possible;
	// once it is set, every later failure goes out as an SSE event, because
	// the status line can no longer change.
	var out *sseSink
	var beat func()
	if wantsStream(body) {
		beat = func() {
			if out == nil {
				out = newSSESink(w, p.heartbeatInterval())
			}
			out.commit()
			out.comment(slotWaitComment)
		}
	}
	release, slotWait, err := p.acquire(r.Context(), m, beat)
	if err != nil {
		return // CodeAF gave up on the call while it waited
	}
	defer release()
	defer func() {
		if out != nil && out.beats > 0 {
			log.Printf("%d heartbeat(s) sent to the client for %s", out.beats, m.Upstream)
		}
	}()
	// Checked after the wait, since other calls may have spent the cap.
	if reason := p.overCap(); reason != "" {
		p.fail(w, out, http.StatusPaymentRequired, reason)
		return
	}
	// One malformed tool call left in the history makes upstream refuse every
	// later request (issue #50), so repair the arguments before forwarding.
	repaired := repairToolArgs(body)
	if repaired > 0 {
		log.Printf("repaired %d invalid tool-call argument(s) for %s", repaired, m.Upstream)
	}
	stream, err := RewriteRequest(body, m)
	if err != nil {
		p.fail(w, out, http.StatusBadRequest, err.Error())
		return
	}
	payload, _ := json.Marshal(body)
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstream+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		p.fail(w, out, http.StatusInternalServerError, err.Error())
		return
	}
	request.Header.Set("Authorization", "Bearer "+p.apiKey)
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	// The call starts here, after any wait for a slot; the ledger reads the
	// duration from this moment to when the call is recorded.
	started := time.Now()
	response, err := p.client.Do(request)
	if err != nil {
		p.fail(w, out, http.StatusBadGateway, "upstream unreachable: "+err.Error())
		return
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		log.Printf("upstream %d for %s: %.300s", response.StatusCode, m.Upstream, data)
		if out == nil {
			w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
			w.WriteHeader(response.StatusCode)
			_, _ = w.Write(data)
			return
		}
		// Committed: the pass-through becomes an SSE error event carrying
		// the upstream's status, since this response is already 200.
		p.fail(w, out, response.StatusCode, fmt.Sprintf("upstream %d: %s", response.StatusCode, capString(string(data), 500)))
		return
	}
	if !stream {
		p.relayJSON(w, response.Body, m, started, slotWait, repaired)
		return
	}
	if out == nil {
		out = newSSESink(w, p.heartbeatInterval())
	}
	p.relayStream(out, response.Body, m, started, slotWait, repaired)
}

// wantsStream reports whether the request asked for a streamed response, from
// the raw body: the rewritten body is built later, and the decision to
// heartbeat while waiting for a slot cannot wait for the rewrite (issue #90).
func wantsStream(body map[string]json.RawMessage) bool {
	var stream bool
	_ = json.Unmarshal(body["stream"], &stream)
	return stream
}

// fail reports a failure to the client: a real status while the response is
// not committed, an SSE error event once it is — past the commit the status
// line cannot change, and CodeAF's chunk decoder reads an error object from
// a data line, so a cap refusal is still read as a refusal (issue #90).
func (p *proxy) fail(w http.ResponseWriter, out *sseSink, status int, message string) {
	if out == nil {
		writeError(w, status, message)
		return
	}
	log.Printf("-> %d %s", status, message)
	out.errorEvent(status, message)
}

// Heartbeat comments (issue #90): CodeAF's SSE reader calls alive for
// every `: ` comment line and its arm watcher counts them as beats, so a
// comment proves the path is alive without counting as a first token.
// OpenRouter sends `: OPENROUTER PROCESSING` lines for the same reason.
const (
	slotWaitComment      = "codeaf-proxy waiting for a model slot"
	upstreamQuietComment = "codeaf-proxy upstream quiet"
)

// defaultHeartbeat is how long a streaming client may hear nothing before the
// proxy proves the path with a comment line (issue #90).
const defaultHeartbeat = 10 * time.Second

// heartbeatInterval is the quiet threshold: defaultHeartbeat unless the proxy
// was built with a shorter one (tests).
func (p *proxy) heartbeatInterval() time.Duration {
	if p.heartbeatEvery > 0 {
		return p.heartbeatEvery
	}
	return defaultHeartbeat
}

// sseSink writes to a streaming client. Every write takes the mutex, so a
// heartbeat comment never lands inside a relayed line (issue #90); beats
// counts the comments sent, for the per-call log.
type sseSink struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	every     time.Duration // quiet threshold for the upstream-quiet beat
	mu        sync.Mutex
	committed bool      // the 200 is out: failures go as SSE events now
	lastWrite time.Time // the last line or comment, for the quiet beat
	beats     int       // comment lines sent, for the per-call log
}

// newSSESink wraps w without committing it: real status codes stay possible
// until commit runs.
func newSSESink(w http.ResponseWriter, every time.Duration) *sseSink {
	flusher, _ := w.(http.Flusher)
	return &sseSink{w: w, flusher: flusher, every: every}
}

// commit writes the streaming headers and the 200. After it the status line
// cannot change, so every later failure goes out as an SSE event.
func (s *sseSink) commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed {
		return
	}
	s.committed = true
	s.lastWrite = time.Now()
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.WriteHeader(http.StatusOK)
}

// comment writes one SSE comment line and flushes it.
func (s *sseSink) comment(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, ": %s\n\n", text)
	s.beats++
	s.lastWrite = time.Now()
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// line writes one relayed line and flushes it.
func (s *sseSink) line(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write(b)
	s.lastWrite = time.Now()
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// quietBeat sends the upstream-quiet heartbeat when nothing has gone to the
// client for a full interval: the relay's ticker calls it, and a stream that
// is being relayed resets lastWrite on every line, so a live upstream is
// never interrupted by a comment.
func (s *sseSink) quietBeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.committed || time.Since(s.lastWrite) < s.every {
		return
	}
	fmt.Fprintf(s.w, ": %s\n\n", upstreamQuietComment)
	s.beats++
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// errorEvent sends a failure as a mid-stream error object — the shape
// OpenRouter uses and CodeAF's chunk decoder reads — followed by [DONE],
// which ends the stream cleanly.
func (s *sseSink) errorEvent(status int, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"code": status, "message": message}})
	fmt.Fprintf(s.w, "data: %s\n\n", payload)
	fmt.Fprintf(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// newSlots makes one semaphore per upstream model that has a
// max_concurrent, sized to this run's slice perRunSlots(m, maxParallelRuns):
// several runs at once each hold a slice, and the slices must never sum past
// the model's account-wide limit.
func newSlots(models []Model, maxParallelRuns int) map[string]chan struct{} {
	slots := map[string]chan struct{}{}
	for _, m := range models {
		if m.MaxConcurrent > 0 {
			slots[m.Upstream] = make(chan struct{}, perRunSlots(m, maxParallelRuns))
		}
	}
	return slots
}

// acquire waits for one of the model's call slots, so that no more than
// max_concurrent calls to it are in flight upstream. Waiting is logged, so the
// proxy log shows how much a run was held up by the limit; the wait is also
// returned, and the ledger records it per call. Issue #90: while a call
// waits, beat is called at once and then every heartbeat interval, so a
// streaming client whose response is already committed sees the path is
// alive; a nil beat (non-streaming calls) waits silently, as before.
func (p *proxy) acquire(ctx context.Context, m Model, beat func()) (func(), time.Duration, error) {
	slot, ok := p.slots[m.Upstream]
	if !ok {
		return func() {}, 0, nil
	}
	release := func() { <-slot }
	select {
	case slot <- struct{}{}:
		return release, 0, nil
	default:
	}
	start := time.Now()
	// beats stays a nil channel — its case never fires — when beat is nil.
	var beats <-chan time.Time
	if beat != nil {
		beat()
		ticker := time.NewTicker(p.heartbeatInterval())
		defer ticker.Stop()
		beats = ticker.C
	}
	for {
		select {
		case slot <- struct{}{}:
			wait := time.Since(start)
			log.Printf("waited %.1fs for a slot on %s (max %d at once)", wait.Seconds(), m.Upstream, cap(slot))
			return release, wait, nil
		case <-beats:
			beat()
		case <-ctx.Done():
			wait := time.Since(start)
			log.Printf("call to %s abandoned after waiting %.1fs for a slot (max %d at once)", m.Upstream, wait.Seconds(), cap(slot))
			return nil, wait, ctx.Err()
		}
	}
}

func (p *proxy) relayJSON(w http.ResponseWriter, upstream io.Reader, m Model, started time.Time, slotWait time.Duration, repaired int) {
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
	// The characters are counted out of the decode above: the choices are
	// read from it directly, so counting adds no second parse of the payload.
	var chars charCounts
	var choices []map[string]json.RawMessage
	if json.Unmarshal(payload["choices"], &choices) == nil {
		chars.addChoices(choices, "message")
	}
	payload["model"], _ = json.Marshal(m.ID)
	p.record(m, usage, false, started, slotWait, chars, repaired)
	writeJSON(w, http.StatusOK, payload)
}

// RewriteChunk rewrites one SSE data payload: the model id back to CodeAF's,
// and usage.cost added when the chunk carries usage. It returns the usage it
// found, if any, and adds the chunk's reasoning and output characters to
// chars when it is not nil. The payload is decoded once, and the choices are
// read out of that decode, so counting adds no second parse of the chunk.
func RewriteChunk(data []byte, m Model, chars *charCounts) ([]byte, *Usage) {
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
	if chars != nil {
		var choices []map[string]json.RawMessage
		if json.Unmarshal(chunk["choices"], &choices) == nil {
			chars.addChoices(choices, "delta")
		}
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return data, usage
	}
	return out, usage
}

// relayStream relays the upstream SSE stream to the client, one line at a
// time, each flushed. Issue #90: while upstream stays quiet for a full
// heartbeat interval, a comment line goes to the client, so its watcher never
// reads the silence as a dead path — and a comment never counts as a first
// token. Comment and relayed lines share the sink's mutex, so they never
// interleave mid-line, and the ticker goroutine is stopped and joined before
// the call is recorded, so nothing is written after the stream ends.
func (p *proxy) relayStream(out *sseSink, upstream io.Reader, m Model, started time.Time, slotWait time.Duration, repaired int) {
	out.commit()
	var usage *Usage
	var chars charCounts
	reader := bufio.NewReaderSize(upstream, 1<<16)
	quiet := time.NewTicker(out.every)
	defer quiet.Stop()
	stop := make(chan struct{})
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		for {
			select {
			case <-stop:
				return
			case <-quiet.C:
				out.quietBeat()
			}
		}
	}()
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if data, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
				data = bytes.TrimSpace(data)
				if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
					rewritten, found := RewriteChunk(data, m, &chars)
					if found != nil {
						usage = found
					}
					trimmed = append([]byte("data: "), rewritten...)
				}
			}
			out.line(append(trimmed, '\n'))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("stream from %s cut: %v", m.Upstream, err)
			}
			break
		}
	}
	close(stop)
	done.Wait()
	p.record(m, usage, true, started, slotWait, chars, repaired)
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
	CreatedAt   string          `json:"created_at"`
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
	ID                int64  `json:"id"`
	SubmittedAt       string `json:"submitted_at"`
	Body              string `json:"body"`
	AuthorAssociation string `json:"author_association"`
	User              user   `json:"user"`
	State             string `json:"state"` // reviews only
}

type event struct {
	Label       *label            `json:"label"`
	Issue       *item             `json:"issue"`
	PullRequest *item             `json:"pull_request"`
	Comment     *comment          `json:"comment"`
	Review      *comment          `json:"review"`
	Inputs      map[string]string `json:"inputs"` // workflow_dispatch inputs
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

// diff fetches a pull request's diff (issue #32): the review brief embeds
// it, so the reviewer reads what changed without touching git. The diff
// format comes from the Accept header, and the body is returned as text.
func (g github) diff(n int) (string, error) {
	request, err := http.NewRequest(http.MethodGet, g.api+"/repos/"+g.repo+fmt.Sprintf("/pulls/%d", n), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+g.token)
	request.Header.Set("Accept", "application/vnd.github.v3.diff")
	response, err := g.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		return "", fmt.Errorf("GET /pulls/%d diff: %s: %.300s", n, response.Status, data)
	}
	data, err := io.ReadAll(response.Body)
	return string(data), err
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
	Mode   string // "issue", "pr" or "review"
	Number int
	Branch string
	Base   string
	// Lane is the seat the run takes: models.json's models[lane mod
	// len(models)] becomes the run's worker model. It comes from the
	// queue's `lane` dispatch input, digits only; anything else, and every
	// non-dispatch event, is lane 0.
	Lane int
	// Profile pins every seat to a named model setup from experiments.json
	// (a model experiment). Only dispatches of issues labelled
	// codeaf-experiment carry one; it is validated against the file.
	Profile string
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

// dispatchIssue reads the `issue` input of a workflow_dispatch: digits only,
// and positive. Anything else is refused; the input is untrusted, and the
// issue it names is checked against the label gate after it is fetched.
func dispatchIssue(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// dispatchLane reads the `lane` input of a workflow_dispatch: digits only,
// like the other dispatch inputs, because it is untrusted. Anything else —
// missing, empty, non-digits, a number too big for an int — is lane 0, the
// default seat.
func dispatchLane(raw string) int {
	if !digitsOnly(raw) {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Decide applies the trust rules to one event. lookupPR fetches a PR when the
// event only carries its issue form, and lookupIssue fetches an issue for the
// dispatch path; either is only called once the event has passed every check
// that needs no network.
func Decide(name string, ev event, labelName string, lookupPR, lookupIssue func(int) (item, error)) (Decision, error) {
	issueRun := func(i item, reason string, lane int, profile string) (Decision, error) {
		id, err := runID()
		if err != nil {
			return Decision{}, err
		}
		return Decision{Run: true, Reason: reason, Mode: "issue", Number: i.Number,
			Branch: fmt.Sprintf("codeaf/issue-%d-%s", i.Number, id), Base: ev.Repository.DefaultBranch, Lane: lane, Profile: profile}, nil
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
		if ev.Issue.labelled(experimentLabel) {
			return skip("experiment issues run only by a profile dispatch"), nil
		}
		return issueRun(*ev.Issue, "issue labelled", 0, "")
	case "workflow_dispatch":
		// The queue starts runs by dispatching this workflow with the
		// issue number in its `issue` input: a GITHUB_TOKEN-labelled
		// event does not fire workflows, so dispatch is the machine
		// path. A continuation starts the same way, with the draft PR
		// to continue in the `pr` input (issue #31). The inputs are
		// untrusted, so only their digits are read, and what they name
		// must still pass the same checks as a labelled event: open,
		// not a fork, and labelled labelName.
		// The lane dispatches the run to a worker seat: the queue hands
		// each concurrent run a different model. The input is untrusted,
		// so only its digits are read; anything else is lane 0.
		lane := dispatchLane(ev.Inputs["lane"])
		// A plan run (issue #61) is dispatched by the queue's planner
		// step with plan=true and nothing else but the lane: the run reads
		// the repository and proposes issues, and writes no patch and no
		// PR. The input is untrusted, so only the exact value "true" is a
		// plan run, and it accepts no other target: an issue, pr, review
		// or profile input alongside it is refused rather than silently
		// demoted to that mode. The decision carries the validated lane,
		// the default branch as base, and no issue number and no branch —
		// every workflow step that reads one must treat both as empty.
		if raw := ev.Inputs["plan"]; raw != "" {
			switch {
			case raw != "true":
				return skip("plan input must be exactly true"), nil
			case ev.Inputs["issue"] != "" || ev.Inputs["pr"] != "" ||
				ev.Inputs["review"] != "" || ev.Inputs["profile"] != "":
				return skip("a plan dispatch takes no issue, pr, review or profile input"), nil
			}
			if _, err := runID(); err != nil {
				return Decision{}, err
			}
			return Decision{Run: true, Reason: "plan dispatch", Mode: "plan",
				Base: ev.Repository.DefaultBranch, Lane: lane}, nil
		}
		if n, ok := dispatchIssue(ev.Inputs["pr"]); ok {
			pr, err := lookupPR(n)
			if err != nil {
				return Decision{}, err
			}
			repo := os.Getenv("GITHUB_REPOSITORY")
			switch {
			case pr.State != "open":
				return skip("PR is not open"), nil
			case pr.Head.Repo == nil || pr.Head.Repo.FullName != repo:
				return skip("PR head is in a fork; CodeAF only pushes to branches in " + repo), nil
			case !pr.labelled(labelName):
				return skip("PR is not labelled " + labelName), nil
			}
			return Decision{Run: true, Reason: "workflow dispatch", Mode: "pr", Number: pr.Number, Branch: pr.Head.Ref, Base: pr.Base.Ref, Lane: lane}, nil
		}
		// A review run (issue #32) is dispatched the same way, with the PR
		// to review in the `review` input: same checks as the pr input
		// above (open, head not in a fork, labelled), but the run goes to
		// review mode. Only consulted when the pr input is absent, so the
		// continuation path keeps its meaning.
		if n, ok := dispatchIssue(ev.Inputs["review"]); ok {
			pr, err := lookupPR(n)
			if err != nil {
				return Decision{}, err
			}
			repo := os.Getenv("GITHUB_REPOSITORY")
			switch {
			case pr.State != "open":
				return skip("PR is not open"), nil
			case pr.Head.Repo == nil || pr.Head.Repo.FullName != repo:
				return skip("PR head is in a fork; CodeAF only pushes to branches in " + repo), nil
			case !pr.labelled(labelName):
				return skip("PR is not labelled " + labelName), nil
			}
			return Decision{Run: true, Reason: "review dispatch", Mode: "review", Number: pr.Number, Branch: pr.Head.Ref, Base: pr.Base.Ref, Lane: lane}, nil
		}
		n, ok := dispatchIssue(ev.Inputs["issue"])
		if !ok {
			return skip("workflow_dispatch needs a numeric issue or pr input"), nil
		}
		i, err := lookupIssue(n)
		if err != nil {
			return Decision{}, err
		}
		switch {
		case i.State != "open":
			return skip("issue is not open"), nil
		case len(i.PullRequest) > 0:
			return skip("issue is a PR"), nil
		case !i.labelled(labelName):
			return skip("issue is not labelled " + labelName), nil
		}
		// A model experiment pins every seat to a named profile; profiles
		// are only for issues labelled codeaf-experiment, and those run only
		// with one.
		profile, hasProfile := ev.Inputs["profile"], ev.Inputs["profile"] != ""
		switch {
		case i.labelled(experimentLabel) && !hasProfile:
			return skip("experiment issues run only with a profile"), nil
		case !i.labelled(experimentLabel) && hasProfile:
			return skip("profiles are only for issues labelled " + experimentLabel), nil
		case hasProfile:
			if _, err := experimentProfile(profile); err != nil {
				return skip(err.Error()), nil
			}
		}
		return issueRun(i, "workflow dispatch", lane, profile)
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
	if target.labelled(experimentLabel) {
		return skip("experiment issues run only by a profile dispatch"), nil
	}
	if name == "issue_comment" && len(target.PullRequest) == 0 {
		return issueRun(*target, "command on issue", 0, "")
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
	}, func(n int) (item, error) {
		var it item
		return it, gh.get(fmt.Sprintf("/issues/%d", n), &it)
	})
	if err != nil {
		return err
	}
	outputs := fmt.Sprintf("run=%t\nreason=%s\nlane=%d\n", decision.Run, decision.Reason, decision.Lane)
	if decision.Run {
		outputs += fmt.Sprintf("mode=%s\nnumber=%d\nbranch=%s\nbase=%s\nprofile=%s\n", decision.Mode, decision.Number, decision.Branch, decision.Base, decision.Profile)
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
	ID                int64  `json:"id"`
	CreatedAt         string `json:"created_at"`
	Body              string `json:"body"`
	AuthorAssociation string `json:"author_association"`
	User              user   `json:"user"`
}

type reviewComment struct {
	ID                int64  `json:"id"`
	CreatedAt         string `json:"created_at"`
	Path              string `json:"path"`
	Line              *int   `json:"line"`
	AuthorAssociation string `json:"author_association"`
	OriginalLine      *int   `json:"original_line"`
	Body              string `json:"body"`
	User              user   `json:"user"`
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

// ---------------------------------------------------------------- the brief's context sections

// budgetLine opens the brief with the run's budget. CODEAF_BUDGET_MINUTES is
// digits only and validated here; missing or invalid falls back to 100,
// which matches the workflow job's timeout-minutes. The cap comes from
// models.json (limits.run_credit_usd, which the proxy enforces); when that
// config cannot be loaded the line says the credit cap is unknown instead of
// failing, so a broken installation still produces a brief.
func budgetLine() string {
	minutes := 100
	if v := os.Getenv("CODEAF_BUDGET_MINUTES"); digitsOnly(v) {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			minutes = n
		}
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		return fmt.Sprintf("You have %d minutes (credit cap unknown). Stop and write your final answer at least 10 minutes before the deadline.", minutes)
	}
	return fmt.Sprintf("You have %d minutes and $%s of credit. Stop and write your final answer at least 10 minutes before the deadline.", minutes, trimNumber(cfg.Limits.RunCreditUSD))
}

// maxBriefBytes bounds the whole brief: every section is capped on its own,
// but a crafted target body, thread or dependency list still adds up.
const maxBriefBytes = 100000

// capBrief truncates s at max bytes without splitting a UTF-8 rune and
// appends a note line; the note counts against the cap.
func capBrief(s string, max int) string {
	if len(s) <= max {
		return s
	}
	note := fmt.Sprintf("\n\n(brief truncated at %d bytes)\n", max)
	cut := max - len(note)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + note
}

// actionsBotEmail is the identity the publish job pushes as; the newest
// commit authored or committed by it marks the last CodeAF push.
const actionsBotEmail = "41898282+github-actions[bot]@users.noreply.github.com"

// reportSummaryOpen opens the summary block of a run report.
const reportSummaryOpen = "<details><summary>CodeAF's summary</summary>"

const (
	maxDeps     = 10 // "depends on" items rendered at most
	maxDepFiles = 20 // changed paths shown per merged pull request
	maxFindings = 20 // review findings rendered at most, newest last
)

// pull is the part of a pull request the dependencies section reads.
type pull struct {
	Number   int    `json:"number"`
	Title    string `json:"title"`
	MergedAt string `json:"merged_at"`
}

// prFile is one changed path of a pull request.
type prFile struct {
	Filename string `json:"filename"`
}

// gitPerson is the author or committer block of a commit.
type gitPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

// prCommit is one commit of a pull request; only who made it and when is
// read.
type prCommit struct {
	Commit struct {
		Author    gitPerson `json:"author"`
		Committer gitPerson `json:"committer"`
	} `json:"commit"`
}

// timelineEvent is one event of an issue's timeline; only cross-references
// whose source is a pull request in this repository are read.
type timelineEvent struct {
	Event  string `json:"event"`
	Source *struct {
		Issue *struct {
			Number      int             `json:"number"`
			PullRequest json.RawMessage `json:"pull_request"` // present when the source is a pull request
			Repository  *struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"issue"`
	} `json:"source"`
}

// parseAPITime parses a GitHub timestamp; a malformed or forged one yields
// false and the item is dropped, not guessed about.
func parseAPITime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// lastReportComment returns the newest comment on the target that carries
// the run-report marker, or nil.
func lastReportComment(comments []issueComment) *issueComment {
	for i := len(comments) - 1; i >= 0; i-- {
		if strings.Contains(comments[i].Body, reportMarker) {
			return &comments[i]
		}
	}
	return nil
}

// reportTableRow returns the cell of the report table's row labelled label:
// the text between the label and the row's closing pipe. A report body is
// plain text here, so it parses as such, not as data.
func reportTableRow(report, label string) (string, bool) {
	prefix := "| " + label + " |"
	for _, line := range strings.Split(report, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		cell := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		return strings.TrimSpace(strings.TrimSuffix(cell, "|")), true
	}
	return "", false
}

// reportSummary returns the text inside a report's summary <details> block.
func reportSummary(report string) (string, bool) {
	i := strings.Index(report, reportSummaryOpen)
	if i < 0 {
		return "", false
	}
	inner := report[i+len(reportSummaryOpen):]
	if j := strings.Index(inner, "</details>"); j >= 0 {
		inner = inner[:j]
	}
	return strings.TrimSpace(inner), true
}

// previousReportSection re-reads the last CodeAF run report on this issue or
// PR: its stop reason, its metered spend and CodeAF's summary. Everything
// extracted from a report is untrusted text, so the section renders it
// inside a quoted data block, escaped, every line prefixed with "> " — a
// report forged to read like instructions stays inert data.
func previousReportSection(gh github, number int) string {
	var b strings.Builder
	b.WriteString("## The previous CodeAF report\n")
	var comments []issueComment
	if err := gh.get(fmt.Sprintf("/issues/%d/comments?per_page=100", number), &comments); err != nil {
		fmt.Fprintf(&b, "\n(previous CodeAF report unavailable: %s)\n", escapeMD(err.Error(), 200, true))
		return b.String()
	}
	report := lastReportComment(comments)
	if report == nil {
		b.WriteString("\n(no previous CodeAF report found on this issue or PR)\n")
		return b.String()
	}
	b.WriteString("\n")
	if stop, ok := reportTableRow(report.Body, "stop / ok"); ok {
		fmt.Fprintf(&b, "> stop: %s\n", escapeMD(stop, 200, true))
	} else {
		b.WriteString("> stop: (not found in the previous report)\n")
	}
	if spend, ok := reportTableRow(report.Body, "metered by proxy (from its ledger)"); ok {
		fmt.Fprintf(&b, "> spend: %s\n", escapeMD(spend, 200, true))
	} else {
		b.WriteString("> spend: (not found in the previous report)\n")
	}
	if summary, ok := reportSummary(report.Body); ok {
		b.WriteString("> summary:\n")
		for _, line := range strings.Split(escapeMD(summary, 1500, false), "\n") {
			if line == "" {
				b.WriteString(">\n")
			} else {
				b.WriteString("> " + line + "\n")
			}
		}
	} else {
		b.WriteString("> summary: (not found in the previous report)\n")
	}
	return b.String()
}

// finding is one item of the review findings: an issue comment, a review or
// an inline review comment newer than the last CodeAF push.
type finding struct {
	when  time.Time
	login string
	assoc string
	state string // reviews only
	path  string // inline review comments only
	line  int    // inline review comments only; 0 when the API sent no line
	body  string
}

// prBotCutoff returns when the publish job last pushed to the pull request:
// the newest commit authored or committed by the Actions bot. With no such
// commit it returns the zero time, which includes every finding.
func (g github) prBotCutoff(number int) (time.Time, error) {
	var commits []prCommit
	if err := g.get(fmt.Sprintf("/pulls/%d/commits?per_page=100", number), &commits); err != nil {
		return time.Time{}, err
	}
	cutoff := time.Time{}
	for _, c := range commits {
		if c.Commit.Author.Email != actionsBotEmail && c.Commit.Committer.Email != actionsBotEmail {
			continue
		}
		t, ok := parseAPITime(c.Commit.Committer.Date)
		if !ok {
			t, ok = parseAPITime(c.Commit.Author.Date)
		}
		if ok && t.After(cutoff) {
			cutoff = t
		}
	}
	return cutoff, nil
}

// reviewFindings collects what reviewers said after cutoff, without the
// bots' entries and without the comment or review that triggered this run.
// A source the API cannot answer degrades to a note; it must not fail the
// brief.
func (g github) reviewFindings(number int, cutoff time.Time, triggerCommentID, triggerReviewID int64) (items []finding, notes []string) {
	var comments []issueComment
	if err := g.get(fmt.Sprintf("/issues/%d/comments?per_page=100", number), &comments); err != nil {
		notes = append(notes, err.Error())
	}
	for _, c := range comments {
		if c.User.Type == "Bot" || (triggerCommentID != 0 && c.ID == triggerCommentID) {
			continue
		}
		when, ok := parseAPITime(c.CreatedAt)
		if !ok || !when.After(cutoff) {
			continue
		}
		items = append(items, finding{when: when, login: c.User.Login, assoc: c.AuthorAssociation, body: c.Body})
	}

	var reviews []comment
	if err := g.get(fmt.Sprintf("/pulls/%d/reviews?per_page=100", number), &reviews); err != nil {
		notes = append(notes, err.Error())
	}
	for _, r := range reviews {
		if r.User.Type == "Bot" || (triggerReviewID != 0 && r.ID == triggerReviewID) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(r.State), "approved") || strings.TrimSpace(r.Body) == "" {
			continue
		}
		when, ok := parseAPITime(r.SubmittedAt)
		if !ok || !when.After(cutoff) {
			continue
		}
		items = append(items, finding{when: when, login: r.User.Login, assoc: r.AuthorAssociation, state: r.State, body: r.Body})
	}

	var inline []reviewComment
	if err := g.get(fmt.Sprintf("/pulls/%d/comments?per_page=100", number), &inline); err != nil {
		notes = append(notes, err.Error())
	}
	for _, c := range inline {
		if c.User.Type == "Bot" {
			continue
		}
		when, ok := parseAPITime(c.CreatedAt)
		if !ok || !when.After(cutoff) {
			continue
		}
		f := finding{when: when, login: c.User.Login, assoc: c.AuthorAssociation, path: c.Path, body: c.Body}
		switch {
		case c.Line != nil:
			f.line = *c.Line
		case c.OriginalLine != nil:
			f.line = *c.OriginalLine
		}
		items = append(items, f)
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].when.Before(items[j].when) })
	return lastN(items, maxFindings), notes
}

// reviewFindingsSection renders what reviewers said after the last CodeAF
// push, newest last. Logins, associations, states, paths and bodies are
// untrusted text and render escaped, one line each; none of it renders raw.
func reviewFindingsSection(gh github, number int, ev event) string {
	var b strings.Builder
	b.WriteString("## Open review findings (after the last CodeAF push)\n")
	cutoff, err := gh.prBotCutoff(number)
	if err != nil {
		fmt.Fprintf(&b, "\n(review findings unavailable: %s)\n", escapeMD(err.Error(), 200, true))
		return b.String()
	}
	var triggerCommentID, triggerReviewID int64
	if ev.Comment != nil {
		triggerCommentID = ev.Comment.ID
	}
	if ev.Review != nil {
		triggerReviewID = ev.Review.ID
	}
	items, notes := gh.reviewFindings(number, cutoff, triggerCommentID, triggerReviewID)
	if len(items) == 0 && len(notes) == 0 {
		b.WriteString("\n(no review findings since the last CodeAF push)\n")
		return b.String()
	}
	b.WriteString("\n")
	for _, f := range items {
		fmt.Fprintf(&b, "- %s %s", f.when.UTC().Format("2006-01-02"), escapeMD("@"+f.login, 80, true))
		detail := f.assoc
		if f.state != "" {
			if detail != "" {
				detail += ", "
			}
			detail += f.state
		}
		if detail != "" {
			fmt.Fprintf(&b, " (%s)", escapeMD(detail, 200, true))
		}
		if f.path != "" {
			if f.line != 0 {
				fmt.Fprintf(&b, " %s line %d:", escapeMD(f.path, 200, true), f.line)
			} else {
				fmt.Fprintf(&b, " %s:", escapeMD(f.path, 200, true))
			}
		} else {
			b.WriteString(":")
		}
		fmt.Fprintf(&b, " %s\n", escapeMD(f.body, 300, true))
	}
	for _, note := range notes {
		fmt.Fprintf(&b, "(some findings unavailable: %s)\n", escapeMD(note, 200, true))
	}
	return b.String()
}

// asciiLower folds ASCII letters only, byte for byte, so indexes into the
// result index the original string; multi-byte sequences pass through.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// isRefByte reports whether c can be part of a word around a "#N" reference:
// "foo#12" and "#12ab" are not references.
func isRefByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

// hashRefs returns every #N reference in s: a '#' outside a word, digits,
// and a word boundary after.
func hashRefs(s string) []int {
	var out []int
	for i := 0; i < len(s); i++ {
		if s[i] != '#' || (i > 0 && isRefByte(s[i-1])) {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i+1 || (j < len(s) && isRefByte(s[j])) {
			continue
		}
		if n, err := strconv.Atoi(s[i+1 : j]); err == nil && n > 0 {
			out = append(out, n)
		}
		i = j - 1
	}
	return out
}

// depsFromBody collects the issue references that follow a "depends on"
// phrase on the same line ("Depends on #12, #13"), deduplicated and sorted,
// at most maxDeps of them.
func depsFromBody(body string) []int {
	seen := map[int]bool{}
	var out []int
	for _, line := range strings.Split(body, "\n") {
		lower := asciiLower(line)
		for i := 0; ; {
			j := strings.Index(lower[i:], "depends on")
			if j < 0 {
				break
			}
			i += j + len("depends on")
			for _, n := range hashRefs(line[i:]) {
				if !seen[n] {
					seen[n] = true
					out = append(out, n)
				}
			}
		}
	}
	sort.Ints(out)
	if len(out) > maxDeps {
		out = out[:maxDeps]
	}
	return out
}

// mergedPRForIssue returns the merged pull request that closed issue n: the
// timeline's cross-referenced events whose source is a pull request in this
// repository, of which the one merged most recently wins. m is 0 when none
// merged. A candidate the API cannot answer is skipped; when none can be
// answered the first error comes back.
func (g github) mergedPRForIssue(n int) (int, pull, error) {
	var events []timelineEvent
	if err := g.get(fmt.Sprintf("/issues/%d/timeline?per_page=100", n), &events); err != nil {
		return 0, pull{}, err
	}
	best, bestAt, firstErr := 0, time.Time{}, error(nil)
	var bestPR pull
	for _, ev := range events {
		if ev.Event != "cross-referenced" || ev.Source == nil || ev.Source.Issue == nil || ev.Source.Issue.PullRequest == nil {
			continue
		}
		if repo := ev.Source.Issue.Repository; repo != nil && repo.FullName != "" && repo.FullName != g.repo {
			continue // another repository's pull request
		}
		var candidate pull
		if err := g.get(fmt.Sprintf("/pulls/%d", ev.Source.Issue.Number), &candidate); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		merged, ok := parseAPITime(candidate.MergedAt)
		if !ok {
			continue // references the issue but never merged
		}
		if best == 0 || merged.After(bestAt) {
			best, bestPR, bestAt = ev.Source.Issue.Number, candidate, merged
		}
	}
	if best == 0 && firstErr != nil {
		return 0, pull{}, firstErr
	}
	return best, bestPR, nil
}

// dependencyBullet renders one "depends on" item: number, state and title,
// and for a closed one the merged pull request that closed it with its
// changed paths. An API failure degrades that item to a note; it must not
// fail the brief.
func dependencyBullet(gh github, n int) string {
	unavailable := func(reason string) string {
		return fmt.Sprintf("- #%d (unavailable: %s)\n", n, escapeMD(reason, 200, true))
	}
	var it item
	if err := gh.get(fmt.Sprintf("/issues/%d", n), &it); err != nil {
		return unavailable(err.Error())
	}
	line := fmt.Sprintf("- #%d (%s) %s", n, escapeMD(it.State, 40, true), escapeMD(it.Title, 200, true))
	m, pr, err := gh.mergedPRForIssue(n)
	if err != nil {
		return line + " (unavailable: " + escapeMD(err.Error(), 200, true) + ")\n"
	}
	if m == 0 {
		return line + "\n"
	}
	line += fmt.Sprintf(" — merged as PR #%d %s", m, escapeMD(pr.Title, 200, true))
	var files []prFile
	if err := gh.get(fmt.Sprintf("/pulls/%d/files?per_page=100", m), &files); err != nil {
		return line + " (unavailable: " + escapeMD(err.Error(), 200, true) + ")\n"
	}
	var b strings.Builder
	b.WriteString(line + "\n")
	shown := files
	if len(shown) > maxDepFiles {
		shown = shown[:maxDepFiles]
	}
	for _, f := range shown {
		b.WriteString("  - " + escapeMD(f.Filename, 200, true) + "\n")
	}
	if rest := len(files) - len(shown); rest > 0 {
		fmt.Fprintf(&b, "  - … and %d more files\n", rest)
	}
	return b.String()
}

// dependenciesSection renders the issues the target body says it depends on:
// title and state, and the merged pull request that closed each one.
func dependenciesSection(gh github, body string) string {
	var b strings.Builder
	b.WriteString("## Dependencies (\"depends on\" items)\n")
	deps := depsFromBody(body)
	if len(deps) == 0 {
		b.WriteString("\n(no \"depends on\" items in the body)\n")
		return b.String()
	}
	b.WriteString("\n")
	for _, n := range deps {
		b.WriteString(dependencyBullet(gh, n))
	}
	return b.String()
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
	parts := []string{budgetLine(), string(standing)}
	var targetBody string

	if os.Getenv("MODE") == "issue" {
		var issue item
		if err := gh.get(fmt.Sprintf("/issues/%d", number), &issue); err != nil {
			return err
		}
		targetBody = issue.Body
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
		targetBody = pr.Body
		parts = append(parts, fmt.Sprintf("# Follow-up on pull request #%d — %s\n\nYou are continuing work on an open pull request. "+
			"The working tree is its head branch `%s`. Address the request below; do not redo work that is already on the branch.\n\n"+
			"## PR description\n\n%s", number, pr.Title, pr.Head.Ref, pr.Body))
		// Issue #57: the checkout may have merged the base branch tip in
		// already — cleanly, or with conflict markers the run must resolve.
		// A conflicted merge that cannot be read fails the brief: CodeAF
		// would otherwise work on marked files it was never told about.
		section, err := mergeBriefSection(envOr("MERGE_INFO_FILE", ""))
		if err != nil {
			return err
		}
		if section != "" {
			parts = append(parts, section)
		}
		// A continuation must say what the issue still requires
		// (issue #31): when the PR body names the issue it closes,
		// fetch it and add its task text. An API failure degrades to
		// a note; it must not fail the brief.
		if n := linkedIssue(pr.Body); n != 0 {
			var closes item
			if err := gh.get(fmt.Sprintf("/issues/%d", n), &closes); err != nil {
				parts = append(parts, fmt.Sprintf("(the issue this PR closes, #%d, is unavailable: %s)\n", n, escapeMD(err.Error(), 200, true)))
			} else {
				parts = append(parts, fmt.Sprintf("## The issue this PR closes (#%d) — %s\n\n%s", n, escapeMD(closes.Title, 200, true), closes.Body))
			}
		}
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

	parts = append(parts, previousReportSection(gh, number))
	if os.Getenv("MODE") == "pr" {
		parts = append(parts, reviewFindingsSection(gh, number, ev))
	}
	parts = append(parts, dependenciesSection(gh, targetBody))

	if trigger := ev.trigger(); trigger != nil {
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(trigger.Body), command))
		if text == "" {
			text = "(no text: continue / fix what the reviews ask)"
		}
		parts = append(parts, fmt.Sprintf("## The request you are acting on now (from @%s)\n\n%s", trigger.User.Login, text))
	}
	brief := strings.Join(parts, "\n\n") + "\n"
	brief = capBrief(brief, maxBriefBytes)
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

// readResult returns the result and its raw bytes; the raw bytes are parsed
// again for the plan's tasks, whose shape is CodeAF's to change.
func readResult() (Result, []byte, error) {
	var result Result
	data, err := os.ReadFile(os.Getenv("RESULT_PATH"))
	if err != nil {
		return result, nil, err
	}
	return result, data, json.Unmarshal(data, &result)
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
	// Issue #49: the review report runs in the publish job, which reads only
	// this job's output artifact, and it falls back to the root task's run
	// record when the result's answer carries no findings. So in review mode
	// the record is shipped beside the result, capped here and read there as
	// untrusted data. Issue #119: plan mode ships it too — a plan run's
	// proposals can live only in the root task's closing result (the answer
	// envelope is often empty), and plan-report's fallback reads this same
	// copy out of the output artifact the publish job downloads.
	if mode := envOr("MODE", ""); mode == "review" || mode == "plan" {
		if from := shipRunRecord(); from != "" {
			log.Printf("shipped the run record %s beside the result", from)
		}
	}
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
		if l.Kind == "proxy_start" {
			continue // the start marker is not a call; unknown kinds still cost money
		}
		// Search lines cost 0 (issue #58); a documented price flows through CostUSD and is counted here unchanged.
		total += l.CostUSD
	}
	return total
}

// spentWeek reads a week's credit spend from the shared state bundle — for the
// proxy, the credit spent earlier in the week (PROXY_WEEK_SPENT_USD); the
// queue reuses it for its own weekly total. That total is built from numbers
// run jobs reported, so it is treated as untrusted: a negative, infinite or
// unparseable total would loosen the cap, and is refused rather than repaired
// (the AGENTS.md rule for spend totals).
func spentWeek(name string) (float64, error) {
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

// fmtDuration renders a duration for the report, rounded to the second.
func fmtDuration(d time.Duration) string { return d.Round(time.Second).String() }

// The ledger is written by the untrusted run job, so every number in it is
// hostile. Token counts and waits are clamped to bounds no real call reaches,
// and timing spans that could not happen — starting before the proxy_start
// marker, ending after now, or lasting more than a day — are dropped rather
// than rendered as overflowing durations.
const (
	maxTokensPerCall = 10_000_000
	maxSpan          = 24 * time.Hour
)

// clampTokens bounds a token count from the ledger.
func clampTokens(n int) int {
	if n < 0 {
		return 0
	}
	if n > maxTokensPerCall {
		return maxTokensPerCall
	}
	return n
}

// clampSeconds bounds a seconds figure from the ledger.
func clampSeconds(s float64) float64 {
	if math.IsNaN(s) || s < 0 {
		return 0
	}
	if s > maxSpan.Seconds() {
		return maxSpan.Seconds()
	}
	return s
}

// spanDuration converts ledger seconds to a duration for the report; the
// clamp also keeps a sum of hostile figures from overflowing the int64.
func spanDuration(s float64) time.Duration {
	return time.Duration(clampSeconds(s) * float64(time.Second))
}

// callSpan is one call's time in flight, for the timing figures.
type callSpan struct {
	start, end time.Time
	out        int
	model      string
}

// inFlight merges the spans: the total time with at least one call in flight,
// and how long exactly 1, 2 and 3+ calls overlapped.
func inFlight(spans []callSpan) (union, at1, at2, at3 float64) {
	type edge struct {
		t time.Time
		d int
	}
	edges := make([]edge, 0, 2*len(spans))
	for _, s := range spans {
		edges = append(edges, edge{s.start, 1}, edge{s.end, -1})
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].t.Before(edges[j].t) })
	level, prev := 0, time.Time{}
	for _, e := range edges {
		if level > 0 && e.t.After(prev) {
			d := e.t.Sub(prev).Seconds()
			union += d
			switch {
			case level == 1:
				at1 += d
			case level == 2:
				at2 += d
			default:
				at3 += d
			}
		}
		level += e.d
		prev = e.t
	}
	return union, at1, at2, at3
}

type modelTotals struct {
	calls, in, cached, out, unpriced int
	cost                             float64
	reasoning                        int
	reasoningKnown                   bool // whether any call reported reasoning tokens
	reasoningChars, outputChars      int  // character counts, for models that do not
}

// sharePct renders a share of a whole as a percent, one decimal under 10% so
// small shares do not round up to a bare "1%".
func sharePct(part, whole int) string {
	if whole <= 0 {
		return "—"
	}
	pct := 100 * float64(part) / float64(whole)
	if pct < 10 {
		return fmt.Sprintf("%.1f%%", pct)
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// ---------------------------------------------------------------- untrusted text

// reportMaxAnswer caps CodeAF's answer in the report, in runes.
const reportMaxAnswer = 6000

// escapeMD makes untrusted text inert in GitHub-flavored markdown. Every
// character that markdown or HTML gives structure to is escaped (backslash
// for markdown punctuation, entities for < > &), and an @ that could start
// a mention or an autolink is split with a zero-width space, so the rendered
// comment shows the text itself and cannot carry headings, tables, code
// spans, links, images, mentions or raw HTML. Control characters are dropped,
// newlines are kept unless oneLine, and the text is capped at max runes with
// an ellipsis. (. - + ( ) stay unescaped; see the default case below.)
func escapeMD(s string, max int, oneLine bool) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max {
			b.WriteRune('…')
			return b.String()
		}
		switch {
		case r == '\n':
			if oneLine {
				r = ' '
			} else {
				b.WriteByte('\n')
				n++
				continue
			}
		case r == '\t':
			r = ' '
		case r < 0x20 || r == 0x7f:
			continue
		}
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '@':
			b.WriteString("@\u200b")
		default:
			// . - + ( ) are left alone: mid-line they are plain prose, and at
			// a line start they make only lists and rules, which carry
			// nothing and cannot inject.
			if strings.ContainsRune("\\`*_{}[]#|~!", r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
}

// plainToken reports whether s is the short lowercase word CodeAF's own stop,
// verdict and task-state values are. Such a value renders as it is; anything
// else is untrusted prose and renders escaped.
func plainToken(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 0:
			if r < 'a' || r > 'z' {
				return false
			}
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// tokenCell renders a short untrusted word: known-shaped values in backticks,
// anything else escaped and capped, an empty one as an em-dash.
func tokenCell(s string, max int) string {
	switch {
	case s == "":
		return "—"
	case plainToken(s):
		return "`" + s + "`"
	default:
		return escapeMD(s, max, true)
	}
}

// ---------------------------------------------------------------- the plan's tasks

// planTask is one task of the plan CodeAF's result may carry.
type planTask struct {
	Title string
	State string
}

const (
	maxPlanTasks      = 200 // parsed at most; a hostile result cannot grow the report
	maxPlanTasksShown = 30  // rendered at most
)

// planTasks digs the plan's tasks and their states out of the raw result
// JSON. The field is CodeAF's to shape, so the reader is permissive: a
// top-level "tasks" array, or a "plan" object with one, of objects carrying
// a title (or name) and a state (or status). Anything else — missing,
// differently shaped, absurdly long — yields what it can, capped.
func planTasks(raw []byte) []planTask {
	if len(raw) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	arr, ok := top["tasks"]
	if !ok {
		var plan struct {
			Tasks json.RawMessage `json:"tasks"`
		}
		if json.Unmarshal(top["plan"], &plan) != nil || plan.Tasks == nil {
			return nil
		}
		arr = plan.Tasks
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(arr, &items); err != nil {
		return nil
	}
	if len(items) > maxPlanTasks {
		items = items[:maxPlanTasks]
	}
	tasks := make([]planTask, 0, len(items))
	for _, it := range items {
		var t planTask
		json.Unmarshal(it["title"], &t.Title) // best effort by design
		if t.Title == "" {
			json.Unmarshal(it["name"], &t.Title)
		}
		json.Unmarshal(it["state"], &t.State)
		if t.State == "" {
			json.Unmarshal(it["status"], &t.State)
		}
		t.Title, t.State = strings.TrimSpace(t.Title), strings.TrimSpace(t.State)
		if len(t.Title) > 1024 {
			t.Title = t.Title[:1024]
		}
		if len(t.State) > 256 {
			t.State = t.State[:256]
		}
		if t.Title == "" && t.State == "" {
			continue
		}
		tasks = append(tasks, t)
	}
	if len(tasks) == 0 {
		return nil
	}
	return tasks
}

// ---------------------------------------------------------------- the patch's diffstat

// fileDiff is one file's change, as the patch's diffstat reports it.
type fileDiff struct {
	Path   string
	Add    int
	Del    int
	Binary bool
}

const (
	maxDiffFiles = 200      // parsed at most; the rest is counted, not parsed
	maxDiffShown = 50       // rendered at most
	maxDiffPath  = 512      // one path, in runes
	maxPatchRead = 64 << 20 // a patch over this is not summarised
)

// diffstat parses a git patch the way `git diff --stat` would: which files
// changed, with lines added and removed. The patch is untrusted, so the
// parser only reads it: quoted paths are unquoted, hunk lines that look like
// headers ("--- x" as a removed line) stay hunk lines, binary sections are
// skipped (their base85 lines can start with + or -), and a patch with more
// than maxDiffFiles files stops early, with the rest counted in the second
// return.
func diffstat(patch []byte) (diffs []fileDiff, more int) {
	sc := bufio.NewScanner(bytes.NewReader(patch))
	sc.Buffer(make([]byte, 64*1024), 1<<22)
	var (
		cur                 *fileDiff
		prePath             string // the section's a/ path; "" is /dev/null or unset
		preGuess, postGuess string // the paths the diff --git header named
		inHunk              bool
	)
	flush := func() {
		if cur == nil {
			return
		}
		path := cur.Path
		switch {
		case path == "":
			path = prePath
		case prePath != "" && prePath != path:
			path = prePath + " → " + path
		}
		if path == "" {
			// No ---/+++ lines named a path — a binary diff has none — so the
			// header the section started with is all there is.
			switch {
			case preGuess != "" && postGuess != "" && preGuess != postGuess:
				path = preGuess + " → " + postGuess
			default:
				path = postGuess
				if path == "" {
					path = preGuess
				}
			}
		}
		if path != "" {
			if len(diffs) < maxDiffFiles {
				diffs = append(diffs, fileDiff{Path: path, Add: cur.Add, Del: cur.Del, Binary: cur.Binary})
			} else {
				more++
			}
		}
		cur, prePath, inHunk = nil, "", false
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &fileDiff{}
			preGuess, postGuess = headerPaths(line[len("diff --git "):])
		case cur == nil:
			// before the first file section there is nothing to read
		case strings.HasPrefix(line, "GIT binary patch"),
			strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ"):
			cur.Binary = true
		case !inHunk && strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk && strings.HasPrefix(line, "--- "):
			prePath = patchPath(line[4:], "a/")
		case !inHunk && strings.HasPrefix(line, "+++ "):
			if p := patchPath(line[4:], "b/"); p != "" {
				cur.Path = p
			}
		case !inHunk && strings.HasPrefix(line, "rename from "):
			prePath = patchPath(line[len("rename from "):], "")
		case !inHunk && strings.HasPrefix(line, "rename to "):
			cur.Path = patchPath(line[len("rename to "):], "")
		case inHunk && !cur.Binary:
			switch line[0] {
			case '+':
				cur.Add++
			case '-':
				cur.Del++
			}
		}
	}
	flush()
	return diffs, more
}

// headerPaths reads the two paths out of a "diff --git a/X b/Y" line, which
// git quotes when a path needs it. Best effort: a section without ---/+++
// lines — a binary diff — is named from them.
func headerPaths(rest string) (pre, post string) {
	if strings.HasPrefix(rest, `"`) {
		i := strings.Index(rest[1:], `"`)
		if i < 0 {
			return "", ""
		}
		pre = patchPath(rest[:i+2], "a/")
		rest = strings.TrimSpace(rest[i+2:])
		return pre, patchPath(rest, "b/")
	}
	i := strings.Index(rest, " b/")
	if i < 0 {
		return patchPath(rest, "a/"), ""
	}
	return patchPath(rest[:i], "a/"), patchPath(rest[i+1:], "b/")
}

// patchPath cleans one path out of a patch header: unquoted if git quoted it,
// stripped of the a/ or b/ prefix, capped, and empty for /dev/null.
func patchPath(arg, prefix string) string {
	p := strings.TrimSpace(arg)
	if p == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(p, `"`) {
		if u, err := strconv.Unquote(p); err == nil {
			p = u
		} else {
			p = strings.Trim(p, `"`)
		}
	}
	p = strings.TrimPrefix(p, prefix)
	if runes := []rune(p); len(runes) > maxDiffPath {
		p = string(runes[:maxDiffPath]) + "…"
	}
	return p
}

// readPublishErrors reads the ::error:: lines the publish steps collected
// into a file. Workflow output, but read as data all the same: capped here,
// and escaped again at render.
func readPublishErrors(path string) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<16))
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "::error::"))
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return lines
}

// patchDiffstat summarises the patch the run job handed over. A missing patch
// is normal (nothing changed, or the run died before packaging); a patch too
// big to read is reported in the note instead.
func patchDiffstat(path string) (diffs []fileDiff, more int, note string) {
	if path == "" {
		return nil, 0, ""
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, "" // missing or unreadable: no diffstat, no note
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, ""
	}
	if info.Size() > maxPatchRead {
		return nil, 0, "the patch is over 64 MB and was not summarised"
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, ""
	}
	diffs, more = diffstat(data)
	return diffs, more, ""
}

// diagnosticsArtifact looks up this run's diagnostics artifact through the
// API, the way runWallTime does. Anything missing — no run id, no attempt, no
// token, a failed call, no artifact — means no id, and the report links to
// the run page, where the artifacts are listed, instead.
func diagnosticsArtifact() string {
	run, attempt := os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_RUN_ATTEMPT")
	gh := newGitHub()
	if run == "" || attempt == "" || gh.repo == "" || gh.token == "" {
		return ""
	}
	var listing struct {
		Artifacts []struct {
			ID int64 `json:"id"`
		} `json:"artifacts"`
	}
	name := fmt.Sprintf("codeaf-diagnostics-%s-%s", run, attempt)
	if err := gh.get(fmt.Sprintf("/actions/runs/%s/artifacts?name=%s&per_page=1", run, name), &listing); err != nil || len(listing.Artifacts) == 0 {
		return ""
	}
	if id := listing.Artifacts[0].ID; id > 0 {
		return strconv.FormatInt(id, 10)
	}
	return ""
}

// Report renders the run report. It is a pure function of its inputs so it can
// be tested; runReport gathers them, including the run job's wall time.
//
// ReportInputs carries the side files the report reads as data: everything in
// them is untrusted output — of the run job (the patch, the result) and of the
// publish steps (the collected ::error:: lines) — so every string rendered
// from them is capped and escaped, and none of it is ever executed.
type ReportInputs struct {
	PublishErrors []string   // the ::error:: lines the publish steps collected, oldest first
	PlanTasks     []planTask // the plan's tasks, if the result JSON has them
	Diff          []fileDiff // the patch's diffstat
	DiffMore      int        // files past the diffstat's parse cap
	DiffNote      string     // why there is no diffstat, when there is none
	DiagArtifact  string     // the diagnostics artifact's id, if the API had one
}

func Report(result Result, haveResult bool, lines []LedgerLine, cfg Config, wall *time.Duration, in ReportInputs, env func(string) string) string {
	models := cfg.Models
	// The ledger is written by the untrusted run job, so its strings are
	// hostile: only model IDs the trusted config knows are rendered, and
	// everything else groups as "(other)". A crafted model name therefore
	// cannot break out of the comment's markdown, and many distinct names
	// cannot inflate the comment.
	prices := make(map[string]Model, len(models))
	for _, m := range models {
		prices[m.ID] = m
	}
	ledgerModel := func(id string) string {
		if _, ok := prices[id]; ok {
			return id
		}
		return "(other)"
	}
	repairedArgs := 0 // assistant tool-call arguments rewritten to valid JSON (issue #50)
	// Credit spent earlier in the rolling week, from the shared state
	// (WEEK_SPENT_BEFORE — the week total this run started from, the same
	// pattern the old DAY_SPENT_BEFORE had). Missing, empty or invalid
	// renders "?" rather than pretending zero was spent.
	weekBefore := 0.0
	weekKnown := false
	if raw := strings.TrimSpace(env("WEEK_SPENT_BEFORE")); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v) {
			weekBefore, weekKnown = v, true
		}
	}
	// apiCost is what this run would have cost at the API list rates;
	// overage is the part of it spent past the weekly credit. apiPrice uses
	// the ledger's APICostUSD when the line carries one (the source this
	// proxy wrote); lines written before the field existed carry only
	// tokens, and are re-priced from them at the config's api_* rates.
	// Overage replays record()'s rule over the ledger in file order: once
	// the week's credit (before this run + credit so far) is exhausted,
	// later calls are billed at API prices and count against the overage
	// cap. An unknown WEEK_SPENT_BEFORE replays from 0, which under-counts;
	// the proxy's own overage figure (/healthz) is what the caps enforced.
	week, over := weekBefore, false
	apiPrice := func(id string, l LedgerLine) float64 {
		if l.APICostUSD > 0 {
			return l.APICostUSD
		}
		m, known := prices[id]
		if !known || !l.UsageSeen {
			return 0
		}
		usage := Usage{PromptTokens: l.PromptTokens, CompletionTokens: l.CompletionTokens}
		usage.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: l.CachedTokens}
		return ApiCostOf(m, usage)
	}
	var apiCost, overage float64
	totals := map[string]*modelTotals{}
	for _, l := range lines {
		if l.Kind != "" {
			continue // marker lines are not calls
		}
		id := ledgerModel(l.Model)
		t := totals[id]
		if t == nil {
			t = &modelTotals{}
			totals[id] = t
		}
		t.calls++
		t.in += clampTokens(l.PromptTokens)
		t.cached += clampTokens(l.CachedTokens)
		t.out += clampTokens(l.CompletionTokens)
		t.cost += l.CostUSD
		if l.ReasoningTokens != nil {
			t.reasoning += clampTokens(*l.ReasoningTokens)
			t.reasoningKnown = true
		}
		t.reasoningChars += clampTokens(l.ReasoningChars)
		t.outputChars += clampTokens(l.OutputChars)
		repairedArgs += clampTokens(l.RepairedToolArgs)
		if !l.UsageSeen {
			t.unpriced++
		}
		api := apiPrice(id, l)
		apiCost += api
		if over {
			overage += api
		}
		week += l.CostUSD
		if week >= cfg.Limits.WeeklyCreditUSD {
			over = true
		}
	}
	// Timing figures. The proxy_start marker and each call's start..ts span
	// carry them; anything the ledger does not have renders as an em-dash, as
	// it does for ledgers written before timing was recorded.
	var (
		marker, lastEnd time.Time // proxy start, and the last call's end
		spans           []callSpan
		totalOut        int
		slotWait        float64
		haveStarts      bool // the ledger carries per-call timing at all
	)
	now := time.Now()
	for _, l := range lines {
		if l.Kind == "proxy_start" {
			if marker.IsZero() || l.Time.Before(marker) {
				marker = l.Time
			}
			continue
		}
		if l.Kind != "" {
			continue
		}
		totalOut += clampTokens(l.CompletionTokens)
		slotWait += clampSeconds(l.SlotWaitS)
		// A hostile ts must not reach the session figure either.
		if !l.Time.After(now) && l.Time.After(lastEnd) {
			lastEnd = l.Time
		}
		if !l.Start.IsZero() {
			haveStarts = true
			// A span is only believed when it could have happened: it ends by
			// now and lasts at most a day. Spans that start before the
			// proxy_start marker are dropped below, once the marker is known.
			if l.Time.After(l.Start) && !l.Time.After(now) && l.Time.Sub(l.Start) <= maxSpan {
				spans = append(spans, callSpan{start: l.Start, end: l.Time, out: clampTokens(l.CompletionTokens), model: ledgerModel(l.Model)})
			}
		}
	}
	if !marker.IsZero() {
		kept := spans[:0]
		for _, s := range spans {
			if !s.start.Before(marker) {
				kept = append(kept, s)
			}
		}
		spans = kept
	}
	total := ledgerTotal(lines)
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
	// Where the diagnostics artifact lives. The artifact id comes from the
	// API (diagnosticsArtifact), and only digits are believed here: it is
	// rendered into a link, and a hostile id must not be able to break out
	// of it. Without an id the link goes to the run page, which lists the
	// artifacts.
	server := env("GITHUB_SERVER_URL")
	if server == "" {
		server = "https://github.com"
	}
	run := env("GITHUB_RUN_ID")
	diagURL := ""
	if run != "" {
		diagURL = fmt.Sprintf("%s/%s/actions/runs/%s", server, env("GITHUB_REPOSITORY"), run)
		if digitsOnly(in.DiagArtifact) {
			diagURL += "/artifacts/" + in.DiagArtifact
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n### CodeAF run — %s\n\n| | |\n|---|---|\n", reportMarker, outcome)
	if haveResult {
		ok := "?"
		if result.OK != nil {
			ok = strconv.FormatBool(*result.OK)
		}
		fmt.Fprintf(&b, "| stop / ok | %s / `%s` |\n", tokenCell(result.Stop, 40), ok)
		if result.Verdict != "" {
			fmt.Fprintf(&b, "| verdict | %s |\n", tokenCell(result.Verdict, 200))
		}
		if result.Error != nil && result.Error != "" {
			fmt.Fprintf(&b, "| error | %s |\n", escapeMD(fmt.Sprint(result.Error), 300, true))
		}
	} else {
		b.WriteString("| result | CodeAF produced no JSON result; see the workflow logs |\n")
	}
	// Seat models render only if the trusted config knows them, the same rule
	// the ledger table applies; a crafted seat name cannot reach the comment.
	seat := func(id string) string {
		switch {
		case id == "":
			return "—"
		case slices.ContainsFunc(models, func(m Model) bool { return m.ID == id }):
			return "`" + id + "`"
		default:
			return "`(other)`"
		}
	}
	allowed := make([]string, len(models))
	for i, m := range models {
		allowed[i] = "`" + m.ID + "`"
	}
	fmt.Fprintf(&b, "| seats | worker %s, planner %s, checker %s (allowed: %s; no escalation) |\n",
		seat(result.Model), seat(result.PlanModel), seat(result.CheckModel), strings.Join(allowed, ", "))
	fmt.Fprintf(&b, "| CodeAF's own spend figure | %s (plan estimate %s) |\n", money(result.SpendUSD), money(result.EstUSD))
	// The Big-plan metered row (issue #59): what the proxy records is credit
	// — subscription-rate dollars — plus the same calls at API list prices
	// and the overage past the weekly credit. previousReportSection looks
	// this exact label up for continuations, so the label must not change.
	weekCell := "?"
	if weekKnown {
		weekCell = money(weekBefore + total)
	}
	fmt.Fprintf(&b, "| metered by proxy (from its ledger) | **%s** credit this run · %s this week (cap $%s/run, $%s/week) · %s at API prices · %s overage |\n",
		money(total), weekCell, trimNumber(cfg.Limits.RunCreditUSD), trimNumber(cfg.Limits.WeeklyCreditUSD), money(apiCost), money(overage))
	// The quota state synthetic.new reported at proxy start, re-parsed from
	// the ledger's "quotas" marker. Omitted when the marker is absent or
	// its body does not parse; the units are undocumented, so the row says
	// so. Values pass escapeMD: the marker body is untrusted input.
	for _, l := range lines {
		if l.Kind != "quotas" {
			continue
		}
		if q := parseQuotas([]byte(l.Quotas)); q.Known {
			renews := "—"
			if s := escapeMD(q.RenewsAt, 80, true); s != "" {
				renews = s
			}
			fmt.Fprintf(&b, "| synthetic.new quota | limit %s, used %s, renewsAt %s (units undocumented) |\n",
				trimNumber(q.Limit), trimNumber(q.Requests), renews)
		}
		break
	}
	// Web searches through the proxy (issue #58): one row, and only when
	// the run made any. Search lines cost 0 until synthetic.new documents a
	// price; a price would flow through CostUSD into the metered row above.
	if n := countSearches(lines); n > 0 {
		fmt.Fprintf(&b, "| web searches | %d |\n", n)
	}
	if repairedArgs > 0 {
		fmt.Fprintf(&b, "| tool-call arguments repaired | %d |\n", repairedArgs)
	}
	fmt.Fprintf(&b, "| files changed | %s |\n", orUnknown(env("CHANGED")))
	// The ::error:: lines the publish steps collected: the two most recent are
	// the cause of a failed publish. Read as data: the prefix is stripped and
	// the line is escaped and capped, so a crafted error line stays a line.
	if errs := in.PublishErrors; len(errs) > 0 {
		if len(errs) > 2 {
			errs = errs[len(errs)-2:]
		}
		for _, e := range errs {
			e = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(e), "::error::"))
			fmt.Fprintf(&b, "| publish error | %s |\n", escapeMD(e, 200, true))
		}
	}
	b.WriteString("\n")
	// When CodeAF stopped without an answer, its report is all the reviewer
	// has: the stop reason, what the patch changed, the plan's tasks if the
	// result carried them, and the warning that none of it is verified.
	// A run CodeAF called ok is not unfinished, whatever its answer length.
	if strings.TrimSpace(result.Answer) == "" && !(result.OK != nil && *result.OK) {
		b.WriteString("### Unfinished work\n\n")
		switch {
		case !haveResult:
			b.WriteString("CodeAF produced no result: **what it did here is unfinished and unverified.**")
		case result.Stop == "":
			b.WriteString("CodeAF stopped without an answer: **what it did here is unfinished and unverified.**")
		default:
			fmt.Fprintf(&b, "CodeAF stopped (%s) without an answer: **what it did here is unfinished and unverified.**",
				tokenCell(result.Stop, 40))
		}
		if diagURL != "" {
			fmt.Fprintf(&b, " Its [diagnostics](%s) show what it did do.", diagURL)
		}
		b.WriteString("\n\n")
		if len(in.Diff) > 0 || in.DiffMore > 0 || in.DiffNote != "" {
			b.WriteString("Files changed (from the patch's diffstat):\n\n| file | added | removed |\n|---|---:|---:|\n")
			for i, d := range in.Diff {
				if i == maxDiffShown {
					break
				}
				if d.Binary {
					fmt.Fprintf(&b, "| %s (binary) | — | — |\n", escapeMD(d.Path, 200, true))
					continue
				}
				fmt.Fprintf(&b, "| %s | %d | %d |\n", escapeMD(d.Path, 200, true), d.Add, d.Del)
			}
			if rest := in.DiffMore + max(len(in.Diff)-maxDiffShown, 0); rest > 0 {
				fmt.Fprintf(&b, "| … and %d more files | | |\n", rest)
			}
			if in.DiffNote != "" {
				fmt.Fprintf(&b, "| %s | | |\n", escapeMD(in.DiffNote, 200, true))
			}
			b.WriteString("\n")
		}
		if len(in.PlanTasks) > 0 {
			b.WriteString("The plan's tasks (from CodeAF's result, unverified):\n\n| task | state |\n|---|---|\n")
			for i, t := range in.PlanTasks {
				if i == maxPlanTasksShown {
					fmt.Fprintf(&b, "| … and %d more tasks | | |\n", len(in.PlanTasks)-i)
					break
				}
				fmt.Fprintf(&b, "| %s | %s |\n", escapeMD(t.Title, 200, true), tokenCell(t.State, 40))
			}
			b.WriteString("\n")
		}
	}

	if len(totals) > 0 {
		ids := make([]string, 0, len(totals))
		for id := range totals {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return totals[ids[i]].cost > totals[ids[j]].cost })
		b.WriteString("| model | $/M in · cached · out | calls | input tok | cached tok | output tok | reasoning tok | reasoning share | cost |\n")
		b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|---:|\n")
		estimated := false
		for _, id := range ids {
			t := totals[id]
			p, known := prices[id]
			inPrice, cachedPrice, outPrice := "—", "—", "—"
			if known {
				inPrice, cachedPrice, outPrice = trimNumber(p.Input), trimNumber(p.CacheRead), trimNumber(p.Output)
			}
			cost := money(t.cost)
			if t.unpriced > 0 {
				cost += fmt.Sprintf(" (+%d unpriced)", t.unpriced)
			}
			reasoning := "—"
			if t.reasoningKnown {
				reasoning = strconv.Itoa(t.reasoning)
			}
			// The share is exact when reasoning tokens were reported; models
			// that do not send them (GLM) get an estimate from the character
			// counts of reasoning text and visible output (issue #25).
			share := "—"
			switch {
			case t.reasoningKnown:
				share = sharePct(t.reasoning, t.out)
			case t.reasoningChars > 0 || t.outputChars > 0:
				share = "~" + sharePct(t.reasoningChars, t.reasoningChars+t.outputChars) + " (chars)"
				estimated = true
			}
			fmt.Fprintf(&b, "| `%s` | %s · %s · %s | %d | %d | %d | %d | %s | %s | %s |\n", id,
				inPrice, cachedPrice, outPrice, t.calls, t.in, t.cached, t.out, reasoning, share, cost)
		}
		if estimated {
			b.WriteString("“reasoning share” marked (chars) is an estimate: those models send no reasoning_tokens, so it is the reasoning text's share of the characters of reasoning text plus visible output (content and tool-call arguments).\n")
		}
		b.WriteString("\n")
	}
	// The timing table: one row per figure, an em-dash where the ledger or
	// the workflow environment lacks the data.
	wallRow := "—"
	if wall != nil {
		wallRow = fmtDuration(*wall)
	}
	session := "—"
	if !marker.IsZero() && lastEnd.After(marker) {
		if d := lastEnd.Sub(marker); d <= maxSpan {
			session = fmtDuration(d)
		}
	}
	union, at1, at2, at3 := inFlight(spans)
	inFlightRow, splitRow, rateRow := "—", "—", "—"
	if union > 0 {
		inFlightRow = fmtDuration(spanDuration(union))
		splitRow = fmtDuration(spanDuration(at1)) + " · " +
			fmtDuration(spanDuration(at2)) + " · " +
			fmtDuration(spanDuration(at3))
		if totalOut > 0 {
			rateRow = fmt.Sprintf("%.1f tok/s", float64(totalOut)/union)
		}
	}
	// Idle is what #20 asked for explicitly: wall time with no call in
	// flight. It needs both figures; a ledger claiming more in-flight time
	// than the job had clamps at zero.
	idleRow := "—"
	if wall != nil && union > 0 {
		idle := *wall - spanDuration(union)
		if idle < 0 {
			idle = 0
		}
		idleRow = fmtDuration(idle)
	}
	longestRow := "—"
	if len(spans) > 0 {
		longest := spans[0]
		for _, s := range spans[1:] {
			if s.end.Sub(s.start) > longest.end.Sub(longest.start) {
				longest = s
			}
		}
		longestRow = fmt.Sprintf("%s · %d tok · `%s`", fmtDuration(longest.end.Sub(longest.start)), longest.out, longest.model)
	}
	queuedRow := "—"
	if haveStarts {
		queuedRow = fmtDuration(spanDuration(slotWait))
	}
	b.WriteString("| timing | |\n|---|---|\n")
	fmt.Fprintf(&b, "| run job wall time | %s |\n", wallRow)
	fmt.Fprintf(&b, "| CodeAF session (proxy start → last call end) | %s |\n", session)
	fmt.Fprintf(&b, "| time with a call in flight | %s |\n", inFlightRow)
	fmt.Fprintf(&b, "| idle (wall time minus in-flight) | %s |\n", idleRow)
	fmt.Fprintf(&b, "| in-flight split (1 / 2 / 3+ concurrent) | %s |\n", splitRow)
	fmt.Fprintf(&b, "| output tok/s while in flight | %s |\n", rateRow)
	fmt.Fprintf(&b, "| longest single call | %s |\n", longestRow)
	fmt.Fprintf(&b, "| queued at the model limit | %s |\n", queuedRow)
	b.WriteString("\n")
	// The answer is untrusted too: escaped, so it cannot break out of this
	// <details> block or any table above, and capped.
	if result.Answer != "" {
		fmt.Fprintf(&b, "<details><summary>CodeAF's summary</summary>\n\n%s\n\n</details>\n\n", escapeMD(result.Answer, reportMaxAnswer, false))
	}
	labelName := env("CODEAF_LABEL")
	if labelName == "" {
		labelName = "codeaf"
	}
	line := fmt.Sprintf("[Workflow run](%s/%s/actions/runs/%s)", server, env("GITHUB_REPOSITORY"), run)
	if diagURL != "" {
		line += " · [Diagnostics](" + diagURL + ")"
	}
	fmt.Fprintf(&b, "%s · reply `%s <instructions>` on a `%s`-labelled issue/PR to run again.\n", line, command, labelName)
	return b.String()
}

// digitsOnly reports whether s is a plain decimal number, the only shape of
// an artifact id the report believes.
func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// runWallTime asks the GitHub API how long this run's `run` job took, latest
// attempt first. Any problem — a missing env, a failed call, a job that has
// not finished — means no figure, and the report still renders without it.
// The token only goes to the API; nothing here is logged or printed.
func runWallTime() *time.Duration {
	gh := newGitHub()
	run := os.Getenv("GITHUB_RUN_ID")
	if gh.repo == "" || run == "" || gh.token == "" {
		return nil
	}
	var listing struct {
		Jobs []struct {
			Name        string    `json:"name"`
			RunAttempt  int       `json:"run_attempt"`
			StartedAt   time.Time `json:"started_at"`
			CompletedAt time.Time `json:"completed_at"`
		} `json:"jobs"`
	}
	if err := gh.get(fmt.Sprintf("/actions/runs/%s/jobs?per_page=100", run), &listing); err != nil {
		return nil
	}
	found, attempt := false, -1
	var started, completed time.Time
	for _, job := range listing.Jobs {
		if job.Name != "run" || job.StartedAt.IsZero() || job.CompletedAt.IsZero() {
			continue
		}
		if !found || job.RunAttempt > attempt {
			found, attempt = true, job.RunAttempt
			started, completed = job.StartedAt, job.CompletedAt
		}
	}
	if !found || !completed.After(started) {
		return nil
	}
	spent := completed.Sub(started)
	return &spent
}

func runReport() error {
	result, raw, err := readResult()
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
	var in ReportInputs
	if haveResult {
		in.PlanTasks = planTasks(raw)
	}
	in.PublishErrors = readPublishErrors(os.Getenv("PUBLISH_ERRORS_PATH"))
	in.Diff, in.DiffMore, in.DiffNote = patchDiffstat(os.Getenv("PATCH_PATH"))
	in.DiagArtifact = diagnosticsArtifact()
	fmt.Print(Report(result, haveResult, lines, cfg, runWallTime(), in, os.Getenv))
	return nil
}
