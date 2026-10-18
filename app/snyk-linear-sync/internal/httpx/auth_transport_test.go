package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"
)

// tokenSourceFunc adapts a function to oauth2.TokenSource so a test can make
// the token fetch fail.
type tokenSourceFunc func() (*oauth2.Token, error)

func (f tokenSourceFunc) Token() (*oauth2.Token, error) { return f() }

// staticToken is the token every test authenticates with.
func staticToken() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"})
}

func TestBearerTransportSetsAuthorizationHeader(t *testing.T) {
	var seen string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Get("Authorization")
		return response(http.StatusOK, "ok"), nil
	})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/rest/orgs", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if _, err := (&BearerTransport{Base: base, TokenSource: staticToken()}).RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if want := "Bearer test-token"; seen != want {
		t.Errorf("base saw Authorization %q; want %q", seen, want)
	}
	if _, ok := req.Header["Authorization"]; ok {
		t.Errorf("the caller's request was mutated: %v", req.Header)
	}
}

func TestBearerTransportPreservesOtherHeaders(t *testing.T) {
	var seen http.Header
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Clone()
		return response(http.StatusOK, "ok"), nil
	})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/rest/orgs", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept", "application/vnd.api+json")

	if _, err := (&BearerTransport{Base: base, TokenSource: staticToken()}).RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if want := "Bearer test-token"; seen.Get("Authorization") != want {
		t.Errorf("base saw Authorization %q; want %q", seen.Get("Authorization"), want)
	}
	if want := "application/vnd.api+json"; seen.Get("Accept") != want {
		t.Errorf("base saw Accept %q; want %q", seen.Get("Accept"), want)
	}
}

func TestBearerTransportReturnsTokenErrorWithoutSending(t *testing.T) {
	wantErr := errors.New("no token")
	var calls int
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(http.StatusOK, "ok"), nil
	})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/rest/orgs", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	transport := &BearerTransport{
		Base:        base,
		TokenSource: tokenSourceFunc(func() (*oauth2.Token, error) { return nil, wantErr }),
	}
	_, err = transport.RoundTrip(req)

	if !errors.Is(err, wantErr) {
		t.Errorf("RoundTrip error = %v; want %v", err, wantErr)
	}
	if calls != 0 {
		t.Errorf("base was called %d times; want 0: a token error must not send", calls)
	}
}

func TestBearerTransportDefaultsBaseToDefaultTransport(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	// Base is left nil on purpose: the transport must fall back to
	// http.DefaultTransport, which is what actually reaches the server.
	transport := &BearerTransport{TokenSource: staticToken()}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if want := "Bearer test-token"; authorization != want {
		t.Errorf("server saw Authorization %q; want %q", authorization, want)
	}
}

func TestHeaderTransportSetsHeaderOnClone(t *testing.T) {
	var seen http.Header
	var calls int
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		seen = r.Header.Clone()
		return response(http.StatusOK, "ok"), nil
	})

	req, err := http.NewRequest(http.MethodPost, "http://example.test/graphql", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	// A pre-existing header of the same name must be replaced, not appended.
	req.Header.Set("Authorization", "stale-key")

	transport := &HeaderTransport{Base: base, Key: "Authorization", Value: "test-key"}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if calls != 1 {
		t.Fatalf("base was called %d times; want 1", calls)
	}
	if want := "test-key"; seen.Get("Authorization") != want {
		t.Errorf("base saw Authorization %q; want %q", seen.Get("Authorization"), want)
	}
	if got := seen.Values("Authorization"); len(got) != 1 {
		t.Errorf("base saw Authorization values %v; want exactly one", got)
	}
	if got := req.Header.Get("Authorization"); got != "stale-key" {
		t.Errorf("the caller's request was mutated: Authorization = %q; want %q", got, "stale-key")
	}
}

func TestHeaderTransportDefaultsBaseToDefaultTransport(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	// Base is left nil on purpose: the transport must fall back to
	// http.DefaultTransport, which is what actually reaches the server.
	transport := &HeaderTransport{Key: "Authorization", Value: "test-key"}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if want := "test-key"; authorization != want {
		t.Errorf("server saw Authorization %q; want %q", authorization, want)
	}
}

// TestSnykClientStackAuthenticatesEndToEnd assembles the Snyk client's
// transport stack exactly as newSnykClient does -- a BearerTransport over an
// AdaptiveTransport -- and proves the two compose through the real transports
// down to a real server, not only in isolation.
func TestSnykClientStackAuthenticatesEndToEnd(t *testing.T) {
	var calls atomic.Int64
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Mirrors internal/snyk/client.go:57-60: the adaptive transport is the
	// base of the bearer transport, and an AdaptiveTransport built with a nil
	// base falls back to http.DefaultTransport.
	transport := &BearerTransport{
		Base:        NewAdaptiveTransport("snyk", 6, discardLogger(), nil),
		TokenSource: staticToken(),
	}
	client := &http.Client{Transport: transport}

	resp, err := client.Get(server.URL + "/rest/orgs")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want %d", resp.StatusCode, http.StatusOK)
	}
	if want := "Bearer test-token"; authorization != want {
		t.Errorf("server saw Authorization %q; want %q", authorization, want)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server was hit %d times; want 1", got)
	}
}
