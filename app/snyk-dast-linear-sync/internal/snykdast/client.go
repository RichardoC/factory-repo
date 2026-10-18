package snykdast

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
	"github.com/tesslio/snyk-dast-linear-sync/internal/httpx"
)

// Client talks to the Snyk DAST REST API. Snyk DAST uses token-based
// authentication: the API key is sent as `Authorization: JWT <key>`. There is
// no OAuth token exchange (unlike Snyk), so the client is a thin wrapper over
// the shared adaptive HTTP transport.
type Client struct {
	cfg         config.SnykDASTConfig
	httpClient  *http.Client
	apiBase     *url.URL
	logger      *slog.Logger
	concurrency int
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Client, error) {
	apiBase, err := url.Parse(strings.TrimRight(cfg.SnykDAST.APIBase, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("parse Snyk DAST API base URL: %w", err)
	}

	baseTransport := httpx.NewAdaptiveTransport("snyk-dast", cfg.Sync.SnykDASTConcurrency, logger, nil)
	httpClient := &http.Client{
		Transport: &httpx.HeaderTransport{
			Base:  baseTransport,
			Key:   "Authorization",
			Value: "JWT " + cfg.SnykDAST.APIKey,
		},
	}

	return &Client{
		cfg:         cfg.SnykDAST,
		httpClient:  httpClient,
		apiBase:     apiBase,
		logger:      logger,
		concurrency: cfg.Sync.SnykDASTConcurrency,
	}, nil
}

// apiError is a non-2xx response from the Snyk DAST API. Path is the
// endpoint path without the query string: the base host is configuration
// (and must not leak into error excerpts people paste into public issues),
// and the path already identifies the endpoint. Callers that need to react
// to a specific status (for example the correlation fetch mapping 403 to
// ErrCorrelationForbidden) match on Status with errors.As.
type apiError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("Snyk DAST API %s %s failed with %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func (c *Client) decodeJSON(resp *http.Response, into any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return &apiError{
			Method: resp.Request.Method,
			Path:   resp.Request.URL.Path,
			Status: resp.StatusCode,
			Body:   strings.TrimSpace(string(body)),
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("decode Snyk DAST API response: %w", err)
	}
	return nil
}
