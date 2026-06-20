package xtid

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// stubFetcher is a counting, substring-routed bundle.Fetcher for xtid tests.
type stubFetcher struct {
	mu     sync.Mutex
	calls  int
	routes map[string][]byte
}

func (s *stubFetcher) Fetch(_ context.Context, u string) ([]byte, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	for sub, body := range s.routes {
		if strings.Contains(u, sub) {
			return body, nil
		}
	}
	return nil, fmt.Errorf("stub: no route for %s", u)
}

// fixtureKey is an 8-byte verification key chosen so the xtid math resolves on
// the hand-authored SVG fixture: byte[0]=0 -> rowIndex 0, byte[5]=0 -> frame 0.
var fixtureKey = []byte{0, 20, 30, 40, 50, 0, 70, 80}

// svgBlock is a loading-x-anim-0 SVG whose cubic path yields one 12-number row
// (>=11), enough for animate() to produce a non-empty animation key.
const svgBlock = `<svg id="loading-x-anim-0"><path d="M0 0C1 2 3 4 5 6 7 8 9 10 11 12" fill="#1d9bf008"></path></svg>`

// ondemandJS carries the key-byte index pattern: rowIndex 0, indices [1].
const ondemandJS = `(function(){return k(c[0],16)*k(c[1],16)})()`

func metaTag(key []byte) string {
	enc := base64.StdEncoding.EncodeToString(key)
	return fmt.Sprintf(`<meta name="twitter-site-verification" content="%s">`, enc)
}

// chunkMapHTML is a modern snapshot: verification meta + SVG + webpack chunk map
// (no legacy embed), so Initialize resolves ondemand.s through the bundle core.
func chunkMapHTML(key []byte) []byte {
	return []byte(`<html><head>` + metaTag(key) +
		`<script>var n={1:"main",20113:"ondemand.s"};var h={20113:"abc1234"};</script>` +
		`</head><body>` + svgBlock + `</body></html>`)
}

// legacyHTML is a legacy snapshot: the direct "ondemand.s":"<hash>" embed and NO
// chunk map, so only the fast-path can resolve it.
func legacyHTML(key []byte) []byte {
	return []byte(`<html><head>` + metaTag(key) +
		`<script>e={"ondemand.s":"deadbe0"}</script>` +
		`</head><body>` + svgBlock + `</body></html>`)
}

func TestInitialize_ResolvesViaChunkMap(t *testing.T) {
	stub := &stubFetcher{routes: map[string][]byte{
		"x.com":         chunkMapHTML(fixtureKey),
		"abs.twimg.com": []byte(ondemandJS),
	}}
	mgr := NewManager(stub)
	mgr.cacheDir = t.TempDir()

	if err := mgr.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if mgr.ct == nil {
		t.Fatal("ct is nil after Initialize")
	}
	if mgr.ct.animationKey == "" {
		t.Fatal("animationKey is empty")
	}

	// animationKey is a pure function of HTML+JS — a fresh manager over the same
	// fixtures must reproduce it byte-for-byte.
	want := mgr.ct.animationKey
	mgr2 := NewManager(stub)
	mgr2.cacheDir = t.TempDir()
	if err := mgr2.Initialize(); err != nil {
		t.Fatalf("Initialize (2nd): %v", err)
	}
	if mgr2.ct.animationKey != want {
		t.Fatalf("animationKey not stable: %q vs %q", mgr2.ct.animationKey, want)
	}
}

// TestInitialize_LegacyFastPath proves a legacy-only snapshot still resolves. The
// stub has NO chunk-map route (only the legacy HTML + the legacy ondemand URL),
// so bundle.Build would fail — Initialize succeeding proves the fast-path ran.
func TestInitialize_LegacyFastPath(t *testing.T) {
	stub := &stubFetcher{routes: map[string][]byte{
		"x.com":                  legacyHTML(fixtureKey),
		"ondemand.s.deadbe0a.js": []byte(ondemandJS),
	}}
	mgr := NewManager(stub)
	mgr.cacheDir = t.TempDir()

	if err := mgr.Initialize(); err != nil {
		t.Fatalf("Initialize (legacy): %v", err)
	}
	if mgr.ct == nil || mgr.ct.animationKey == "" {
		t.Fatal("legacy fast-path did not build a usable ct")
	}
}

func TestInitialize_FetchFailureReturnsError(t *testing.T) {
	stub := &stubFetcher{routes: map[string][]byte{}} // no route -> warm-page fetch fails
	mgr := NewManager(stub)
	mgr.cacheDir = t.TempDir()

	if err := mgr.Initialize(); err == nil {
		t.Fatal("expected Initialize to error when the warm page is unreachable")
	}
}
