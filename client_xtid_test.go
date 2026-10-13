package twitter

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// deadProxy refuses connections immediately, so any request that reaches the
// real network path fails fast instead of leaving the process.
const deadProxy = "http://127.0.0.1:1"

const (
	testSVG       = `<svg id="loading-x-anim-0"><path d="M0 0C1 2 3 4 5 6 7 8 9 10 11 12" fill="#1d9bf008"></path></svg>`
	testIndicesJS = `(function(){return k(c[0],16)*k(c[1],16)})()`
	testViteBase  = "https://abs.twimg.com/x-web/x-web/"
)

// fakeWarmBackend stands in for x.com: it serves a RECONSTRUCTED authenticated
// Vite page set (not a capture) and records the headers of every request.
type fakeWarmBackend struct {
	mu   sync.Mutex
	seen map[string]map[string]string
}

func (f *fakeWarmBackend) DoCtx(_ context.Context, _, u string, h map[string]string, _ io.Reader) ([]byte, map[string]string, int, error) {
	f.mu.Lock()
	if f.seen == nil {
		f.seen = map[string]map[string]string{}
	}
	f.seen[u] = h
	f.mu.Unlock()
	key := base64.StdEncoding.EncodeToString([]byte{0, 20, 30, 40, 50, 0, 70, 80})
	switch u {
	case "https://x.com/tesla":
		return []byte(`<html><head><meta name="twitter-site-verification" content="` + key + `">` +
			`<script type="module" src="` + testViteBase + `entry-client-0000111122223333.js"></script>` +
			`</head><body>` + testSVG + `</body></html>`), nil, 200, nil
	case testViteBase + "entry-client-0000111122223333.js":
		return []byte(`const s=()=>import("./sign.o-0123456789abcdef.js");`), nil, 200, nil
	case testViteBase + "sign.o-0123456789abcdef.js":
		return []byte(testIndicesJS), nil, 200, nil
	}
	return nil, nil, 404, nil
}

// F5 (client level): an authenticated request through the real doRequestWithBody
// path makes the xtid warm-page fetch carry THAT request's cookie, and the
// request then goes out with an x-client-transaction-id.
func TestDoRequestWithBody_WarmFetchCarriesRequestCookie(t *testing.T) {
	c, err := NewClient(ClientConfig{DefaultProxy: deadProxy})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	backend := &fakeWarmBackend{}
	c.warmDoerOverride = backend

	headers := twitterHeaders("tok-abc", "csrf-xyz", "")
	// The real network leg dies on the dead proxy; only the header assembly matters.
	_, _, _, _ = c.doRequestWithBody(c.client, "GET", "https://x.com/i/api/graphql/x/Op", headers, nil)

	got := backend.seen["https://x.com/tesla"]["cookie"]
	if want := "auth_token=tok-abc; ct0=csrf-xyz"; got != want {
		t.Fatalf("warm-page cookie = %q, want this request's cookie %q", got, want)
	}
	if headers["x-client-transaction-id"] == "" {
		t.Fatal("request went out without x-client-transaction-id although the authenticated init should succeed")
	}
	if c := backend.seen[testViteBase+"entry-client-0000111122223333.js"]["cookie"]; c != "" {
		t.Fatalf("session cookie leaked to the CDN chunk fetch: %q", c)
	}
}

// F2 (client level): after an init failure the pool alert hook receives
// xtid.init_failed exactly once, however many requests follow.
func TestNewClient_InitFailedAlertReachesPoolHookOnce(t *testing.T) {
	var alerts atomic.Int32
	c, err := NewClient(ClientConfig{
		DefaultProxy: deadProxy,
		PoolAlertHook: func(topic string, _ any) {
			if topic == "xtid.init_failed" {
				alerts.Add(1)
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for i := 0; i < 3; i++ {
		h := twitterHeaders("", "", "") // guest: no auth_token
		_, _, _, _ = c.doRequestWithBody(c.client, "GET", fmt.Sprintf("https://x.com/i/api/%d", i), h, nil)
	}
	if got := alerts.Load(); got != 1 {
		t.Fatalf("xtid.init_failed reached PoolAlertHook %d times, want exactly 1", got)
	}
}
