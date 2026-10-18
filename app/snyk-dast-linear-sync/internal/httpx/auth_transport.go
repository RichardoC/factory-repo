package httpx

import (
	"net/http"
)

// HeaderTransport sets a fixed header key/value on every outgoing request.
// It is used both for Linear (Authorization: <api key>) and for Snyk DAST
// (Authorization: JWT <api key>), since Snyk DAST's API uses token-based
// authentication with the required "JWT " prefix rather than OAuth.
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
