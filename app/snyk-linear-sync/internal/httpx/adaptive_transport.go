package httpx

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// defaultBackoff is the first retry delay; it doubles per attempt. A
// variable so tests can shorten it.
var defaultBackoff = 2 * time.Second

type AdaptiveTransport struct {
	name   string
	base   http.RoundTripper
	logger *slog.Logger

	sem chan struct{}

	mu           sync.Mutex
	blockedUntil time.Time
}

func NewAdaptiveTransport(name string, maxConcurrent int, logger *slog.Logger, base http.RoundTripper) *AdaptiveTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	if logger == nil {
		logger = slog.Default()
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	return &AdaptiveTransport{
		name:   name,
		base:   base,
		logger: logger,
		sem:    make(chan struct{}, maxConcurrent),
	}
}

func (t *AdaptiveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.acquire(req.Context()); err != nil {
		return nil, err
	}
	defer t.release()

	attempt := 0
	for {
		if err := t.waitForWindow(req.Context()); err != nil {
			return nil, err
		}

		currentReq, err := cloneRequest(req)
		if err != nil {
			return nil, err
		}

		resp, err := t.base.RoundTrip(currentReq)
		if err != nil {
			return nil, err
		}

		if delay, limited := requestLimitDelay(resp.Header); limited {
			t.noteBlock(delay)
		}

		// Only retryable statuses are retried: 429 for every request, and
		// 502/503/504 for idempotent requests and GraphQL reads (which are
		// POSTs but carry no mark). A request marked non-idempotent -- a
		// GraphQL mutation -- is not re-sent on a gateway error, because
		// the error can arrive after Linear committed the mutation;
		// re-sending would duplicate the write. The caller reconciles the
		// ambiguous outcome instead. A "no requests remaining" header on a
		// successful response must not cause a re-send either: the request
		// that used the last unit of the window succeeded. Its only effect
		// is the noteBlock above, which delays the next request.
		if !shouldRetry(req, resp.StatusCode) {
			return resp, nil
		}

		if !canRetry(req) || attempt >= 4 {
			return resp, nil
		}

		retryDelay := retryAfter(resp.Header, attempt)
		if delay, limited := requestLimitDelay(resp.Header); limited {
			retryDelay = delay
		}
		t.noteBlock(retryDelay)
		t.logger.Warn("rate limited request",
			slog.String("service", t.name),
			slog.Int("status", resp.StatusCode),
			slog.Duration("retry_after", retryDelay),
			slog.Int("attempt", attempt+1),
		)

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		timer := time.NewTimer(retryDelay)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}

		attempt++
	}
}

func (t *AdaptiveTransport) acquire(ctx context.Context) error {
	select {
	case t.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *AdaptiveTransport) release() {
	<-t.sem
}

func (t *AdaptiveTransport) waitForWindow(ctx context.Context) error {
	for {
		t.mu.Lock()
		until := t.blockedUntil
		t.mu.Unlock()

		if until.IsZero() || time.Now().After(until) {
			return nil
		}

		timer := time.NewTimer(time.Until(until))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (t *AdaptiveTransport) noteBlock(delay time.Duration) {
	until := time.Now().Add(delay)

	t.mu.Lock()
	if until.After(t.blockedUntil) {
		t.blockedUntil = until
	}
	t.mu.Unlock()
}

func cloneRequest(req *http.Request) (*http.Request, error) {
	cloned := req.Clone(req.Context())
	if req.Body == nil {
		return cloned, nil
	}
	if req.GetBody == nil {
		return cloned, nil
	}

	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	cloned.Body = body
	return cloned, nil
}

func canRetry(req *http.Request) bool {
	return req.Body == nil || req.GetBody != nil
}

// nonIdempotentKey marks a request whose re-send could duplicate a write.
type nonIdempotentKey struct{}

// WithNonIdempotent marks the request carried by ctx as non-idempotent: one
// whose re-send after a gateway error (502/503/504) could create a duplicate
// write. Only 429 -- where the request was rejected rather than possibly
// applied -- is retried for such a request.
func WithNonIdempotent(ctx context.Context) context.Context {
	return context.WithValue(ctx, nonIdempotentKey{}, true)
}

// isNonIdempotent reports whether req is a non-GET/HEAD request whose context
// carries the WithNonIdempotent mark. A GET or HEAD is always idempotent, so
// the mark is ignored there. It is safe with a nil request or context.
func isNonIdempotent(req *http.Request) bool {
	if req == nil || req.Context() == nil {
		return false
	}
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		return false
	}
	marked, _ := req.Context().Value(nonIdempotentKey{}).(bool)
	return marked
}

// shouldRetry reports whether a response with status may be re-sent for req.
// 429 is always retryable; 502, 503 and 504 are retryable only for
// idempotent requests -- reads, including GraphQL POST reads -- and not for
// requests marked WithNonIdempotent.
func shouldRetry(req *http.Request, status int) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return !isNonIdempotent(req)
	default:
		return false
	}
}

func retryAfter(header http.Header, attempt int) time.Duration {
	raw := header.Get("Retry-After")
	if raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		if when, err := http.ParseTime(raw); err == nil {
			if delay := time.Until(when); delay > 0 {
				return delay
			}
		}
	}

	return time.Duration(math.Pow(2, float64(attempt))) * defaultBackoff
}

func requestLimitDelay(header http.Header) (time.Duration, bool) {
	if header == nil {
		return 0, false
	}

	remaining, err := strconv.ParseInt(header.Get("X-RateLimit-Requests-Remaining"), 10, 64)
	if err != nil || remaining > 0 {
		return 0, false
	}

	resetRaw := header.Get("X-RateLimit-Requests-Reset")
	if resetRaw == "" {
		return defaultBackoff, true
	}

	resetValue, err := strconv.ParseInt(resetRaw, 10, 64)
	if err != nil {
		return defaultBackoff, true
	}

	var resetAt time.Time
	switch {
	case resetValue > 1_000_000_000_000:
		resetAt = time.UnixMilli(resetValue)
	case resetValue > 1_000_000_000:
		resetAt = time.Unix(resetValue, 0)
	default:
		return defaultBackoff, true
	}

	delay := max(time.Until(resetAt), 0)
	return delay, true
}
