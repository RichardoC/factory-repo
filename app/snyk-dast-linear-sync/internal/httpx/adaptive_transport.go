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

const defaultBackoff = 2 * time.Second

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

		if !shouldRetry(resp.StatusCode) && !isRequestRateLimitedResponse(resp) {
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

func shouldRetry(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func isRequestRateLimitedResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}

	_, limited := requestLimitDelay(resp.Header)
	return limited
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
