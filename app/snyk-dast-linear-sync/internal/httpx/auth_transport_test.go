package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHeaderTransportSetsHeader proves the header the transport is configured
// with arrives at the base with exactly the configured value.
func TestHeaderTransportSetsHeader(t *testing.T) {
	var seen string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Get("Authorization")
		return response(http.StatusOK, "ok"), nil
	})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	transport := &HeaderTransport{Base: base, Key: "Authorization", Value: "JWT test-key"}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if want := "JWT test-key"; seen != want {
		t.Errorf("base saw Authorization %q; want %q", seen, want)
	}
}

// TestHeaderTransportDoesNotMutateCallerRequest proves the caller keeps its
// own header map: the transport works on a clone, so reusing a request must
// not leak the credential back into it.
func TestHeaderTransportDoesNotMutateCallerRequest(t *testing.T) {
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, "ok"), nil
	})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	transport := &HeaderTransport{Base: base, Key: "Authorization", Value: "JWT test-key"}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if _, ok := req.Header["Authorization"]; ok {
		t.Errorf("the caller's request was mutated: %v", req.Header)
	}
}

// TestHeaderTransportReplacesExistingHeader proves a request that already
// carries the configured header arrives with the configured value only, not
// appended next to a stale one.
func TestHeaderTransportReplacesExistingHeader(t *testing.T) {
	var seen []string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Values("Authorization")
		return response(http.StatusOK, "ok"), nil
	})

	req, err := http.NewRequest(http.MethodGet, "http://example.test/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "stale")

	transport := &HeaderTransport{Base: base, Key: "Authorization", Value: "JWT test-key"}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if len(seen) != 1 || seen[0] != "JWT test-key" {
		t.Errorf("base saw Authorization %q; want exactly [\"JWT test-key\"]", seen)
	}
}

// TestHeaderTransportDefaultsToDefaultTransport proves a nil Base falls back
// to http.DefaultTransport, which is how the Linear and Snyk DAST clients are
// built in practice.
func TestHeaderTransportDefaultsToDefaultTransport(t *testing.T) {
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
	transport := &HeaderTransport{Key: "Authorization", Value: "JWT test-key"}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if want := "JWT test-key"; authorization != want {
		t.Errorf("server saw Authorization %q; want %q", authorization, want)
	}
}
