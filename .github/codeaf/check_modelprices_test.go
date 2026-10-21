package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestModelPricesDiff(t *testing.T) {
	modelsJSON, err := os.ReadFile("models.json")
	if err != nil {
		t.Fatalf("reading models.json: %v", err)
	}
	catalogJSON, err := os.ReadFile("testdata/models-catalog.json")
	if err != nil {
		t.Fatalf("reading testdata/models-catalog.json: %v", err)
	}

	changed, report, err := modelPricesDiff(modelsJSON, catalogJSON)
	if err != nil {
		t.Fatalf("modelPricesDiff: %v", err)
	}
	if !changed {
		t.Fatal("fixture differs from models.json, got changed=false")
	}
	for _, want := range []string{
		// removed model: zai-org/glm-5.3's upstream is missing from the fixture
		"| hf:zai-org/GLM-5.3 | removed |",
		// price change: Kimi-K3's api_output 15 -> 18
		"api_output", "15 → 18",
		// context-length mismatch on GLM-5.3-Flash
		"| hf:zai-org/GLM-5.3-Flash | context | 524288 → 262144 |",
		// efforts mismatch on GLM-5.3-Flash
		"| hf:zai-org/GLM-5.3-Flash | efforts |",
		// new infinite: alias, informational
		"new alias: infinite:small:text",
		// catalog addition, informational
		"catalog addition: hf:Qwen/Qwen3.8-27B",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q\nreport:\n%s", want, report)
		}
	}
	// informational lines live outside the diff table
	if strings.Contains(report, "| hf:Qwen/Qwen3.8-27B |") ||
		strings.Contains(report, "| infinite:small:text |") {
		t.Errorf("informational catalog entries leaked into the diff table:\n%s", report)
	}

	// A catalog that matches models.json exactly: no change, no table.
	unchanged := unchangedCatalog(t, modelsJSON)
	changed, report, err = modelPricesDiff(modelsJSON, unchanged)
	if err != nil {
		t.Fatalf("modelPricesDiff(unchanged): %v", err)
	}
	if changed {
		t.Errorf("unchanged catalog: got changed=true\nreport:\n%s", report)
	}
	if strings.Contains(report, "|") {
		t.Errorf("unchanged catalog: report should be summary-only, got a table:\n%s", report)
	}
	if !strings.Contains(report, "no changes") {
		t.Errorf("unchanged catalog: report missing summary line:\n%s", report)
	}

	// Aliases and additions alone are informational: an otherwise matching
	// catalog with only a new alias is not a change.
	aliasOnly := strings.Replace(string(unchanged),
		`{"data":[`,
		`{"data":[{"id":"infinite:small:text","hugging_face_id":"deepseek-ai/DeepSeek-V4.1-Flash","context_length":524288,"reasoning_parameters":{"efforts":["none","low","high","xhigh","max"]},"pricing":{"prompt":"0","completion":"0","input_cache_reads":"0"}},`,
		1)
	changed, report, err = modelPricesDiff(modelsJSON, []byte(aliasOnly))
	if err != nil {
		t.Fatalf("modelPricesDiff(aliasOnly): %v", err)
	}
	if changed {
		t.Errorf("alias-only catalog: got changed=true\nreport:\n%s", report)
	}
	if !strings.Contains(report, "new alias: infinite:small:text") {
		t.Errorf("alias-only catalog: alias not reported:\n%s", report)
	}

	// Malformed input returns an error, not a panic.
	if _, _, err := modelPricesDiff([]byte("{ not json"), catalogJSON); err == nil {
		t.Error("malformed models.json: want an error")
	}
	if _, _, err := modelPricesDiff(modelsJSON, []byte("not json")); err == nil {
		t.Error("malformed catalog: want an error")
	}
	wantPrompt := `"prompt":"` + perToken(0.15) + `"`
	if !strings.Contains(string(unchanged), wantPrompt) {
		t.Fatalf("unchanged catalog lacks %s", wantPrompt)
	}
	badPrice := strings.Replace(string(unchanged), wantPrompt, `"prompt":"expensive"`, 1)
	if _, _, err := modelPricesDiff(modelsJSON, []byte(badPrice)); err == nil {
		t.Error("catalog price that is not a number: want an error")
	}
}

// unchangedCatalog builds a catalog that matches models.json exactly, from
// the values models.json itself carries, so the unchanged case cannot drift
// from the real config.
func unchangedCatalog(t *testing.T, modelsJSON []byte) []byte {
	t.Helper()
	var cfg modelsFile
	if err := json.Unmarshal(modelsJSON, &cfg); err != nil {
		t.Fatalf("parsing models.json: %v", err)
	}
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i, m := range cfg.Models {
		if i > 0 {
			b.WriteByte(',')
		}
		id, _ := json.Marshal(m.Upstream)
		hf, _ := json.Marshal(strings.TrimPrefix(m.Upstream, "hf:"))
		b.WriteString(`{"id":` + string(id) +
			`,"hugging_face_id":` + string(hf) +
			`,"context_length":` + strconv.Itoa(m.ContextLength) +
			`,"reasoning_parameters":{"efforts":[`)
		for j, e := range m.Efforts {
			if j > 0 {
				b.WriteByte(',')
			}
			q, _ := json.Marshal(e)
			b.Write(q)
		}
		p, _ := json.Marshal(perToken(m.APIInput))
		c, _ := json.Marshal(perToken(m.APIOutput))
		r, _ := json.Marshal(perToken(m.APICached))
		b.WriteString(`]},"pricing":{"prompt":` + string(p) +
			`,"completion":` + string(c) +
			`,"input_cache_reads":` + string(r) + `}}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}
