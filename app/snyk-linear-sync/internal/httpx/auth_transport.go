package httpx

import (
	"net/http"

	"golang.org/x/oauth2"
)

type BearerTransport struct {
	Base        http.RoundTripper
	TokenSource oauth2.TokenSource
}

func (t *BearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.TokenSource.Token()
	if err != nil {
		return nil, err
	}

	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	cloned.Header.Set("Authorization", "Bearer "+token.AccessToken)

	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	return base.RoundTrip(cloned)
}

type HeaderTransport struct {
	Base  http.RoundTripper
	Key   string
	Value string
}

func (t *HeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	cloned.Header.Set(t.Key, t.Value)

	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	return base.RoundTrip(cloned)
}
