package xtid

import (
	"context"
	"testing"
)

// runtimeScript is the inline webpack runtime shape the reference parses: a
// chunk-name map holding "ondemand.s" and a hash map keyed by the same id.
const runtimeScript = `<script>n.u=e=>(({1:"main",20113:"ondemand.s",30:"x"})[e]||e)+"."+({1:"aa11",20113:"cafe123",30:"bb22"})[e]+"a.js"</script>`

func TestOnDemandURLFromRuntime(t *testing.T) {
	cases := []struct {
		name, html, want string
	}{
		{"reference shape", runtimeScript, "https://abs.twimg.com/responsive-web/client-web/ondemand.s.cafe123a.js"},
		{
			// A different id holds an identical-looking hash; the match must follow
			// the ondemand.s id (the reference's backreference), not the first hash.
			"hash picked by chunk id",
			`<script>x=(({7:"other",99:"ondemand.s"})[e]||e)+"."+({7:"WRONG",99:"right_1-x"})[e]</script>`,
			"https://abs.twimg.com/responsive-web/client-web/ondemand.s.right_1-xa.js",
		},
		{"no ondemand.s", `<script>x=1</script>`, ""},
		{"name without hash map", `<script>y={5:"ondemand.s"}</script>`, ""},
	}
	for _, tc := range cases {
		if got := onDemandURLFromRuntime(tc.html); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func loggedOutPage(key []byte) []byte {
	return []byte(`<html><head>` + metaTag(key) + runtimeScript + `</head><body>` + svgBlock + `</body></html>`)
}

// With the logged-out key source, keys come from /i/jf/ over the cookie-less
// fetcher only; the account's fetcher must never be used, and the indices file
// is resolved from the page's inline runtime.
func TestKeySourceLoggedOut_CookielessJFPage(t *testing.T) {
	base := newExactFetcher(map[string][]byte{
		LoggedOutPageURL: loggedOutPage(fixtureKey),
		"https://abs.twimg.com/responsive-web/client-web/ondemand.s.cafe123a.js": []byte(ondemandJS),
	})
	auth := newExactFetcher(viteRoutes(fixtureKey)) // would succeed if (wrongly) used
	mgr := newTestManager(base)
	mgr.SetKeySource(KeySourceLoggedOut)

	id, err := mgr.GenerateIDWith(context.Background(), "GET", "/p", auth)
	if err != nil || id == "" {
		t.Fatalf("GenerateIDWith: %q, %v", id, err)
	}
	if got := base.count(LoggedOutPageURL); got != 1 {
		t.Fatalf("logged-out page fetched %d times, want 1", got)
	}
	if n := len(auth.counts); n != 0 {
		t.Fatalf("account fetcher used for key derivation: %v", auth.counts)
	}
	if base.count(warmPageURL) != 0 {
		t.Fatal("default landing page fetched under the logged-out key source")
	}
}

func TestKeySourceEnv(t *testing.T) {
	t.Setenv(KeySourceEnv, "logged-out")
	if NewManager(nil).keySource != KeySourceLoggedOut {
		t.Fatal("env did not select the logged-out source")
	}
	t.Setenv(KeySourceEnv, "")
	if NewManager(nil).keySource != KeySourceAccountPage {
		t.Fatal("default must stay the account page")
	}
}

// The inline-runtime lookup is part of the logged-out source only: on every
// other page the default order (legacy, Vite scan, chunk map) applies, so a
// page that merely carries a runtime script must not be resolved through it.
//
// Mutation: drop the `pageURL == LoggedOutPageURL` condition in locateIndices
// (xtid/manager.go) => RED.
func TestLocateIndices_RuntimeLookupOnlyForLoggedOutSource(t *testing.T) {
	m := newTestManager(newExactFetcher(nil)) // any fetch fails
	m.cacheDir = t.TempDir()                  // no cached chunk map from other runs
	page := string(loggedOutPage(fixtureKey))
	const want = "https://abs.twimg.com/responsive-web/client-web/ondemand.s.cafe123a.js"

	if got, err := m.locateIndices(context.Background(), newExactFetcher(nil), page, LoggedOutPageURL); err != nil || got != want {
		t.Fatalf("logged-out page: %q, %v; want %q", got, err, want)
	}
	for _, u := range []string{warmPageURL, authedWarmPageURL} {
		got, err := m.locateIndices(context.Background(), newExactFetcher(nil), page, u)
		if got == want || err == nil {
			t.Fatalf("%s consulted the runtime lookup on the default path: %q, %v", u, got, err)
		}
	}
}
