package main

// Check routine "model-prices" (issue #117): compare every model in
// models.json against synthetic.new's public catalog
// (GET https://api.synthetic.new/openai/v1/models, no key needed) and
// report the differences. The catalog carries per-token price strings;
// models.json carries USD per million tokens, so catalog prices are
// converted x1e6 and compared with a small relative tolerance.
//
// modelPricesDiff is pure so TestModelPricesDiff can pin its output on a
// captured fixture; modelPricesCheck only fetches and loads. The required
// diff signature returns an error as well as (changed, report): malformed
// input must come back as an error, not a panic, and cannot be signalled
// through a report string.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	modelPricesURL     = "https://api.synthetic.new/openai/v1/models"
	modelPricesTimeout = 10 * time.Second
	// A few MB is far more than the catalog will ever be (the live
	// catalog is ~13 KB); anything past this is refused, not truncated,
	// so a wrong or hostile response cannot silently pass.
	modelPricesMaxBody = 8 << 20
	// Relative tolerance for price comparison: catalog prices are
	// decimal strings rounded to the nearest token price, so exact float
	// equality is too strict and anything below 1e-9 relative is noise.
	modelPricesTolerance = 1e-9
)

func init() {
	registerCheck("model-prices", modelPricesCheck)
}

// catalogEntry is the subset of an OpenAI-style /openai/v1/models object
// the diff needs. Field names pinned against a live fetch on 2026-10-19:
// pricing values are strings (some with a leading "$"), efforts live under
// reasoning_parameters.
type catalogEntry struct {
	ID            string `json:"id"`
	HuggingFaceID string `json:"hugging_face_id"`
	ContextLength int    `json:"context_length"`
	Reasoning     struct {
		Efforts []string `json:"efforts"`
	} `json:"reasoning_parameters"`
	Pricing struct {
		Prompt          string `json:"prompt"`
		Completion      string `json:"completion"`
		InputCacheReads string `json:"input_cache_reads"`
	} `json:"pricing"`
}

type catalogFile struct {
	Data []catalogEntry `json:"data"`
}

// modelsFile is the subset of models.json the diff needs.
type modelsFile struct {
	Models []modelsEntry `json:"models"`
}

type modelsEntry struct {
	ID            string   `json:"id"`
	Upstream      string   `json:"upstream"`
	ContextLength int      `json:"context_length"`
	Efforts       []string `json:"efforts"`
	APIInput      float64  `json:"api_input"`
	APICached     float64  `json:"api_cached"`
	APIOutput     float64  `json:"api_output"`
}

// modelPricesCheck implements the "model-prices" check command: it fetches
// the public catalog, loads models.json and returns the harness-rendered
// diff report. It runs no model and needs no secret.
func modelPricesCheck() (string, error) {
	client := &http.Client{Timeout: modelPricesTimeout}
	resp, err := client.Get(modelPricesURL)
	if err != nil {
		return "", fmt.Errorf("model-prices: fetching %s: %w", modelPricesURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("model-prices: %s returned HTTP %d", modelPricesURL, resp.StatusCode)
	}
	catalogJSON, err := io.ReadAll(io.LimitReader(resp.Body, modelPricesMaxBody+1))
	if err != nil {
		return "", fmt.Errorf("model-prices: reading catalog response: %w", err)
	}
	if len(catalogJSON) > modelPricesMaxBody {
		return "", fmt.Errorf("model-prices: catalog response larger than %d bytes", modelPricesMaxBody)
	}
	modelsJSON, err := os.ReadFile(configPath())
	if err != nil {
		return "", fmt.Errorf("model-prices: reading %s: %w", configPath(), err)
	}
	changed, report, err := modelPricesDiff(modelsJSON, catalogJSON)
	if err != nil {
		return "", err
	}
	_ = changed // the report already carries the summary line
	return report, nil
}

// modelPricesDiff compares models.json against a captured catalog and
// reports, as one markdown table plus prose lines: removed models (a
// models.json upstream missing from the catalog), context-length and
// efforts mismatches, and price changes (api_input/api_cached/api_output
// vs pricing.prompt/completion/input_cache_reads, per-token x1e6). Catalog
// models absent from models.json and new syn:/infinite: aliases for known
// upstream ids are informational only. changed is true iff any of the
// first four kinds is present.
//
// All strings from the catalog are untrusted data: they go through
// escapeMD and are length-bounded before they reach the report.
func modelPricesDiff(modelsJSON, catalogJSON []byte) (changed bool, report string, err error) {
	var cfg modelsFile
	if err := json.Unmarshal(modelsJSON, &cfg); err != nil {
		return false, "", fmt.Errorf("model-prices: models.json: %w", err)
	}
	if len(cfg.Models) == 0 {
		return false, "", errors.New("model-prices: models.json lists no models")
	}
	var cat catalogFile
	if err := json.Unmarshal(catalogJSON, &cat); err != nil {
		return false, "", fmt.Errorf("model-prices: catalog: %w", err)
	}
	byID := make(map[string]catalogEntry, len(cat.Data))
	for _, e := range cat.Data {
		if e.ID == "" {
			return false, "", errors.New("model-prices: catalog entry with empty id")
		}
		byID[e.ID] = e
	}

	// The catalog's hugging_face_id carries no "hf:" prefix, while a
	// models.json upstream is "hf:<org>/<repo>": match against both forms.
	upstreams := make(map[string]bool, 2*len(cfg.Models))
	for _, m := range cfg.Models {
		upstreams[m.Upstream] = true
		upstreams[strings.TrimPrefix(m.Upstream, "hf:")] = true
	}

	var rows []string
	additions := []string{}
	aliases := []string{}
	aliasIDs := map[string]bool{}

	for _, m := range cfg.Models {
		id := escapeMD(m.Upstream, 200, true)
		e, ok := byID[m.Upstream]
		if !ok {
			rows = append(rows, fmt.Sprintf("| %s | removed | upstream id missing from the catalog |", id))
			continue
		}
		prices := []struct {
			field string
			cat   string
			want  float64
		}{
			{"api_input", e.Pricing.Prompt, m.APIInput},
			{"api_cached", e.Pricing.InputCacheReads, m.APICached},
			{"api_output", e.Pricing.Completion, m.APIOutput},
		}
		for _, p := range prices {
			got, err := parseTokenPrice(p.cat)
			if err != nil {
				return false, "", fmt.Errorf("model-prices: catalog model %s: %w", e.ID, err)
			}
			got *= 1e6 // catalog prices are per token; models.json is per million
			if !priceEqual(p.want, got) {
				rows = append(rows, fmt.Sprintf("| %s | price | %s (pricing per-token x1e6) %s → %s |",
					id, p.field, formatPrice(p.want), formatPrice(got)))
			}
		}
		if e.ContextLength != m.ContextLength {
			rows = append(rows, fmt.Sprintf("| %s | context | %d → %d |",
				id, m.ContextLength, e.ContextLength))
		}
		if !sameEfforts(m.Efforts, e.Reasoning.Efforts) {
			rows = append(rows, fmt.Sprintf("| %s | efforts | models.json [%s] vs catalog [%s] |",
				id, escapeMD(strings.Join(m.Efforts, ", "), 200, true),
				escapeMD(strings.Join(e.Reasoning.Efforts, ", "), 200, true)))
		}
	}

	for _, e := range cat.Data {
		if upstreams[e.ID] {
			continue // the upstream itself, not an alias
		}
		if strings.HasPrefix(e.ID, "syn:") || strings.HasPrefix(e.ID, "infinite:") {
			if upstreams[e.HuggingFaceID] {
				aliases = append(aliases, fmt.Sprintf("- new alias: %s (for upstream %s)",
					escapeMD(e.ID, 200, true), escapeMD(e.HuggingFaceID, 200, true)))
				aliasIDs[e.ID] = true
				continue
			}
		}
		additions = append(additions, fmt.Sprintf("- catalog addition: %s (hugging_face_id %s)",
			escapeMD(e.ID, 200, true), escapeMD(e.HuggingFaceID, 200, true)))
	}
	sort.Strings(additions)
	sort.Strings(aliases)

	changed = len(rows) > 0
	var b strings.Builder
	if changed {
		fmt.Fprintf(&b, "model-prices: %d change(s) between models.json and the catalog (%d models in models.json, %d in the catalog).\n\n",
			len(rows), len(cfg.Models), len(cat.Data))
		b.WriteString("| model | kind | detail |\n|---|---|---|\n")
		for _, r := range rows {
			b.WriteString(r)
			b.WriteByte('\n')
		}
	} else {
		fmt.Fprintf(&b, "model-prices: no changes between models.json and the catalog (%d models in models.json, %d in the catalog).\n",
			len(cfg.Models), len(cat.Data))
	}
	if len(additions)+len(aliases) > 0 {
		b.WriteString("\nInformational (no change flagged):\n")
		for _, a := range additions {
			b.WriteString(a)
			b.WriteByte('\n')
		}
		for _, a := range aliases {
			b.WriteString(a)
			b.WriteByte('\n')
		}
	}
	return changed, b.String(), nil
}

// parseTokenPrice parses a catalog price string ("0.0000006", "$0.0000006"
// or "0") into a per-token float. Catalog strings are untrusted data: a
// value that is not a number is an error, never a zero.
func parseTokenPrice(s string) (float64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "$")
	if s == "" {
		return 0, errors.New("empty price")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("price %q is not a number", s)
	}
	return v, nil
}

// priceEqual compares two per-million prices with a small relative
// tolerance.
func priceEqual(a, b float64) bool {
	if a == b {
		return true
	}
	return math.Abs(a-b) <= modelPricesTolerance*math.Max(math.Abs(a), math.Abs(b))
}

// formatPrice prints a per-million price the way models.json writes it.
func formatPrice(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// sameEfforts compares effort lists as sets: the catalog's order is its
// own and not a difference.
func sameEfforts(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
