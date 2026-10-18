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

func (c *Client) decodeJSON(resp *http.Response, into any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Snyk DAST API %s %s failed with %d: %s", resp.Request.Method, resp.Request.URL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("decode Snyk DAST API response: %w", err)
	}
	return nil
}
