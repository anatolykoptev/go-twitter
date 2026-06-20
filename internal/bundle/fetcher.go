// Package bundle fetches x.com warm pages, reassembles the webpack chunk map
// (chunkID->name, chunkID->hash), resolves a module name to its abs.twimg.com
// bundle URL, BFS-follows import() refs, and caches the snapshot to disk with a
// TTL. It is the shared core under queryID extraction, xtid on-demand location,
// and feature-flag extraction.
package bundle

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultUserAgent is the single source of truth for the desktop Chrome UA
// go-twitter presents to x.com. It MUST stay consistent with the go-stealth
// Chrome JA3 the BrowserClient emits — a UA major version that disagrees with the
// TLS ClientHello is a trivially fingerprintable bot tell. The twitter package's
// header builder aliases this (see headers.go's defaultUserAgent) so a
// StealthFetcher or HTTPFetcher built without an explicit UA cannot drift to a
// different Chrome version than the rest of the client.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

const (
	defaultFetchTimeout = 30 * time.Second
	acceptHeader        = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	acceptLangHeader    = "en-US,en;q=0.9"
	// maxBundleBytes caps the body read — these are adversarial third-party
	// responses. Legitimate warm pages / bundles are a few MB; 32 MiB is safe
	// headroom against a memory-exhaustion body.
	maxBundleBytes = 32 << 20
)

// allowedBundleHosts is the exact set of hosts the HTTP fetcher may dial or be
// redirected to. x.com warm pages and abs.twimg.com bundles are the only
// legitimate targets; the others are historical aliases. Anything else (a
// compromised response returning 302 Location: http://169.254.169.254/… , a
// link-local / cloud-metadata address, or a downgrade to http) is refused.
var allowedBundleHosts = map[string]bool{
	"abs.twimg.com":   true,
	"x.com":           true,
	"api.twitter.com": true,
	"twitter.com":     true,
}

// allowedBundleHost reports whether host is in the bundle-fetch allowlist.
func allowedBundleHost(host string) bool {
	return allowedBundleHosts[host]
}

// checkBundleRedirect refuses any redirect (or initial hop) whose scheme is not
// https or whose host is not allowlisted. Wired into the HTTP client's
// CheckRedirect so net/http stops following before the disallowed request is
// dialed.
func checkBundleRedirect(req *http.Request, _ []*http.Request) error {
	if req.URL.Scheme != "https" || !allowedBundleHost(req.URL.Hostname()) {
		return fmt.Errorf("blocked redirect to disallowed target %s://%s", req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// Fetcher fetches a URL and returns the body. The runtime wires a stealth-backed
// impl (added in T1); the gql-sync CLI / CI uses HTTPFetcher.
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// HTTPFetcher is the plain net/http impl (CI / offline / codegen). It honors
// HTTPS_PROXY (via the default transport) or an explicit proxy URL so it can
// ride the Webshare pool from a datacenter IP.
type HTTPFetcher struct {
	Client    *http.Client
	UserAgent string

	// skipInitialHostCheck disables the belt-and-suspenders initial-URL
	// allowlist guard in Fetch. Test-only seam (the httptest harness dials
	// 127.0.0.1 over http); the production CheckRedirect SSRF guard is unaffected.
	skipInitialHostCheck bool
}

// NewHTTPFetcher builds an HTTPFetcher. An empty proxyURL falls back to
// http.ProxyFromEnvironment (HTTPS_PROXY / HTTP_PROXY); a non-empty proxyURL
// pins all requests through that proxy.
func NewHTTPFetcher(proxyURL string) (*HTTPFetcher, error) {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy url: %w", err)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &HTTPFetcher{
		Client: &http.Client{
			Timeout:       defaultFetchTimeout,
			Transport:     transport,
			CheckRedirect: checkBundleRedirect,
		},
		UserAgent: DefaultUserAgent,
	}, nil
}

// Fetch issues a GET and returns the body, erroring on any non-200 status.
func (f *HTTPFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	// Belt-and-suspenders: assert the INITIAL resolved URL is https + an
	// allowlisted host before dialing. Today BundleURL only ever yields
	// abs.twimg.com, but a future regex change must not silently re-open SSRF.
	// (Redirects are blocked separately via the client's CheckRedirect.)
	if !f.skipInitialHostCheck {
		if u, perr := url.Parse(rawURL); perr != nil {
			return nil, fmt.Errorf("parse url %s: %w", rawURL, perr)
		} else if u.Scheme != "https" || !allowedBundleHost(u.Hostname()) {
			return nil, fmt.Errorf("refusing to fetch disallowed target %s://%s", u.Scheme, u.Host)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request %s: %w", rawURL, err)
	}
	f.setHeaders(req)

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBundleBytes))
	if err != nil {
		return nil, fmt.Errorf("read body %s: %w", rawURL, err)
	}
	return body, nil
}

func (f *HTTPFetcher) setHeaders(req *http.Request) {
	ua := f.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("Accept-Language", acceptLangHeader)
}

func (f *HTTPFetcher) httpClient() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return http.DefaultClient
}

// stealthDoer is the subset of go-stealth's *BrowserClient that StealthFetcher
// needs. Declaring it here keeps internal/bundle free of a hard go-stealth
// import and makes StealthFetcher unit-testable with a stub.
type stealthDoer interface {
	DoCtx(ctx context.Context, method, urlStr string, headers map[string]string, body io.Reader) ([]byte, map[string]string, int, error)
}

// StealthFetcher fetches warm pages through go-stealth's BrowserClient so the
// runtime rides the same TLS/JA3 fingerprint + proxy as the rest of go-twitter.
// Unauthenticated (guest) is fine — warm pages are public HTML. It satisfies
// Fetcher and is the impl xtid wires at runtime (HTTPFetcher stays the CI/codegen
// path).
type StealthFetcher struct {
	Client    stealthDoer
	UserAgent string
}

// Fetch issues a GET via the BrowserClient and returns the body, erroring on a
// non-200 status (matching HTTPFetcher's contract). The BrowserClient applies
// its configured header order, so no order argument is passed here.
//
// Redirect/host safety here is delegated to go-stealth's BrowserClient (its own
// CheckRedirect / proxy policy); the allowedBundleHost guard above governs only
// the plain net/http HTTPFetcher path.
func (f *StealthFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	ua := f.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	headers := map[string]string{
		"User-Agent":      ua,
		"Accept":          acceptHeader,
		"Accept-Language": acceptLangHeader,
	}
	body, _, status, err := f.Client.DoCtx(ctx, http.MethodGet, rawURL, headers, nil)
	if err != nil {
		return nil, fmt.Errorf("stealth fetch %s: %w", rawURL, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("stealth fetch %s: HTTP %d", rawURL, status)
	}
	return body, nil
}
