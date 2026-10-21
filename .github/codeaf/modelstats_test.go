// Tests for the deterministic core of `harness model-stats` (issue #131).
//
// The fixture is deliberately small and hand-checked: the percentiles, the
// rates and every alert are computed by hand in the test's own comment, so a
// change in the routine that shifts a figure fails here rather than in a
// report somebody reads later.
package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestModelStatsTables: a small fixture of ledger lines yields exact
// expected tables, percentiles and alerts. The rendered text is compared in
// full, not in substrings, so an extra table, a reordered column or a
// silently dropped alert all fail.
func TestModelStatsTables(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)

	// The fixture, in one run (zip). Model ids are the two models.json
	// carries in the test config; the timestamps are inside the window
	// (now-72h .. now).
	//
	//   alpha: 6 calls, all completing.
	//     ttft 1000,1000,1000,1000,1000,1000 -> p50=p90=p99=1000 (nearest rank)
	//     decode: only completions with >= 300 tokens count. dur-ttft:
	//       c1 9000-1000=8000ms, 1000 tok -> 125.0 tok/s
	//       c2 113000-1000=112000ms, 1000 tok -> 8.92857 tok/s
	//       c3 29000-1000=28000ms, 1000 tok -> 35.71428 tok/s
	//       p50 of [125, 8.928571, 35.714286] sorted -> [8.928571, 35.714286, 125]
	//       nearest rank p50 = sorted[ceil(0.5*3)-1] = sorted[1] = 35.714286
	//     e2e tok/s: 1000/9 = 111.111; 1000/113 = 8.8495; 1000/29 = 34.4828 ...
	//       p50 = sorted[2] of 6 values.
	//   beta: 29 calls with ttft 2000 and a completion of 0 tokens on one,
	//     so beta has 28 completions, one abandoned slot wait (wait 5 s and
	//     no completion) and 29 calls. beta completes 1000 tokens in
	//     12000-2000=10000 ms -> 100.0 tok/s; one completion is under the
	//     300-token decode floor? No: 1000 >= 300, so all 28 count, and the
	//     p50 is 100.0. beta's e2e p50: 1000/12 = 83.333 tok/s, with the
	//     zero-token call at 0 -> p50 = sorted[14] = 83.333.
	lines := []string{
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"duration_note":"ignored","ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":113000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":29000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":10,"usage_seen":true,"ttft_ms":1000,"dur_ms":2000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":10,"usage_seen":true,"ttft_ms":1000,"dur_ms":2000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":10,"usage_seen":true,"ttft_ms":1000,"dur_ms":2000}`,
	}
	// beta: 29 calls at 2026-10-21T10:00Z (one hour bucket, under the 30-call
	// hourly floor, so the hourly p50 alert cannot fire on it).
	for i := 0; i < 29; i++ {
		tok, seen := 1000, "true"
		if i == 0 {
			tok, seen = 0, "false"
		}
		lines = append(lines, fmt.Sprintf(
			`{"ts":"2026-10-21T10:00:00Z","model":"beta","completion_tokens":%d,"usage_seen":%s,"ttft_ms":2000,"dur_ms":12000}`,
			tok, seen))
		if i == 0 {
			// The abandoned wait: a call that queued 5 s for a slot and then
			// produced no completion at all.
			lines = append(lines, `{"ts":"2026-10-21T10:00:00Z","model":"beta","completion_tokens":0,"usage_seen":false,"ttft_ms":2000,"dur_ms":12000,"slot_wait_s":5}`)
		}
	}
	// A status marker: a 429 the proxy never completed.
	lines = append(lines, `{"ts":"2026-10-21T10:00:00Z","model":"beta","kind":"status","status":429}`)
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")

	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	if in.RunCount != 1 || in.Stats.Rows != 6+29+1 || in.Stats.Rejected != 0 {
		t.Fatalf("window: runs=%d rows=%d rejected=%d, want 1/36/0", in.RunCount, in.Stats.Rows, in.Stats.Rejected)
	}
	got := modelStatsReport(in, cfg, win, now)
	const goldenPath = "testdata/modelstats/tables.md"
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v (the golden is the hand-checked rendering; regenerate it only from a hand-checked diff)", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("rendered report differs from %s\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
	}
	// The figures the tables have to carry, stated independently of the
	// golden file so a regenerated golden cannot hide a wrong number.
	for _, wantLine := range []string{
		"| `alpha` | 6 | 1 | 1.0 s | 1.0 s | 1.0 s | 35.7 (8.9-125.0) | 34.5 | 0 | 0 | 0 | 0 | 0.00% | 0 | 0.0 | 0 |",
		"| `beta` | 30 | 1 | 2.0 s | 2.0 s | 2.0 s | 100.0 (100.0-100.0) | 83.3 | 0 | 1 | 1 | 0 | 0.00% | 1 | 5.0 | 1 |",
		"| 2026-10-21 10:00 | `beta` | 30 | 2.0 s |",
		"| 2026-10-21 11:00 | `alpha` | 6 | 1.0 s |",
		"Rows: 36 accepted, 0 rejected (0.00%, threshold 1.00%) across 1 run(s) with a ledger.",
		"`beta`: 1 abandoned slot wait(s)",
	} {
		if !strings.Contains(got, wantLine) {
			t.Errorf("report is missing %q\n%s", wantLine, got)
		}
	}
	if !strings.Contains(got, "### Alerts (3)") {
		t.Errorf("expected exactly one alert (the abandoned wait), got:\n%s", got)
	}
	if !strings.Contains(got, "`gamma`") {
		t.Errorf("a configured model with no calls was not reported in the tables:\n%s", got)
	}
}

// TestModelStatsStatusIsNotAMarkerUnlessKindSaysSo: the proxy writes a
// status line only with kind "status" (record, main.go), and a successful
// call carries no kind. A call that nevertheless records an explicit status
// -- the ledger this routine reads -- must still count as a call: keying the
// marker branch off the status number alone dropped it from every table and
// charged it to the error class (found while running the issue #131
// end-to-end check on a directory of hand-built zips).
func TestModelStatsStatusIsNotAMarkerUnlessKindSaysSo(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)

	// One call with an explicit status 200, one marker, one plain call.
	lines := []string{
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","status":200,"completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","kind":"status","status":429}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
	}
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")

	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	// Both calls count; the marker is not a call and is not a rejection.
	if in.Stats.Rows != 2 || in.Stats.Rejected != 0 {
		t.Fatalf("status 200 call was not counted: rows=%d rejected=%d, want 2/0 (classes=%v)",
			in.Stats.Rows, in.Stats.Rejected, in.Stats.RejectedClass)
	}
	got := modelStatsReport(in, cfg, win, now)
	// The call's status 200 is a success: no error, no 429 from it. The
	// marker's 429 is the one 429.
	if !strings.Contains(got, "| `alpha` | 2 | 1 | 1.0 s |") {
		t.Errorf("the status-200 call is missing from the model table:\n%s", got)
	}
	// The status-200 call must not be charged to a class; the marker's 429
	// is the only 429 and the only error (Errs sums the 4xx/5xx/other/429
	// classes). Columns after e2e tok/s are: cut | 429 | error | stall.
	if !strings.Contains(got, "| 0 | 1 | 1 | 0 |") {
		t.Errorf("want cut=0, one 429 (the marker), one error (that same 429) and no stall, got:\n%s", got)
	}
	if strings.Contains(got, "above the 1.00% threshold") == false {
		// 429 rate is 1 of (2 calls + 1 marker) = 33.33%: the alert must
		// still fire, and the denominator must include the marker as a call
		// the proxy never completed.
		t.Errorf("the 429 alert did not fire on a 33.33%% rate:\n%s", got)
	}
}

// modelStatsTestConfig is a small config built in the test, never read from
// models.json (AGENTS.md: a test builds its own config).
func modelStatsTestConfig(t *testing.T) Config {
	t.Helper()
	alerts, err := validateAlerts(Alerts{})
	if err != nil {
		t.Fatalf("validateAlerts: %v", err)
	}
	return Config{Alerts: alerts, Models: []Model{{ID: "alpha"}, {ID: "beta"}, {ID: "gamma"}}}
}

// modelStatsTestZip builds an in-memory zip, the way a downloaded
// codeaf-output artifact arrives -- there is no fixture on disk and nothing
// is extracted.
func modelStatsTestZip(t *testing.T, entries ...string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i+1 < len(entries); i += 2 {
		w, err := zw.Create(entries[i])
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write([]byte(entries[i+1])); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}
	return zr
}

// TestModelStatsDeterministic: the same inputs yield byte-identical output,
// which is what makes the comment safe to edit in place.
func TestModelStatsDeterministic(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join([]string{
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T10:00:00Z","model":"beta","completion_tokens":500,"usage_seen":true,"ttft_ms":1500,"dur_ms":5000}`,
	}, "\n")+"\n")
	first := modelStatsReport(parseModelStatsZips([]*zip.Reader{zr}, cfg), cfg, win, now)
	for i := 0; i < 3; i++ {
		again := modelStatsReport(parseModelStatsZips([]*zip.Reader{zr}, cfg), cfg, win, now)
		if again != first {
			t.Fatalf("report %d differs from the first rendering", i+2)
		}
	}
}

// TestModelStatsRejectsHostileRows: oversized lines, unknown models,
// negative or huge numbers, and markdown or @-mentions in any field are
// either rejected or never echoed, and the rejected count is reported.
func TestModelStatsRejectsHostileRows(t *testing.T) {
	cfg := modelStatsTestConfig(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	win := modelStatsWindowFrom(cfg, now)

	hostileMarkdown := "@everyone **pwn** [click](https://example.invalid) `code` <b>x</b>"
	lines := []string{
		// Accepted: one honest call, the baseline the reject count is read
		// against.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":1000,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		// A free-text field carrying markdown and at-mentions, alongside the
		// valid fields: the row is accepted, and nothing of it is echoed.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","upstream":"` + hostileMarkdown + `","completion_tokens":1000,"usage_seen":true,"ttft_ms":1500,"dur_ms":9000,"kind":"` + hostileMarkdown + `"}`,
		// An unknown model id, and an id that is not a string at all.
		`{"ts":"2026-10-21T11:00:00Z","model":"` + hostileMarkdown + `","ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":12345,"ttft_ms":1000,"dur_ms":9000}`,
		// Negative and huge numbers.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":-1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":9999999999999,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":-5,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","completion_tokens":999999999999,"usage_seen":true,"ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","slot_wait_s":-1,"ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","slot_wait_s":1e12,"ttft_ms":1000,"dur_ms":9000}`,
		// A status outside 100..599.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","kind":"status","status":999}`,
		// A first token after the call ended.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":9000,"dur_ms":1000}`,
		// No timestamp, a timestamp in another century, and a line that is
		// not JSON at all.
		`{"model":"alpha","ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"1901-01-01T00:00:00Z","model":"alpha","ttft_ms":1000,"dur_ms":9000}`,
		`{"ts":"<img src=x onerror=alert(1)>","model":"alpha"`,
		// An array and a bare string are not ledger lines.
		`[1,2,3]`,
		`"` + hostileMarkdown + `"`,
		// An unterminated line longer than the 64 KiB cap.
		`{"ts":"2026-10-21T11:00:00Z","model":"alpha","note":"` + strings.Repeat("A", 70*1024) + `"}`,
	}
	zr := modelStatsTestZip(t, "codeaf-ledger.jsonl", strings.Join(lines, "\n")+"\n")
	in := parseModelStatsZips([]*zip.Reader{zr}, cfg)
	got := modelStatsReport(in, cfg, win, now)

	// Two rows survive: the honest call and the one whose markdown sits in
	// fields the report never reads.
	if in.Stats.Rows != 2 {
		t.Errorf("accepted rows = %d, want 2:\n%s", in.Stats.Rows, got)
	}
	wantRejected := len(lines) - 2
	if in.Stats.Rejected != wantRejected {
		t.Errorf("rejected rows = %d, want %d", in.Stats.Rejected, wantRejected)
	}
	// The rejected count and its share are in the report, and over the 1%
	// threshold the report alerts.
	if !strings.Contains(got, fmt.Sprintf("%d rejected", wantRejected)) {
		t.Errorf("report does not carry the rejected count:\n%s", got)
	}
	if !strings.Contains(got, "above the 1.00% threshold") {
		t.Errorf("the rejected share should alert (it is far over 1%%):\n%s", got)
	}
	// Nothing hostile is echoed: not the markdown, not the at-mention, not
	// the model name, not the oversized line.
	for _, leaked := range []string{"everyone", "pwn", "click", "example.invalid", "onerror", "<b>", "<img", strings.Repeat("A", 100)} {
		if strings.Contains(got, leaked) {
			t.Errorf("report echoes hostile input %q:\n%s", leaked, got)
		}
	}
	// Every reason the reader states is its own text.
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "- ") && !strings.HasPrefix(l, "- **") && !strings.Contains(l, "rejected") &&
			!strings.Contains(l, "model not in") && !strings.Contains(l, "outside") &&
			!strings.Contains(l, "not a JSON") && !strings.Contains(l, "longer than") &&
			!strings.Contains(l, "timestamp") && !strings.Contains(l, "not finite") &&
			!strings.Contains(l, "s:") && !strings.Contains(l, "skipped") {
			t.Errorf("unexpected reported reason %q", l)
		}
	}
}

// TestModelStatsZipBounds: a zip bomb and too many entries are refused
// without extracting.
func TestModelStatsZipBounds(t *testing.T) {
	allowed := map[string]bool{"alpha": true}
	b := defaultModelStatsBounds()

	// Too many entries: a zip with more files than any artifact has. It is
	// refused whole -- not read until the cap -- and named by count.
	many := []string{}
	for i := 0; i < 3; i++ {
		many = append(many, fmt.Sprintf("junk-%d.txt", i), "x")
	}
	manyZip := modelStatsTestZip(t, many...)
	tight := defaultModelStatsBounds()
	tight.MaxEntries = 2
	in := parseModelStatsWithBounds([]*zip.Reader{manyZip}, allowed, tight)
	if in.RunCount != 0 || in.Stats.Zips != 0 || in.Stats.ZipSkipped != 1 {
		t.Fatalf("many-entry zip: runs=%d zips=%d skipped=%d, want 0/0/1", in.RunCount, in.Stats.Zips, in.Stats.ZipSkipped)
	}
	if len(in.Stats.SkippedReason) != 1 || !strings.Contains(in.Stats.SkippedReason[0], "over the 2-entry cap") {
		t.Fatalf("many-entry zip reason = %v", in.Stats.SkippedReason)
	}

	// A zip bomb: a tiny compressed entry whose header claims far more than
	// the uncompressed cap. The reader must refuse it on the header alone,
	// so it is never decompressed.
	bomb := modelStatsTestZip(t, modelStatsLedgerName, strings.Repeat("A", 4096))
	if len(bomb.File) != 1 || bomb.File[0].UncompressedSize64 < 4096 {
		t.Fatalf("fixture is not a bomb: %+v", bomb.File)
	}
	small := defaultModelStatsBounds()
	small.MaxEntryUnc = 1024
	small.MaxEntryComp = 1 << 20
	in = parseModelStatsWithBounds([]*zip.Reader{bomb}, allowed, small)
	if len(in.Rows) != 0 {
		t.Fatalf("bomb produced %d rows, want 0", len(in.Rows))
	}
	if len(in.Stats.SkippedReason) != 1 || !strings.Contains(in.Stats.SkippedReason[0], "uncompressed size over the cap") {
		t.Fatalf("bomb reason = %v", in.Stats.SkippedReason)
	}

	// A compressed-size cap refuses before inflation too.
	comp := defaultModelStatsBounds()
	comp.MaxEntryComp = 1
	in = parseModelStatsWithBounds([]*zip.Reader{bomb}, allowed, comp)
	if len(in.Rows) != 0 || len(in.Stats.SkippedReason) != 1 ||
		!strings.Contains(in.Stats.SkippedReason[0], "compressed size over the cap") {
		t.Fatalf("compressed cap: rows=%d reasons=%v", len(in.Rows), in.Stats.SkippedReason)
	}

	// An entry the header accepts is still read under a cap: the reader never
	// inflates more than MaxEntryUnc bytes of it, and refuses it outright when
	// the stream reaches the cap. A zip whose deflate stream expands past
	// what its header promised (a hand-built archive, or one a future Go
	// version reports differently) therefore cannot inflate without bound.
	big := modelStatsTestZip(t, modelStatsLedgerName,
		strings.Repeat(`{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":1,"dur_ms":2}`+"\n", 500))
	liar := defaultModelStatsBounds()
	liar.MaxEntryUnc = 64 * int64(zipReaderUncompressedSize(big)) // the header passes...
	in = parseModelStatsWithBounds([]*zip.Reader{big}, allowed, liar)
	if len(in.Rows) == 0 {
		t.Fatalf("read cap: expected the accepted entry to be read, got reasons=%v", in.Stats.SkippedReason)
	}
	tiny := defaultModelStatsBounds()
	tiny.MaxEntryUnc = int64(zipReaderUncompressedSize(big))
	in = parseModelStatsWithBounds([]*zip.Reader{big}, allowed, tiny)
	if len(in.Rows) != 0 {
		t.Fatalf("read cap: %d rows past a cap the stream reaches, want 0", len(in.Rows))
	}
	if len(in.Stats.SkippedReason) == 0 || !strings.Contains(in.Stats.SkippedReason[0], "inflated past the cap") {
		t.Fatalf("read cap: reasons=%v", in.Stats.SkippedReason)
	}

	// And the ordinary caps hold: 20k rows in one run, and a bounded total.
	rows := []string{}
	for i := 0; i < b.MaxRunRows+10; i++ {
		rows = append(rows, `{"ts":"2026-10-21T11:00:00Z","model":"alpha","ttft_ms":1,"dur_ms":2}`)
	}
	run := modelStatsTestZip(t, modelStatsLedgerName, strings.Join(rows, "\n")+"\n")
	in = parseModelStatsWithBounds([]*zip.Reader{run}, allowed, b)
	if len(in.Rows) != b.MaxRunRows {
		t.Fatalf("row cap: got %d rows, want %d", len(in.Rows), b.MaxRunRows)
	}
}

// zipReaderUncompressedSize is the size the reader sees on a test zip's
// single entry header, which the caps in these tests are set against.
func zipReaderUncompressedSize(zr *zip.Reader) uint64 {
	if len(zr.File) == 0 {
		return 0
	}
	return zr.File[0].UncompressedSize64
}
