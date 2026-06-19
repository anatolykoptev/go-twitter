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

// defaultUserAgent matches the desktop Chrome UA the rest of go-twitter sends so
// warm pages serve the same bundle graph the real client sees.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"

const (
	defaultFetchTimeout = 30 * time.Second
	acceptHeader        = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	acceptLangHeader    = "en-US,en;q=0.9"
	// maxBundleBytes caps the body read — these are adversarial third-party
	// responses. Legitimate warm pages / bundles are a few MB; 32 MiB is safe
	// headroom against a memory-exhaustion body.
	maxBundleBytes = 32 << 20
)

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
		Client:    &http.Client{Timeout: defaultFetchTimeout, Transport: transport},
		UserAgent: defaultUserAgent,
	}, nil
}

// Fetch issues a GET and returns the body, erroring on any non-200 status.
func (f *HTTPFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
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
		ua = defaultUserAgent
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
