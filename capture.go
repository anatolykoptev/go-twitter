package twitter

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"

	stealth "github.com/anatolykoptev/go-stealth"

	"github.com/anatolykoptev/go-twitter/internal/queryid"
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
// placeholders. The transaction id is generated the way it is online (from a
// fixed placeholder page set served to the client's own key fetch), so the header
// is present on every wire; its value changes with each request. The capture
// holds the headers and their order only: HTTP/2 settings, pseudo-header order
// and stream priority sit below the recording seam. It leaves the process-wide
// queryId refresher untouched. It is meant for tests that pin the exact headers
// an identity sends.
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
		SessionStore: &captureSessions{},
		// The default URL guard resolves the host before the backend sees the
		// request; with the recorder in place there is nothing to guard, and the
		// lookup would be the only network traffic of a capture.
		stealthOpts: []stealth.ClientOption{stealth.WithBackend(rec.factory), stealth.WithoutSSRFGuard()}})
	if err != nil {
		return CapturedRequest{}, err
	}
	defer func() { _ = c.Close() }()

	// Keep this client's side effects to itself: key fetches are answered by a
	// fixed page set, the page is not handed to the process-wide queryId
	// refresher, and the client reports to a private, disabled one.
	c.warmDoerOverride = captureWarm{}
	c.xtidMgr.SetWarmPageHook(func(string, []byte) {})
	c.qid = newQIDRefresher(queryid.Config{Disabled: true})

	bc, err := c.clientForAccount(probe)
	if err != nil {
		return CapturedRequest{}, err
	}
	// The response is irrelevant (the recorder answers every request with an
	// empty object); a decode or status error does not make the capture invalid.
	_, _, _, reqErr := c.doRequestWithBody(context.Background(), probe, bc, method,
		rawURL, twitterHeaders(probe.AuthToken, probe.CT0, probe.UserAgent), nil)
	got, err := rec.find(method, target.Path)
	if err != nil && reqErr != nil {
		return CapturedRequest{}, fmt.Errorf("%w: %w", err, reqErr)
	}
	return got, err
}

// captureWarm answers the client's key fetches with a fixed, reconstructed page
// set (a verification key and the two scripts the generator reads), so a capture
// builds its transaction id offline exactly as an online request does.
type captureWarm struct{}

func (captureWarm) DoCtx(_ context.Context, _, u string, _ map[string]string, _ io.Reader) ([]byte, map[string]string, int, error) {
	const (
		base = "https://abs.twimg.com/x-web/x-web/"
		svg  = `<svg id="loading-x-anim-0"><path d="M0 0C1 2 3 4 5 6 7 8 9 10 11 12" fill="#1d9bf008"></path></svg>`
	)
	key := base64.StdEncoding.EncodeToString([]byte{0, 20, 30, 40, 50, 0, 70, 80})
	switch u {
	case "https://x.com/tesla":
		return []byte(`<html><head><meta name="twitter-site-verification" content="` + key + `">` +
			`<script type="module" src="` + base + `entry-client-0000111122223333.js"></script>` +
			`</head><body>` + svg + `</body></html>`), nil, 200, nil
	case base + "entry-client-0000111122223333.js":
		return []byte(`const s=()=>import("./sign.o-0123456789abcdef.js");`), nil, 200, nil
	case base + "sign.o-0123456789abcdef.js":
		return []byte(`(function(){return k(c[0],16)*k(c[1],16)})()`), nil, 200, nil
	}
	return nil, nil, 404, nil
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
