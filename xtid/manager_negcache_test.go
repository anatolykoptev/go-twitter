package xtid

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// exactFetcher serves exact URLs from a swappable route table and counts every
// fetch per URL. Unknown URLs fail with a non-retryable "HTTP 404".
type exactFetcher struct {
	mu     sync.Mutex
	routes map[string][]byte
	counts map[string]int
}

func newExactFetcher(routes map[string][]byte) *exactFetcher {
	return &exactFetcher{routes: routes, counts: map[string]int{}}
}

func (e *exactFetcher) set(routes map[string][]byte) {
	e.mu.Lock()
	e.routes = routes
	e.mu.Unlock()
}

func (e *exactFetcher) count(u string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.counts[u]
}

func (e *exactFetcher) Fetch(_ context.Context, u string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.counts[u]++
	if b, ok := e.routes[u]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("exact stub: %s: HTTP 404", u)
}

const viteBase = "https://abs.twimg.com/x-web/x-web/"

// viteShellHTML is the logged-out Vite shell seen in production (#57): no
// bundle references, only the logged-out entry script.
func viteShellHTML() []byte {
	return []byte(`<!DOCTYPE html><html><head>` +
		`<script type="module" crossorigin src="` + viteBase + `entry-client-logged-out-aaaabbbbccccdddd.js"></script>` +
		`</head><body><div id="react-root"></div></body></html>`)
}

func newTestManager(base *exactFetcher) *Manager {
	m := NewManager(base)
	m.cacheDir = ""
	return m
}

// F1: 50 concurrent GenerateID while the warm page is the empty Vite shell ->
// exactly one warm-page fetch; a second burst inside the backoff adds none; a
// burst after the backoff retries exactly once more.
func TestGenerateID_NegativeCacheAndSingleflight(t *testing.T) {
	stub := newExactFetcher(map[string][]byte{warmPageURL: viteShellHTML()})
	mgr := newTestManager(stub)
	clock := time.Now()
	mgr.now = func() time.Time { return clock }

	burst := func() {
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := mgr.GenerateID("GET", "/i/api/graphql/x/Op"); err == nil {
					t.Error("GenerateID succeeded against the logged-out shell")
				}
			}()
		}
		wg.Wait()
	}

	burst()
	if got := stub.count(warmPageURL); got != 1 {
		t.Fatalf("warm page fetched %d times after first burst, want exactly 1", got)
	}
	burst()
	if got := stub.count(warmPageURL); got != 1 {
		t.Fatalf("warm page fetched %d times after second burst inside backoff, want 1", got)
	}

	clock = clock.Add(defaultBackoffBase + time.Second)
	burst()
	if got := stub.count(warmPageURL); got != 2 {
		t.Fatalf("warm page fetched %d times after backoff expiry, want 2", got)
	}
	// Second consecutive failure doubles the wait: 61s later is still blocked.
	clock = clock.Add(defaultBackoffBase + time.Second)
	burst()
	if got := stub.count(warmPageURL); got != 2 {
		t.Fatalf("backoff did not grow: %d fetches, want 2", got)
	}
}

// F2: the alert fires once per ok->failed transition, not per attempt/request.
func TestInitFailedHook_OncePerTransition(t *testing.T) {
	stub := newExactFetcher(map[string][]byte{warmPageURL: viteShellHTML()})
	mgr := newTestManager(stub)
	clock := time.Now()
	mgr.now = func() time.Time { return clock }
	var alerts atomic.Int32
	mgr.SetInitFailedHook(func(error) { alerts.Add(1) })

	hammer := func() {
		for i := 0; i < 50; i++ {
			_, _ = mgr.GenerateID("GET", "/p")
		}
	}
	want := func(n int32, when string) {
		t.Helper()
		if got := alerts.Load(); got != n {
			t.Fatalf("%s: alert fired %d times, want %d", when, got, n)
		}
	}

	hammer()
	want(1, "50 failing requests")
	clock = clock.Add(defaultBackoffBase + time.Second)
	hammer() // a second failing ATTEMPT, still the same outage
	want(1, "second failing attempt in the same outage")

	// Recover (legacy page is fine), then fail again after the keys go stale.
	stub.set(map[string][]byte{
		warmPageURL: legacyHTML(fixtureKey),
		"https://abs.twimg.com/responsive-web/client-web/ondemand.s.deadbe0a.js": []byte(ondemandJS),
	})
	clock = clock.Add(2 * defaultBackoffBase * 2)
	if _, err := mgr.GenerateID("GET", "/p"); err != nil {
		t.Fatalf("GenerateID after recovery: %v", err)
	}
	want(1, "after recovery")

	stub.set(map[string][]byte{warmPageURL: viteShellHTML()})
	clock = clock.Add(mgr.refreshInterval + time.Minute)
	if _, err := mgr.GenerateID("GET", "/p"); err != nil {
		t.Fatalf("stale keys must still serve while refresh fails: %v", err)
	}
	hammer()
	want(2, "success->failure transition")
}
