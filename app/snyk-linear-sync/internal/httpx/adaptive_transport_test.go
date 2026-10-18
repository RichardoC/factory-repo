package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripFunc adapts a function to http.RoundTripper so tests can script
// responses per call.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// discardLogger keeps the transport's warnings out of the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// readAll drains a response body, closing it.
func readAll(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	if body == nil {
		t.Fatal("response body is nil")
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
	return string(data)
}

func TestRoundTripReturnsSuccessWithZeroRemainingWithoutResend(t *testing.T) {
	previous := defaultBackoff
	defaultBackoff = time.Millisecond
	t.Cleanup(func() { defaultBackoff = previous })

	const resetDelay = 20 * time.Millisecond

	var calls atomic.Int32
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := response(http.StatusOK, `{"data":{"ok":true}}`)
		resp.Header.Set("X-RateLimit-Requests-Remaining", "0")
		// The header carries whole milliseconds; round the encoded instant up
		// so flooring it cannot make the wait shorter than resetDelay.
		resetAt := time.Now().Add(resetDelay).Truncate(time.Millisecond).Add(time.Millisecond)
		resp.Header.Set("X-RateLimit-Requests-Reset", fmt.Sprintf("%d", resetAt.UnixMilli()))
		return resp, nil
	})

	transport := NewAdaptiveTransport("test", 1, discardLogger(), base)
	client := &http.Client{Transport: transport}

	req, err := http.NewRequest(http.MethodPost, "http://example.test/graphql", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The response body is the base's body, returned untouched.
	if got := readAll(t, resp.Body); got != `{"data":{"ok":true}}` {
		t.Fatalf("body = %q, want the base's body", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("base calls = %d, want 1: a 200 with no requests remaining must not be re-sent", got)
	}

	// The header still delays the next request until the reset time.
	start := time.Now()
	second, err := http.NewRequest(http.MethodPost, "http://example.test/graphql", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	secondResp, err := client.Do(second)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	readAll(t, secondResp.Body)

	if elapsed := time.Since(start); elapsed < resetDelay {
		t.Fatalf("second request reached the base after %v, want >= %v", elapsed, resetDelay)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("base calls = %d, want 2", got)
	}
}

func TestRoundTripRetriesOn429UntilSuccess(t *testing.T) {
	previous := defaultBackoff
	defaultBackoff = time.Millisecond
	t.Cleanup(func() { defaultBackoff = previous })

	var calls atomic.Int32
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) <= 2 {
			return response(http.StatusTooManyRequests, "slow down"), nil
		}
		return response(http.StatusOK, "done"), nil
	})

	transport := NewAdaptiveTransport("test", 1, discardLogger(), base)
	req, err := http.NewRequest(http.MethodPost, "http://example.test/graphql", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := readAll(t, resp.Body); got != "done" {
		t.Fatalf("body = %q, want %q", got, "done")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("base calls = %d, want 3", got)
	}
}

func TestRoundTripRewindsBodyOnRetry(t *testing.T) {
	previous := defaultBackoff
	defaultBackoff = time.Millisecond
	t.Cleanup(func() { defaultBackoff = previous })

	var mu sync.Mutex
	var bodies []string
	var calls atomic.Int32
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()

		if calls.Add(1) == 1 {
			return response(http.StatusServiceUnavailable, "unavailable"), nil
		}
		return response(http.StatusOK, "done"), nil
	})

	transport := NewAdaptiveTransport("test", 1, discardLogger(), base)
	req, err := http.NewRequest(http.MethodPost, "http://example.test/graphql", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	readAll(t, resp.Body)

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("base read %d request bodies, want 2", len(bodies))
	}
	for i, got := range bodies {
		if got != "payload" {
			t.Fatalf("body of call %d = %q, want %q", i+1, got, "payload")
		}
	}
}

func TestRoundTripDoesNotRetryWithoutGetBody(t *testing.T) {
	previous := defaultBackoff
	defaultBackoff = time.Millisecond
	t.Cleanup(func() { defaultBackoff = previous })

	var calls atomic.Int32
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusServiceUnavailable, "unavailable"), nil
	})

	transport := NewAdaptiveTransport("test", 1, discardLogger(), base)
	req, err := http.NewRequest(http.MethodPost, "http://example.test/graphql", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// A one-shot body: a retry would re-send nothing, so it must not be tried.
	req.Body = io.NopCloser(strings.NewReader("payload"))
	req.GetBody = nil

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("base calls = %d, want 1", got)
	}
}

func TestRoundTripGivesUpAfterFiveAttempts(t *testing.T) {
	previous := defaultBackoff
	defaultBackoff = time.Millisecond
	t.Cleanup(func() { defaultBackoff = previous })

	var calls atomic.Int32
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusServiceUnavailable, "unavailable"), nil
	})

	transport := NewAdaptiveTransport("test", 1, discardLogger(), base)
	req, err := http.NewRequest(http.MethodPost, "http://example.test/graphql", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	readAll(t, resp.Body)
	if got := calls.Load(); got != 5 {
		t.Fatalf("base calls = %d, want 5", got)
	}
}

func TestRoundTripUsesResetHeaderOn429(t *testing.T) {
	const resetDelay = 30 * time.Millisecond

	// The header carries whole seconds or whole milliseconds. A seconds value
	// means the second the reset falls in, so the delay to encode depends on
	// where the clock is inside the current second.
	cases := []struct {
		name  string
		reset func(time.Time) string
		// resetDelayFor says where to put the requested reset instant: far
		// enough ahead that the header's truncation is only a rounding.
		resetDelayFor func(time.Time) time.Duration
		// encodedAt reports the instant a reset header encoded.
		encodedAt func(time.Time) time.Time
	}{
		{
			name: "milliseconds",
			reset: func(at time.Time) string {
				return fmt.Sprintf("%d", at.UnixMilli())
			},
			resetDelayFor: func(time.Time) time.Duration { return resetDelay },
			encodedAt: func(at time.Time) time.Time {
				return time.UnixMilli(at.UnixMilli())
			},
		},
		{
			name: "seconds",
			reset: func(at time.Time) string {
				return fmt.Sprintf("%d", at.Unix())
			},
			resetDelayFor: func(now time.Time) time.Duration {
				// The header names a whole second, so the reset is the start
				// of the next one; the wait is the distance to that boundary.
				return time.Until(now.Truncate(time.Second).Add(time.Second))
			},
			encodedAt: func(at time.Time) time.Time {
				return time.Unix(at.Unix(), 0)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var retryAt atomic.Int64
			start := time.Now()
			requested := tc.resetDelayFor(start)
			resetAt := start.Add(requested)
			// What the header encodes, after the same truncation it applies.
			encoded := tc.encodedAt(resetAt)

			base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					resp := response(http.StatusTooManyRequests, "slow down")
					resp.Header.Set("X-RateLimit-Requests-Remaining", "0")
					resp.Header.Set("X-RateLimit-Requests-Reset", tc.reset(resetAt))
					return resp, nil
				}
				// Time the retry measured from just before the first call.
				retryAt.Store(time.Now().UnixNano())
				return response(http.StatusOK, "done"), nil
			})

			transport := NewAdaptiveTransport("test", 1, discardLogger(), base)
			req, err := http.NewRequest(http.MethodPost, "http://example.test/graphql", bytes.NewReader([]byte("payload")))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}

			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			readAll(t, resp.Body)

			// Compare against what the header actually encoded, which is
			// never later than the requested instant.
			want := encoded.Sub(start) - time.Millisecond
			elapsed := time.Duration(retryAt.Load() - start.UnixNano())
			if elapsed < want {
				t.Fatalf("retry reached the base after %v, want >= %v", elapsed, want)
			}
		})
	}
}

func TestRoundTripReturnsContextErrorDuringBackoff(t *testing.T) {
	previous := defaultBackoff
	defaultBackoff = time.Second
	t.Cleanup(func() { defaultBackoff = previous })

	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusServiceUnavailable, "unavailable"), nil
	})

	transport := NewAdaptiveTransport("test", 1, discardLogger(), base)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.test/graphql", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = transport.RoundTrip(req)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("RoundTrip returned after %v, want within 100ms", elapsed)
	}
}

func TestRoundTripLimitsConcurrency(t *testing.T) {
	previous := defaultBackoff
	defaultBackoff = time.Millisecond
	t.Cleanup(func() { defaultBackoff = previous })

	const concurrency = 2

	release := make(chan struct{})
	var inFlight, peak atomic.Int32

	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		current := inFlight.Add(1)
		for {
			seen := peak.Load()
			if current <= seen || peak.CompareAndSwap(seen, current) {
				break
			}
		}
		defer inFlight.Add(-1)

		<-release
		return response(http.StatusOK, "done"), nil
	})

	transport := NewAdaptiveTransport("test", concurrency, discardLogger(), base)

	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			req, err := http.NewRequest(http.MethodPost, "http://example.test/graphql", bytes.NewReader([]byte("payload")))
			if err != nil {
				t.Errorf("new request: %v", err)
				return
			}
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Errorf("RoundTrip: %v", err)
				return
			}
			readAll(t, resp.Body)
		})
	}

	// Wait for the transport to be saturated, then release one in-flight call
	// at a time so every goroutine gets through.
	deadline := time.Now().Add(2 * time.Second)
	for peak.Load() < concurrency && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := peak.Load(); got != concurrency {
		close(release)
		wg.Wait()
		t.Fatalf("simultaneous base calls = %d, want %d", got, concurrency)
	}

	for range 6 {
		release <- struct{}{}
	}
	wg.Wait()

	if got := peak.Load(); got != concurrency {
		t.Fatalf("peak simultaneous base calls = %d, want %d", got, concurrency)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	previous := defaultBackoff
	defaultBackoff = 10 * time.Millisecond
	t.Cleanup(func() { defaultBackoff = previous })

	headerWith := func(values map[string]string) http.Header {
		header := http.Header{}
		for key, value := range values {
			header.Set(key, value)
		}
		return header
	}

	cases := []struct {
		name    string
		header  http.Header
		attempt int
		check   func(t *testing.T, got time.Duration)
	}{
		{
			name:   "seconds",
			header: headerWith(map[string]string{"Retry-After": "3"}),
			check: func(t *testing.T, got time.Duration) {
				if got != 3*time.Second {
					t.Fatalf("delay = %v, want 3s", got)
				}
			},
		},
		{
			name:   "http date",
			header: headerWith(map[string]string{"Retry-After": time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)}),
			check: func(t *testing.T, got time.Duration) {
				if got < time.Second || got > 2*time.Second {
					t.Fatalf("delay = %v, want between 1s and 2s", got)
				}
			},
		},
		{
			name:    "absent first attempt",
			header:  headerWith(nil),
			attempt: 0,
			check: func(t *testing.T, got time.Duration) {
				if got != defaultBackoff {
					t.Fatalf("delay = %v, want %v", got, defaultBackoff)
				}
			},
		},
		{
			name:    "absent second attempt",
			header:  headerWith(nil),
			attempt: 1,
			check: func(t *testing.T, got time.Duration) {
				if want := 2 * defaultBackoff; got != want {
					t.Fatalf("delay = %v, want %v", got, want)
				}
			},
		},
		{
			name:    "absent third attempt",
			header:  headerWith(nil),
			attempt: 2,
			check: func(t *testing.T, got time.Duration) {
				if want := 4 * defaultBackoff; got != want {
					t.Fatalf("delay = %v, want %v", got, want)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, retryAfter(tc.header, tc.attempt))
		})
	}
}

func TestRequestLimitDelayParsesSecondsAndMilliseconds(t *testing.T) {
	previous := defaultBackoff
	defaultBackoff = 50 * time.Millisecond
	t.Cleanup(func() { defaultBackoff = previous })

	const tolerance = 100 * time.Millisecond

	cases := []struct {
		name        string
		remaining   string
		reset       *string
		wantLimited bool
		wantExact   *time.Duration
		wantAround  *time.Duration
	}{
		{name: "remaining absent"},
		{name: "remaining above zero", remaining: "5"},
		{
			name:        "remaining zero without reset",
			remaining:   "0",
			wantLimited: true,
			wantExact:   &defaultBackoff,
		},
		{
			name:        "reset in seconds",
			remaining:   "0",
			reset:       new(fmt.Sprintf("%d", time.Now().Add(40*time.Millisecond).Unix())),
			wantLimited: true,
			wantAround:  ptr(40 * time.Millisecond),
		},
		{
			name:        "reset in milliseconds",
			remaining:   "0",
			reset:       new(fmt.Sprintf("%d", time.Now().Add(40*time.Millisecond).UnixMilli())),
			wantLimited: true,
			wantAround:  ptr(40 * time.Millisecond),
		},
		{
			name:        "reset unparsable",
			remaining:   "0",
			reset:       new("not-a-time"),
			wantLimited: true,
			wantExact:   &defaultBackoff,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			if tc.remaining != "" {
				header.Set("X-RateLimit-Requests-Remaining", tc.remaining)
			}
			if tc.reset != nil {
				header.Set("X-RateLimit-Requests-Reset", *tc.reset)
			}

			got, limited := requestLimitDelay(header)
			if limited != tc.wantLimited {
				t.Fatalf("limited = %v, want %v", limited, tc.wantLimited)
			}
			if !limited {
				return
			}
			if tc.wantExact != nil && got != *tc.wantExact {
				t.Fatalf("delay = %v, want %v", got, *tc.wantExact)
			}
			if tc.wantAround != nil {
				if diff := got - *tc.wantAround; diff > tolerance || diff < -tolerance {
					t.Fatalf("delay = %v, want within %v of %v", got, tolerance, *tc.wantAround)
				}
			}
		})
	}
}

//go:fix inline
func ptr[T any](v T) *T { return new(v) }

// TestAdaptiveTransportEndToEndSingleDelivery builds the production stack the
// way the Linear client does and checks that a response carrying "no requests
// remaining" is delivered once, unchanged.
func TestAdaptiveTransportEndToEndSingleDelivery(t *testing.T) {
	previous := defaultBackoff
	defaultBackoff = time.Millisecond
	t.Cleanup(func() { defaultBackoff = previous })

	var hits atomic.Int32
	var mu sync.Mutex
	var authHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("read request body: %v", err)
		}
		mu.Lock()
		authHeader = r.Header.Get("Authorization")
		mu.Unlock()

		w.Header().Set("X-RateLimit-Requests-Remaining", "0")
		w.Header().Set("X-RateLimit-Requests-Reset",
			fmt.Sprintf("%d", time.Now().Add(10*time.Millisecond).UnixMilli()))
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"data":{"ok":true}}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	client := &http.Client{
		Transport: &HeaderTransport{
			Base:  NewAdaptiveTransport("linear", 8, discardLogger(), nil),
			Key:   "Authorization",
			Value: "test-key",
		},
	}

	resp, err := client.Post(server.URL, "application/json", strings.NewReader(`{"query":"mutation"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readAll(t, resp.Body)

	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1", got)
	}
	mu.Lock()
	gotAuth := authHeader
	mu.Unlock()
	if gotAuth != "test-key" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "test-key")
	}
	if body != `{"data":{"ok":true}}` {
		t.Fatalf("body = %q, want the server's body unchanged", body)
	}
}
