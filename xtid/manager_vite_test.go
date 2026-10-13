package xtid

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
)

const (
	viteIndices = viteBase + "sign.o-0123456789abcdef.js"
	viteChunkA  = viteBase + "vendor-fedcba9876543210.js"
	viteChunkB  = viteBase + "shell-1111222233334444.js"
)

// viteAuthedHTML is a RECONSTRUCTED authenticated page (built from twscrape's
// description of the current x-web build, not a capture): verification meta, the
// loading-x-anim SVG, and absolute x-web script links incl. modulepreloads.
func viteAuthedHTML(key []byte) []byte {
	return []byte(`<html><head>` + metaTag(key) +
		`<script type="module" crossorigin src="` + viteBase + `entry-client-0000111122223333.js"></script>` +
		`<link rel="modulepreload" href="` + viteChunkA + `">` +
		`<link rel="modulepreload" href="` + viteChunkB + `">` +
		`</head><body>` + svgBlock + `</body></html>`)
}

// viteRoutes: authenticated page + chunks; only shell-*.js dynamically imports
// the indices file, so the scan must really read chunks.
func viteRoutes(key []byte) map[string][]byte {
	return map[string][]byte{
		authedWarmPageURL: viteAuthedHTML(key),
		viteChunkA:        []byte(`export const a=1;`),
		viteChunkB:        []byte(`const s=()=>import("./sign.o-0123456789abcdef.js");export{s};`),
		viteIndices:       []byte(ondemandJS),
	}
}

// F3: authenticated client on the (reconstructed) Vite build: Initialize via the
// auth fetcher succeeds, the id is non-empty, and the unauthenticated shell was
// never needed.
func TestGenerateIDWith_ViteBuildAuthenticated(t *testing.T) {
	base := newExactFetcher(map[string][]byte{warmPageURL: viteShellHTML()})
	auth := newExactFetcher(viteRoutes(fixtureKey))
	mgr := newTestManager(base)

	id, err := mgr.GenerateIDWith(context.Background(), "GET", "/i/api/graphql/x/Op", auth)
	if err != nil {
		t.Fatalf("GenerateIDWith: %v", err)
	}
	if id == "" {
		t.Fatal("empty transaction id")
	}
	if got := base.count(warmPageURL); got != 0 {
		t.Fatalf("unauthenticated fallback used %d times although auth succeeded", got)
	}
	if auth.count(viteIndices) != 1 {
		t.Fatalf("indices file fetched %d times, want 1", auth.count(viteIndices))
	}
}

// Logged-out shell on the auth path falls back, then reports a typed cause.
func TestInitialize_LoggedOutShellIsTyped(t *testing.T) {
	mgr := newTestManager(newExactFetcher(map[string][]byte{warmPageURL: viteShellHTML()}))
	err := mgr.Initialize()
	if !errors.Is(err, bundle.ErrLoggedOutShell) {
		t.Fatalf("Initialize error = %v, want ErrLoggedOutShell", err)
	}
}

// F3b: the 16-hex webpack chunk-map hash (post 2026-08-24) resolves.
func TestInitialize_ChunkMap16HexHash(t *testing.T) {
	html := []byte(`<html><head>` + metaTag(fixtureKey) +
		`<script>var n={1:"main",20113:"ondemand.s"};var h={20113:"0123456789abcdef"};</script>` +
		`</head><body>` + svgBlock + `</body></html>`)
	stub := newExactFetcher(map[string][]byte{
		warmPageURL:                 html,
		"https://x.com/home":        html,
		"https://x.com/xdevelopers": html,
		"https://abs.twimg.com/responsive-web/client-web/ondemand.s.0123456789abcdefa.js": []byte(ondemandJS),
	})
	mgr := newTestManager(stub)
	mgr.cacheDir = t.TempDir()
	if err := mgr.Initialize(); err != nil {
		t.Fatalf("Initialize with 16-hex chunk-map hash: %v", err)
	}
}

// F4: legacy webpack build still initialises, here against the REAL captured
// ondemand.s (testdata) + real warm page.
func TestInitialize_LegacyRealFixtures(t *testing.T) {
	home, err := os.ReadFile(filepath.Join("testdata", "home_real.html"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile(filepath.Join("testdata", "ondemand_real.js"))
	if err != nil {
		t.Fatal(err)
	}
	page := append(home, []byte(`<script>e={"ondemand.s":"91612d9"}</script>`)...)
	stub := newExactFetcher(map[string][]byte{
		warmPageURL: page,
		"https://abs.twimg.com/responsive-web/client-web/ondemand.s.91612d9a.js": js,
	})
	mgr := newTestManager(stub)
	mgr.cacheDir = t.TempDir()
	if err := mgr.Initialize(); err != nil {
		t.Fatalf("Initialize (legacy real fixtures): %v", err)
	}
	if id, err := mgr.GenerateID("GET", "/p"); err != nil || id == "" {
		t.Fatalf("GenerateID: %q, %v", id, err)
	}
}

// Migrate redirect: stub page -> document.location -> real page.
func TestInitialize_FollowsDocumentLocation(t *testing.T) {
	routes := viteRoutes(fixtureKey)
	stubPage := []byte(`<html><script>document.location = "https://x.com/tesla?mx=2"</script></html>`)
	routes[authedWarmPageURL] = stubPage
	routes["https://x.com/tesla?mx=2"] = viteAuthedHTML(fixtureKey)
	auth := newExactFetcher(routes)
	mgr := newTestManager(newExactFetcher(nil))
	if _, err := mgr.GenerateIDWith(context.Background(), "GET", "/p", auth); err != nil {
		t.Fatalf("GenerateIDWith through document.location hop: %v", err)
	}
}
