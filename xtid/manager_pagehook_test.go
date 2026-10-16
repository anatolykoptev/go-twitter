package xtid

import (
	"context"
	"testing"
)

// The warm-page hook receives the authenticated page once per init, and never
// the unauthenticated shell (that page carries nothing a consumer could reuse).
func TestSetWarmPageHook_AuthedPageOnly(t *testing.T) {
	base := newExactFetcher(map[string][]byte{warmPageURL: viteShellHTML()})
	auth := newExactFetcher(viteRoutes(fixtureKey))
	mgr := newTestManager(base)

	var urls []string
	var bodies [][]byte
	mgr.SetWarmPageHook(func(pageURL string, page []byte) {
		urls = append(urls, pageURL)
		bodies = append(bodies, page)
	})

	if _, err := mgr.GenerateIDWith(context.Background(), "GET", "/i/api/graphql/x/Op", auth); err != nil {
		t.Fatalf("GenerateIDWith: %v", err)
	}
	if len(urls) != 1 || urls[0] != AuthedWarmPageURL {
		t.Fatalf("hook calls = %v, want exactly one for %s", urls, AuthedWarmPageURL)
	}
	if string(bodies[0]) != string(viteAuthedHTML(fixtureKey)) {
		t.Fatal("hook did not receive the authenticated page body")
	}

	// A second id from the warm manager must not re-fetch (and so not re-fire).
	if _, err := mgr.GenerateIDWith(context.Background(), "GET", "/i/api/graphql/x/Op", auth); err != nil {
		t.Fatalf("second GenerateIDWith: %v", err)
	}
	if len(urls) != 1 {
		t.Fatalf("hook fired %d times, want 1 (no re-init)", len(urls))
	}
}
