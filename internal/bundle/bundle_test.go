package bundle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubFetcher is a counting in-memory Fetcher used across the suite.
//   - routes: returned when the requested URL contains the key substring.
//   - fixed: returned for every URL (mimics a login-wall body).
//   - failErr: returned for every URL (mimics a hard network failure).
type stubFetcher struct {
	mu      sync.Mutex
	calls   int
	routes  map[string][]byte
	fixed   []byte
	failErr error
}

func (s *stubFetcher) Fetch(_ context.Context, u string) ([]byte, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()

	if s.failErr != nil {
		return nil, s.failErr
	}
	if s.fixed != nil {
		return s.fixed, nil
	}
	for sub, body := range s.routes {
		if strings.Contains(u, sub) {
			return body, nil
		}
	}
	return nil, fmt.Errorf("stub: no route for %s", u)
}

func (s *stubFetcher) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

const homeURL = "https://x.com/home"

func TestBuild_ParsesChunkMap(t *testing.T) {
	stub := &stubFetcher{routes: map[string][]byte{"/home": loadFixture(t, "home.html")}}
	opts := Options{WarmPages: []string{homeURL}, CacheDir: t.TempDir()}

	m, err := Build(context.Background(), stub, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(m.Names) == 0 || len(m.Hashes) == 0 {
		t.Fatalf("empty map: names=%d hashes=%d", len(m.Names), len(m.Hashes))
	}

	id, ok := m.ChunkIDByName("ondemand.s")
	if !ok || id != "20113" {
		t.Fatalf("ChunkIDByName(ondemand.s) = %q,%v want 20113,true", id, ok)
	}

	gotURL, ok := m.BundleURL("ondemand.s")
	wantURL := "https://abs.twimg.com/responsive-web/client-web/ondemand.s.117abc8a.js"
	if !ok || gotURL != wantURL {
		t.Fatalf("BundleURL = %q,%v want %q,true", gotURL, ok, wantURL)
	}
}

func TestBuild_UsesCacheWithinTTL(t *testing.T) {
	stub := &stubFetcher{routes: map[string][]byte{"/home": loadFixture(t, "home.html")}}
	opts := Options{WarmPages: []string{homeURL}, CacheDir: t.TempDir()}

	if _, err := Build(context.Background(), stub, opts); err != nil {
		t.Fatalf("cold Build: %v", err)
	}
	if stub.count() == 0 {
		t.Fatal("expected at least one fetch on the cold build")
	}

	before := stub.count()
	if _, err := Build(context.Background(), stub, opts); err != nil {
		t.Fatalf("warm Build: %v", err)
	}
	if got := stub.count() - before; got != 0 {
		t.Fatalf("expected 0 fetches within TTL, got %d", got)
	}
}

func TestBuild_LoginWallReturnsError(t *testing.T) {
	stub := &stubFetcher{fixed: []byte("<html><body>Log in to X</body></html>")}
	opts := Options{WarmPages: []string{homeURL}, CacheDir: t.TempDir()}

	_, err := Build(context.Background(), stub, opts)
	if err == nil {
		t.Fatal("expected an error on login-wall HTML")
	}
	if !errors.Is(err, ErrEmptyChunkMap) {
		t.Fatalf("expected ErrEmptyChunkMap, got %v", err)
	}
}

func TestBuild_ReturnsCachedMapOnFreshCache(t *testing.T) {
	dir := t.TempDir()
	good := &stubFetcher{routes: map[string][]byte{"/home": loadFixture(t, "home.html")}}
	opts := Options{WarmPages: []string{homeURL}, CacheDir: dir}
	if _, err := Build(context.Background(), good, opts); err != nil {
		t.Fatalf("seed Build: %v", err)
	}

	// A broken fetcher would fail every fetch; a fresh cache must short-circuit it.
	broken := &stubFetcher{fixed: []byte("<html>Log in to X</html>")}
	m, err := Build(context.Background(), broken, opts)
	if err != nil {
		t.Fatalf("cached Build: %v", err)
	}
	if _, ok := m.ChunkIDByName("ondemand.s"); !ok {
		t.Fatal("cached map missing ondemand.s")
	}
	if broken.count() != 0 {
		t.Fatalf("expected 0 fetches with a fresh cache, got %d", broken.count())
	}
}

func TestBuild_FallsBackToStaleCacheOnFailure(t *testing.T) {
	dir := t.TempDir()
	stale := &Map{
		Names:     map[string]string{"20113": "ondemand.s"},
		Hashes:    map[string]string{"20113": "117abc8"},
		FetchedAt: time.Now().Add(-48 * time.Hour),
	}
	if err := (&diskCache{dir: dir, ttl: defaultCacheTTL}).store(stale); err != nil {
		t.Fatalf("seed stale cache: %v", err)
	}

	broken := &stubFetcher{failErr: errors.New("boom")}
	opts := Options{WarmPages: []string{homeURL}, CacheDir: dir, CacheTTL: time.Hour}
	m, err := Build(context.Background(), broken, opts)
	if err != nil {
		t.Fatalf("expected stale-cache fallback, got error: %v", err)
	}
	if id, ok := m.ChunkIDByName("ondemand.s"); !ok || id != "20113" {
		t.Fatalf("stale map not returned: %q,%v", id, ok)
	}
}

func TestParseChunkMap_SkipsHashShapedNames(t *testing.T) {
	body := `var n={1:"main",20113:"ondemand.s"};u={1:"deadbe0",20113:"117abc8"}`
	names, hashes := parseChunkMap(body)

	if names["20113"] != "ondemand.s" {
		t.Fatalf("names[20113] = %q want ondemand.s (hash leaked in)", names["20113"])
	}
	if hashes["20113"] != "117abc8" {
		t.Fatalf("hashes[20113] = %q want 117abc8", hashes["20113"])
	}
	if names["1"] != "main" {
		t.Fatalf("names[1] = %q want main", names["1"])
	}
}

func walkStub(t *testing.T) *stubFetcher {
	t.Helper()
	return &stubFetcher{routes: map[string][]byte{
		"main.bundle.js": loadFixture(t, "main.bundle.js"),
		"foo.js":         loadFixture(t, "foo.js"),
	}}
}

func TestWalkImports_FollowsImportsNoLoop(t *testing.T) {
	stub := walkStub(t)
	start := []string{"https://abs.twimg.com/responsive-web/client-web/main.bundle.js"}

	var visited []string
	err := (&Map{}).WalkImports(context.Background(), stub, start,
		func(u string, _ []byte) { visited = append(visited, u) },
		Options{MaxBundles: 16})
	if err != nil {
		t.Fatalf("WalkImports: %v", err)
	}
	// main.bundle.js + foo.js; the foo.js self-import must not loop.
	if len(visited) != 2 {
		t.Fatalf("expected 2 visited (main+foo), got %d: %v", len(visited), visited)
	}
}

func TestWalkImports_RespectsCap(t *testing.T) {
	stub := walkStub(t)
	start := []string{"https://abs.twimg.com/responsive-web/client-web/main.bundle.js"}

	count := 0
	err := (&Map{}).WalkImports(context.Background(), stub, start,
		func(string, []byte) { count++ },
		Options{MaxBundles: 1})
	if err != nil {
		t.Fatalf("WalkImports: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the cap to limit visits to 1, got %d", count)
	}
}

func TestHTTPFetcher_Fetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello-bundle"))
	}))
	defer srv.Close()

	f, err := NewHTTPFetcher("")
	if err != nil {
		t.Fatalf("NewHTTPFetcher: %v", err)
	}
	body, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(body) != "hello-bundle" {
		t.Fatalf("body = %q want hello-bundle", body)
	}
}

func TestHTTPFetcher_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	f, err := NewHTTPFetcher("")
	if err != nil {
		t.Fatalf("NewHTTPFetcher: %v", err)
	}
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error on HTTP 403")
	}
}

func TestHTTPFetcher_ZeroValueUsesDefaults(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	f := &HTTPFetcher{} // nil Client, empty UserAgent → defaults
	body, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q want ok", body)
	}
	if gotUA != defaultUserAgent {
		t.Fatalf("User-Agent = %q want the default", gotUA)
	}
}

func TestNewHTTPFetcher_BadProxy(t *testing.T) {
	if _, err := NewHTTPFetcher("://bad"); err == nil {
		t.Fatal("expected an error on a malformed proxy URL")
	}
}
