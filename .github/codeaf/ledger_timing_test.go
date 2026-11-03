package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issue #131: the ledger carries the timing the model-stats routine reads.
// TTFTMs, MaxGapMs, DurMs and Cut are the four fields, and the fake upstreams
// below make each one measurable: one sleeps before its first data line and
// again mid-stream, the other ends cleanly with `data: [DONE]`.

// ledgerTimingProxy builds a proxy around an upstream, with a short heartbeat
// so the tests do not wait on the 10s default.
func ledgerTimingProxy(t *testing.T, interval time.Duration, upstream *httptest.Server) (*proxy, string) {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	models := []Model{mustResolve(t, testModels(t), "zai-org/glm-5.3-flash")}
	return &proxy{upstream: upstream.URL, apiKey: "k", ledger: ledger, runCap: 5, weekCap: 20,
		models: models, client: upstream.Client(), slots: newSlots(models, 1),
		heartbeatEvery: interval, headerGrace: time.Second}, ledger
}

// postStream POSTs a streaming call and drains the response, so the relay runs
// to completion before the ledger is read.
func postStream(t *testing.T, url, body string) {
	t.Helper()
	response, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerTiming(t *testing.T) {
	const interval = 20 * time.Millisecond
	// First data line after 4 intervals (the first-token wait), then 8
	// intervals of silence between two data lines (the mid-stream pause). The
	// gaps are far larger than any scheduling jitter, so the assertions below
	// have wide bounds and stay flake-free under -race.
	const headerWaitSpan = 12 * interval
	const afterHeaders = 4 * interval
	const firstWait = headerWaitSpan + afterHeaders
	// The mid-stream pause is longer than the whole pre-first-token wait, so
	// max_gap_ms and ttft_ms cannot be confused for one another.
	const midPause = 24 * interval
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The headers are held back for a span of their own, so header_ms and
		// ttft_ms are different figures: the requirement is that the first
		// byte is the first *data line*, not the header. A proxy that timed
		// ttft from the header commit would report headerWaitSpan here.
		time.Sleep(headerWaitSpan)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// Then nothing while the first token is awaited; the proxy
		// heartbeats, and a comment must not become the first byte.
		time.Sleep(afterHeaders)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(midPause)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":300}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p, ledger := ledgerTimingProxy(t, interval, upstream)
	p.headerGrace = interval // the header wait is committed and heartbeated
	server := httptest.NewServer(p)
	defer server.Close()

	postStream(t, server.URL, `{"model":"zai-org/glm-5.3-flash","stream":true,"messages":[]}`)

	lines, err := readLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("ledger has %d line(s), want 1: %+v", len(lines), lines)
	}
	got := lines[0]
	if !got.Stream || !got.UsageSeen || got.CompletionTokens != 300 {
		t.Fatalf("the call was not recorded as a priced stream: %+v", got)
	}
	// ttft_ms: at least the upstream's first-token sleep, and not the whole
	// call -- and provably not the header wait alone, since upstream committed
	// its headers immediately.
	if got.TTFTMs < int64(firstWait/time.Millisecond) {
		t.Errorf("ttft_ms = %d, want at least %d (the preamble sleep)", got.TTFTMs, firstWait/time.Millisecond)
	}
	if got.TTFTMs >= got.DurMs {
		t.Errorf("ttft_ms = %d, dur_ms = %d: the first token cannot arrive after the call ends", got.TTFTMs, got.DurMs)
	}
	// ttft_ms is the wait for the first data line and nothing earlier. The
	// discriminator is the header hold: the 200 was committed about one
	// heartbeat in (headerGrace = interval), so a proxy that stamped the
	// first byte at the header commit -- or at the last header-wait heartbeat,
	// which `: codeaf-proxy waiting for upstream` comments mark -- would
	// report a figure near headerWaitSpan, not one past it. The lower bound
	// below is the whole pre-first-token wait, header hold plus afterHeaders,
	// which only a stamp at the first data line's own read can reach.
	if got.HeaderMs > got.DurMs {
		t.Errorf("header_ms = %d, dur_ms = %d: the header wait is part of the call", got.HeaderMs, got.DurMs)
	}
	want := int64(headerWaitSpan/time.Millisecond) + int64(afterHeaders/time.Millisecond)
	if got.TTFTMs < want-25 {
		t.Errorf("ttft_ms = %d, want about %d: the header hold (%dms) plus the first-token wait (%dms) -- a smaller figure means the first byte was stamped before the first data line",
			got.TTFTMs, want, headerWaitSpan/time.Millisecond, afterHeaders/time.Millisecond)
	}
	if got.TTFTMs > got.DurMs {
		t.Errorf("ttft_ms = %d, dur_ms = %d: the first token cannot arrive after the call ends", got.TTFTMs, got.DurMs)
	}
	// max_gap_ms: the mid-stream pause, within tolerance. It is far bigger
	// than the heartbeat interval, so a heartbeat comment counted as a
	// streamed line would shrink it below the lower bound.
	if got.MaxGapMs < int64(midPause/time.Millisecond)-20 || got.MaxGapMs > int64(midPause/time.Millisecond)+200 {
		t.Errorf("max_gap_ms = %d, want about %d (the mid-stream pause)", got.MaxGapMs, midPause/time.Millisecond)
	}
	if got.MaxGapMs <= int64(interval/time.Millisecond) {
		t.Errorf("max_gap_ms = %d: no bigger than the heartbeat interval, so a comment line was counted as a streamed line", got.MaxGapMs)
	}
	if got.MaxGapMs < got.TTFTMs {
		t.Errorf("max_gap_ms = %d, ttft_ms = %d: the mid-stream pause here (%dms) is longer than the first-token wait",
			got.MaxGapMs, got.TTFTMs, midPause/time.Millisecond)
	}
	// dur_ms: the first wait plus the pause plus the rest of the call.
	if got.DurMs < int64((firstWait+midPause)/time.Millisecond) {
		t.Errorf("dur_ms = %d, want at least %d", got.DurMs, (firstWait+midPause)/time.Millisecond)
	}
	if got.DurMs > int64((firstWait+midPause+10*interval)/time.Millisecond) {
		t.Errorf("dur_ms = %d, want about %d (no heartbeats bloating the figure)", got.DurMs, (firstWait+midPause)/time.Millisecond)
	}
	// The same fields with no heartbeat anywhere in between: the headers are
	// committed at once and headerGrace is long, so doWithHeartbeats emits no
	// `: codeaf-proxy waiting for upstream` comment at all. header_ms is then
	// the true header time (about 0) while the first token is the whole
	// upstream sleep, so a proxy that stamped ttft at the header commit --
	// or at any comment -- would read about 0 here. This is the case that
	// isolates "first byte is the first data line" from the header wait.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// A leading SSE comment from upstream, which real upstreams send as a
		// keepalive. It is not a token: ttft_ms must ignore it and wait for
		// the data line six intervals later.
		_, _ = io.WriteString(w, ": upstream keepalive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(6 * interval) // headers go out at once; the first token does not
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer plain.Close()

	pp, ledgerP := ledgerTimingProxy(t, interval, plain)
	pp.headerGrace = time.Minute // no header-wait comment can ever be written
	serverP := httptest.NewServer(pp)
	defer serverP.Close()
	postStream(t, serverP.URL, `{"model":"zai-org/glm-5.3-flash","stream":true,"messages":[]}`)

	linesP, err := readLedger(ledgerP)
	if err != nil {
		t.Fatal(err)
	}
	if len(linesP) != 1 {
		t.Fatalf("plain ledger has %d line(s), want 1: %+v", len(linesP), linesP)
	}
	plainLine := linesP[0]
	if plainLine.TTFTMs < int64((6*interval)/time.Millisecond)-20 {
		t.Errorf("ttft_ms = %d, want about %d: with no heartbeat between the request and the first token, a smaller figure means the first byte was stamped before the data line",
			plainLine.TTFTMs, (6*interval)/time.Millisecond)
	}
	// header_ms is not the contrast to assert against: chat stamps it with
	// time.Since(started) after doWithHeartbeats returns, so on a streaming
	// call it is read at the first body bytes, not at the headers -- a
	// pre-existing issue #145 figure, deliberately left alone here (issue
	// #131 is purely additive). The lower bound above is the real check: no
	// comment line exists in this fixture to have been mistaken for the first
	// byte.
	if plainLine.Cut {
		t.Errorf("cut = true for a stream that ended with [DONE]: %+v", plainLine)
	}
	// The upstream comment is a streamed line, so the silence between it and
	// the data line is the one gap in the stream -- and it must not have been
	// taken for the first byte.
	if plainLine.MaxGapMs < int64((6*interval)/time.Millisecond)-20 {
		t.Errorf("max_gap_ms = %d, want about %d (the comment to the data line)",
			plainLine.MaxGapMs, (6*interval)/time.Millisecond)
	}

	// A stream that ended with `data: [DONE]` was not cut.
	if got.Cut {
		t.Errorf("cut = true for a stream that ended with [DONE]: %+v", got)
	}

	// The second fake: no [DONE] at all, so the stream ends on EOF with the
	// content already relayed -- cut, and the timing still recorded.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		time.Sleep(2 * interval)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// The handler returns without [DONE]: upstream dropped the stream.
	}))
	defer short.Close()

	p2, ledger2 := ledgerTimingProxy(t, interval, short)
	server2 := httptest.NewServer(p2)
	defer server2.Close()
	postStream(t, server2.URL, `{"model":"zai-org/glm-5.3-flash","stream":true,"messages":[]}`)

	lines2, err := readLedger(ledger2)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines2) != 1 {
		t.Fatalf("cut ledger has %d line(s), want 1: %+v", len(lines2), lines2)
	}
	cut := lines2[0]
	if !cut.Cut {
		t.Errorf("cut = false for a stream that ended without [DONE]: %+v", cut)
	}
	if cut.TTFTMs < int64((2*interval)/time.Millisecond) || cut.DurMs < cut.TTFTMs {
		t.Errorf("cut call timing = ttft %d dur %d, want ttft >= %d and dur >= ttft", cut.TTFTMs, cut.DurMs, (2*interval)/time.Millisecond)
	}

	// A non-streaming call gets ttft_ms = header_ms, no gap and no cut.
	js := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * interval)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"model":"x","usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer js.Close()

	p3, ledger3 := ledgerTimingProxy(t, interval, js)
	server3 := httptest.NewServer(p3)
	defer server3.Close()
	response, err := http.Post(server3.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"zai-org/glm-5.3-flash","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	lines3, err := readLedger(ledger3)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines3) != 1 {
		t.Fatalf("JSON ledger has %d line(s), want 1: %+v", len(lines3), lines3)
	}
	jsonLine := lines3[0]
	if jsonLine.Stream || jsonLine.Cut || jsonLine.MaxGapMs != 0 {
		t.Errorf("a non-streaming call got stream timing: %+v", jsonLine)
	}
	if jsonLine.TTFTMs != jsonLine.HeaderMs {
		t.Errorf("JSON call ttft_ms = %d, header_ms = %d: with no streamed lines the header wait is the first byte",
			jsonLine.TTFTMs, jsonLine.HeaderMs)
	}
	if jsonLine.TTFTMs < int64((2*interval)/time.Millisecond) || jsonLine.DurMs < jsonLine.TTFTMs {
		t.Errorf("JSON call timing = ttft %d dur %d, want ttft >= %d", jsonLine.TTFTMs, jsonLine.DurMs, (2*interval)/time.Millisecond)
	}

	// The fields are additive: an old-format ledger line without them still
	// parses, reads as a call with no timing, and keeps its other fields.
	old := LedgerLine{Time: time.Now().UTC(), Model: "zai-org/glm-5.3-flash", CompletionTokens: 12, Status: 200}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var back LedgerLine
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.TTFTMs != 0 || back.DurMs != 0 || back.MaxGapMs != 0 || back.Cut {
		t.Errorf("a line without timing parsed back with some: %+v (json %s)", back, raw)
	}
	// And a line with only the new fields set carries exactly those keys.
	raw, err = json.Marshal(LedgerLine{TTFTMs: 1500, DurMs: 9000, MaxGapMs: 400, Cut: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"ttft_ms":1500`, `"dur_ms":9000`, `"max_gap_ms":400`, `"cut":true`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("ledger JSON lacks %s: %s", key, raw)
		}
	}
	// An unset timing writes no keys at all, so old ledgers stay byte-shape
	// compatible for the fields a call did not measure.
	raw, err = json.Marshal(LedgerLine{Model: "zai-org/glm-5.3-flash"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ttft_ms", "dur_ms", "max_gap_ms", "cut"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("a zero timing wrote %q: %s", key, raw)
		}
	}
}

// TestLedgerTimingStreamNeverSaysDone: the client going away mid-stream is a
// cut too -- the relay's read fails on the cancelled body.
func TestLedgerTimingClientGoesAway(t *testing.T) {
	const interval = 20 * time.Millisecond
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// A real first-token wait, so ttft_ms is unambiguously non-zero.
		time.Sleep(4 * interval)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()

	p, ledger := ledgerTimingProxy(t, interval, upstream)
	server := httptest.NewServer(p)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"zai-org/glm-5.3-flash","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Read the first data line, then walk away: the relay's next read fails.
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the first data line: %v", err)
		}
		if strings.HasPrefix(line, "data:") {
			break
		}
	}
	response.Body.Close()
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	var lines []LedgerLine
	for time.Now().Before(deadline) {
		lines, err = readLedger(ledger)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(lines) != 1 {
		t.Fatalf("ledger has %d line(s), want the abandoned call recorded: %+v", len(lines), lines)
	}
	if !lines[0].Cut {
		t.Errorf("cut = false for a stream the client abandoned: %+v", lines[0])
	}
	if lines[0].TTFTMs < int64((4*interval)/time.Millisecond) {
		t.Errorf("ttft_ms = %d, want at least the %dms first-token wait: %+v",
			lines[0].TTFTMs, (4*interval)/time.Millisecond, lines[0])
	}
}

// Issue #456: a call the proxy gives up on before it produces anything is a
// real wait and must reach the ledger. Two paths left chat() before record()
// and wrote nothing, so neither the model-stats slot figures nor the
// abandoned-wait alert could ever see them.
//
// This one abandons the call while it waits for a slot: acquire measured the
// wait and logged it, and the line carries that wait under the abandoned
// stage "before_start", with no usage and no cost. The model's only slot is
// taken directly, so the call is queued in chat() itself and is abandoned
// there -- no upstream is ever contacted.
func TestLedgerTimingAbandonedWhileQueuedForSlot(t *testing.T) {
	const interval = 20 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a call abandoned while queued must never be forwarded upstream")
	}))
	defer upstream.Close()

	p, ledger := ledgerTimingProxy(t, interval, upstream)
	server := httptest.NewServer(p)
	defer server.Close()

	// Hold the model's only slot for the whole test. The slot is shaped here
	// rather than read off models.json, so the test does not hard-code a
	// max_concurrent value.
	m := mustResolve(t, testModels(t), "zai-org/glm-5.3-flash")
	slot := make(chan struct{}, 1)
	p.slots = map[string]chan struct{}{m.Upstream: slot}
	slot <- struct{}{}
	defer func() { <-slot }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*interval)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"zai-org/glm-5.3-flash","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("the queued call should have been abandoned with its client")
	}

	lines := waitForLedger(t, ledger, func(lines []LedgerLine) bool { return len(lines) >= 1 })
	var abandoned []LedgerLine
	for _, l := range lines {
		if l.Kind == "abandoned" {
			abandoned = append(abandoned, l)
		}
	}
	if len(abandoned) != 1 {
		t.Fatalf("ledger has %d abandoned line(s), want the queued call recorded: %+v", len(abandoned), lines)
	}
	got := abandoned[0]
	if got.Abandoned != abandonedBeforeStart {
		t.Errorf("abandoned = %q, want %q: %+v", got.Abandoned, abandonedBeforeStart, got)
	}
	// Half the deadline is the bound: the measured wait must be the real one,
	// not a rounding of it, and the cancel lands within a scheduling tick of
	// the deadline either way.
	if want := (5 * interval).Seconds(); got.SlotWaitS < want {
		t.Errorf("slot_wait_s = %.3f, want at least the %.3fs it queued: %+v", got.SlotWaitS, want, got)
	}
	// It never started: no usage, no cost, no call timing, and not a cut row.
	if got.UsageSeen || got.CostUSD != 0 || got.APICostUSD != 0 || got.Cut || got.TTFTMs != 0 || got.DurMs != 0 {
		t.Errorf("an abandoned-before-start call carries call figures: %+v", got)
	}
}

// TestLedgerTimingAbandonedBeforeHeaders: the call already held its slot and
// its request was issued upstream when the client left during the header
// wait. It used to be a phantom -- no line at all -- so it is recorded now as
// the abandoned stage "before_headers", carrying the header wait it spent.
func TestLedgerTimingAbandonedBeforeHeaders(t *testing.T) {
	const interval = 20 * time.Millisecond
	const cancelAfter = 10 * interval
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No headers until well after the client has gone.
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()

	p, ledger := ledgerTimingProxy(t, interval, upstream)
	// The grace and heartbeat are far longer than the cancel, so the proxy is
	// still waiting for the headers (doWithHeartbeats) when the client goes.
	p.headerGrace = time.Second
	server := httptest.NewServer(p)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), cancelAfter)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"zai-org/glm-5.3-flash","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if response, err := http.DefaultClient.Do(req); err == nil {
		response.Body.Close()
	}

	lines := waitForLedger(t, ledger, func(lines []LedgerLine) bool { return len(lines) >= 1 })
	var abandoned []LedgerLine
	for _, l := range lines {
		if l.Kind == "abandoned" {
			abandoned = append(abandoned, l)
		}
	}
	if len(abandoned) != 1 {
		t.Fatalf("ledger has %d abandoned line(s), want the call abandoned before the headers recorded: %+v",
			len(abandoned), lines)
	}
	got := abandoned[0]
	if got.Abandoned != abandonedBeforeHeaders {
		t.Errorf("abandoned = %q, want %q: %+v", got.Abandoned, abandonedBeforeHeaders, got)
	}
	// Half the deadline, as above: header_ms is truncated to whole
	// milliseconds and the cancel lands within a tick of the deadline.
	if want := (cancelAfter / 2).Milliseconds(); got.HeaderMs < want {
		t.Errorf("header_ms = %d, want at least the %dms the call waited for headers: %+v",
			got.HeaderMs, want, got)
	}
	if !got.Stream {
		t.Errorf("the stream flag was lost: %+v", got)
	}
	// No usage, no cost, no completion timing, and not the mid-stream cut row.
	if got.UsageSeen || got.CostUSD != 0 || got.Cut || got.TTFTMs != 0 || got.DurMs != 0 {
		t.Errorf("an abandoned-before-headers call carries call figures: %+v", got)
	}
}

// waitForLedger polls the ledger until want is satisfied, so a test reads a
// line the handler wrote just after its connection ended.
func waitForLedger(t *testing.T, ledger string, want func([]LedgerLine) bool) []LedgerLine {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		lines, err := readLedger(ledger)
		if err != nil {
			t.Fatal(err)
		}
		if want(lines) || time.Now().After(deadline) {
			return lines
		}
		time.Sleep(10 * time.Millisecond)
	}
}
