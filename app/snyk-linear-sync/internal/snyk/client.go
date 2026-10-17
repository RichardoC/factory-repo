package snyk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	snyksdk "github.com/pavel-snyk/snyk-sdk-go/v2/snyk"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/tesslio/snyk-linear-sync/internal/cache"
	"github.com/tesslio/snyk-linear-sync/internal/config"
	"github.com/tesslio/snyk-linear-sync/internal/httpx"
)

const issuesAPIVersion = "2024-10-15"

type Client struct {
	orgID      string
	httpClient *http.Client
	restBase   *url.URL
	v1Base     *url.URL
	sdk        *snyksdk.Client
	logger     *slog.Logger
	cache      *cache.Store
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Client, error) {
	region, ok := findRegion(cfg.Snyk.Region)
	if !ok {
		return nil, fmt.Errorf("unknown Snyk region %q", cfg.Snyk.Region)
	}

	tokenURL, err := url.Parse(region.RESTBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse region REST URL: %w", err)
	}
	tokenURL.Path = "/oauth2/token"
	tokenURL.RawQuery = ""
	tokenURL.Fragment = ""

	oauthCfg := clientcredentials.Config{
		ClientID:     cfg.Snyk.ClientID,
		ClientSecret: cfg.Snyk.ClientSecret,
		TokenURL:     tokenURL.String(),
		Scopes:       cfg.Snyk.Scopes,
		AuthStyle:    oauth2.AuthStyleInParams,
	}

	baseTransport := httpx.NewAdaptiveTransport("snyk", cfg.Sync.SnykConcurrency, logger, nil)
	httpClient := &http.Client{
		Transport: &httpx.BearerTransport{
			Base:        baseTransport,
			TokenSource: oauthCfg.TokenSource(ctx),
		},
	}

	sdkClient, err := snyksdk.NewClient("oauth-managed", snyksdk.WithHTTPClient(httpClient), snyksdk.WithRegion(region))
	if err != nil {
		return nil, err
	}

	restBase, err := url.Parse(region.RESTBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse region REST URL: %w", err)
	}

	v1Base, err := url.Parse(region.V1BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse region V1 URL: %w", err)
	}

	return &Client{
		orgID:      cfg.Snyk.OrgID,
		httpClient: httpClient,
		restBase:   restBase,
		v1Base:     v1Base,
		sdk:        sdkClient,
		logger:     logger,
	}, nil
}

// SetCache attaches a SQLite cache store so the client can persist v1 ignore
// metadata. This lets the sync survive flaky 404s from the Snyk v1 ignores
// endpoint without losing snooze expiries or disregard-if-fixable flags.
func (c *Client) SetCache(store *cache.Store) {
	c.cache = store
}

func findRegion(alias string) (snyksdk.Region, bool) {
	for _, region := range snyksdk.Regions() {
		if region.Alias == alias {
			return region, true
		}
	}
	return snyksdk.Region{}, false
}

func (c *Client) decodeJSON(resp *http.Response, into any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("snyk API %s %s failed with %d: %s", resp.Request.Method, resp.Request.URL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("decode Snyk API response: %w", err)
	}
	return nil
}
