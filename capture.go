package twitter

import (
	"context"
	"errors"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"

	stealth "github.com/anatolykoptev/go-stealth"
)

// CapturedRequest is one request exactly as an account's client hands it to its
// transport: the header names with their values, and the order list the client
// asks the transport to write them in.
type CapturedRequest struct {
	Method string
	URL    string
	// Headers maps each header name (lower case) to its value.
	Headers map[string]string
	// Order is the order list passed to the transport. Headers named in it are
	// written first, in that order; any other header follows in lexicographic order.
	Order []string
}

// Names returns the header names in the order they are written: those listed in
// Order and present in Headers first, then the rest sorted. Headers the
// transport adds on its own (content-length, a default accept-encoding) are not
// included.
func (r CapturedRequest) Names() []string {
	out := make([]string, 0, len(r.Headers))
	seen := make(map[string]bool, len(r.Headers))
	for _, n := range r.Order {
		if _, ok := r.Headers[n]; ok && !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	var rest []string
	for n := range r.Headers {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// CaptureRequest builds a client for acc's identity (Profile and UserAgent; an
// account that states neither gets the identity NewClient derives for its
// Username), sends one request through a recording transport instead of the
// network, and returns what the client handed to it. Nothing is sent anywhere
// and acc is not modified. The credentials in the request are fixed
// placeholders, and per-request values (a transaction id) are whatever the
// client produced offline. It is meant for tests that pin the exact headers an
// identity sends.
func CaptureRequest(acc *Account, method, rawURL string) (CapturedRequest, error) {
	if acc == nil {
		return CapturedRequest{}, errors.New("twitter: CaptureRequest needs an account")
	}
	target, err := url.Parse(rawURL)
	if err != nil {
		return CapturedRequest{}, err
	}
	probe := &Account{Username: acc.Username, Profile: acc.Profile, UserAgent: acc.UserAgent,
		AuthToken: "captured-auth", CT0: "captured-ct0"}
	rec := &captureBackend{}
	c, err := NewClient(ClientConfig{Accounts: []*Account{probe}, DefaultProxy: "http://127.0.0.1:1",
		SessionStore: &captureSessions{}, stealthOpts: []stealth.ClientOption{stealth.WithBackend(rec.factory)}})
	if err != nil {
		return CapturedRequest{}, err
	}
	defer func() { _ = c.Close() }()

	bc, err := c.clientForAccount(probe)
	if err != nil {
		return CapturedRequest{}, err
	}
	// The response is irrelevant (the recorder answers every request with an
	// empty object); a decode or status error does not make the capture invalid.
	_, _, _, _ = c.doRequestWithBody(context.Background(), probe, bc, method,
		rawURL, twitterHeaders(probe.AuthToken, probe.CT0, probe.UserAgent), nil)
	return rec.find(method, target.Path)
}

// captureSessions is a session store that keeps nothing, so a capture never reads
// or writes the caller's saved sessions.
type captureSessions struct{}

func (*captureSessions) Load(context.Context, string) (Session, error) {
	return Session{}, ErrSessionNotFound
}
func (*captureSessions) Save(context.Context, string, Session) error { return nil }
func (*captureSessions) Delete(context.Context, string) error        { return nil }

// captureBackend is a transport that records every request and answers `{}`.
type captureBackend struct {
	mu   sync.Mutex
	reqs []CapturedRequest
}

func (b *captureBackend) factory(stealth.BackendConfig) (stealth.HTTPDoer, error) { return b, nil }

func (b *captureBackend) Do(req *stealth.Request) (*stealth.Response, error) {
	h := make(map[string]string, len(req.Headers))
	for k, v := range req.Headers {
		h[strings.ToLower(k)] = v
	}
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	b.mu.Lock()
	b.reqs = append(b.reqs, CapturedRequest{Method: req.Method, URL: req.URL, Headers: h,
		Order: append([]string(nil), req.HeaderOrder...)})
	b.mu.Unlock()
	return &stealth.Response{StatusCode: 200, Body: []byte(`{}`), Headers: map[string]string{}}, nil
}

func (b *captureBackend) SetProxy(string) error             { return nil }
func (b *captureBackend) GetCookieValue(_, _ string) string { return "" }

// find returns the last captured request for method and path. The client may
// make other requests of its own (warm-up fetches) around the one asked for.
func (b *captureBackend) find(method, path string) (CapturedRequest, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.reqs) - 1; i >= 0; i-- {
		r := b.reqs[i]
		if u, err := url.Parse(r.URL); err == nil && r.Method == method && u.Path == path {
			return r, nil
		}
	}
	return CapturedRequest{}, errors.New("twitter: the client sent no request for that URL")
}
