package bundle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync"
	"testing"
)

type mapFetcher struct {
	mu     sync.Mutex
	routes map[string][]byte
	posted []url.Values
}

func (m *mapFetcher) Fetch(_ context.Context, u string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.routes[u]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("map fetcher: %s: HTTP 404", u)
}

func (m *mapFetcher) PostForm(_ context.Context, u string, form url.Values) ([]byte, error) {
	m.mu.Lock()
	m.posted = append(m.posted, form)
	m.mu.Unlock()
	return m.Fetch(context.Background(), u+"#posted")
}

func TestViteScripts_FiltersHostsAndDedupes(t *testing.T) {
	page := `<script src="https://abs.twimg.com/x-web/x-web/a-1.js"></script>` +
		`<link href="https://abs.twimg.com/x-web/x-web/a-1.js">` +
		`<script src="https://evil.example/x-web/x-web/b.js"></script>` +
		`<script src="https://abs.twimg.com/responsive-web/client-web/main.js"></script>`
	got := ViteScripts(page, "https://x.com/tesla")
	if len(got) != 1 || got[0] != "https://abs.twimg.com/x-web/x-web/a-1.js" {
		t.Fatalf("ViteScripts = %v, want only the deduped allowlisted x-web link", got)
	}
}

func TestIsLoggedOutShell(t *testing.T) {
	if !IsLoggedOutShell([]string{"https://abs.twimg.com/x-web/x-web/entry-client-logged-out-0123456789abcdef.js"}) {
		t.Fatal("logged-out entry not recognised")
	}
	if IsLoggedOutShell([]string{"https://abs.twimg.com/x-web/x-web/entry-client-0123456789abcdef.js"}) {
		t.Fatal("logged-in entry misread as logged-out")
	}
}

func TestFindIndicesURL_ScansChunksAndResolvesRelative(t *testing.T) {
	const base = "https://abs.twimg.com/x-web/x-web/"
	f := &mapFetcher{routes: map[string][]byte{
		base + "a.js": []byte(`var design = "design.o-0000.js";`), // \b must reject this
		base + "b.js": []byte(`x=()=>import("./sign.o-0123456789abcdef.js")`),
		base + "c.js": []byte(`nothing`),
	}}
	got, err := FindIndicesURL(context.Background(), f, []string{base + "a.js", base + "b.js", base + "c.js", base + "gone.js"}, 2, 10)
	if err != nil {
		t.Fatalf("FindIndicesURL: %v", err)
	}
	if want := base + "sign.o-0123456789abcdef.js"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFindIndicesURL_NotFoundIsTyped(t *testing.T) {
	const base = "https://abs.twimg.com/x-web/x-web/"
	f := &mapFetcher{routes: map[string][]byte{base + "a.js": []byte(`nothing`)}}
	_, err := FindIndicesURL(context.Background(), f, []string{base + "a.js", base + "missing.js"}, 2, 10)
	if !errors.Is(err, ErrIndicesNotFound) {
		t.Fatalf("err = %v, want ErrIndicesNotFound", err)
	}
}

func TestFindIndicesURL_RejectsOffAllowlistReference(t *testing.T) {
	const base = "https://abs.twimg.com/x-web/x-web/"
	f := &mapFetcher{routes: map[string][]byte{
		base + "a.js": []byte(`import("https://evil.example/sign.o-1.js")`),
	}}
	if _, err := FindIndicesURL(context.Background(), f, []string{base + "a.js"}, 1, 10); !errors.Is(err, ErrIndicesNotFound) {
		t.Fatalf("err = %v, want ErrIndicesNotFound for an off-allowlist reference", err)
	}
}

func TestFetchPage_FollowsLocationThenMigrateForm(t *testing.T) {
	f := &mapFetcher{routes: map[string][]byte{
		"https://x.com/tesla":            []byte(`<html><script>document.location = "https:\/\/x.com\/tesla?mx=2"</script></html>`),
		"https://x.com/tesla?mx=2":       []byte(`<form action="https://x.com/x/migrate" method="post"><input type="hidden" name="tok" value="a&amp;b"><input name="empty"></form>`),
		"https://x.com/x/migrate#posted": []byte(`REAL PAGE`),
	}}
	got, err := FetchPage(context.Background(), f, "https://x.com/tesla")
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if string(got) != "REAL PAGE" {
		t.Fatalf("body = %q", got)
	}
	if len(f.posted) != 1 || f.posted[0].Get("tok") != "a&b" || !f.posted[0].Has("empty") {
		t.Fatalf("posted form = %v", f.posted)
	}
}

func TestFetchPage_RefusesOffAllowlistRedirect(t *testing.T) {
	f := &mapFetcher{routes: map[string][]byte{
		"https://x.com/tesla": []byte(`<script>document.location = "https://evil.example/x"</script>`),
	}}
	if _, err := FetchPage(context.Background(), f, "https://x.com/tesla"); err == nil {
		t.Fatal("expected refusal of an off-allowlist document.location target")
	}
}

func TestFetchPage_PlainPagePassesThrough(t *testing.T) {
	f := &mapFetcher{routes: map[string][]byte{"https://x.com/": []byte(`<html>document.location = "https://evil.example"</html>`)}}
	got, err := FetchPage(context.Background(), f, "https://x.com/")
	if err != nil || string(got) != string(f.routes["https://x.com/"]) {
		t.Fatalf("plain page altered: %q, %v", got, err)
	}
}

// --- cookie scoping on the wire ---

type recordingDoer struct {
	mu      sync.Mutex
	headers map[string]map[string]string // url -> headers
}

func (r *recordingDoer) DoCtx(_ context.Context, _, u string, h map[string]string, _ io.Reader) ([]byte, map[string]string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.headers == nil {
		r.headers = map[string]map[string]string{}
	}
	r.headers[u] = h
	return []byte("ok"), nil, 200, nil
}

// The session cookie goes to x.com only; the abs.twimg.com CDN never sees it.
func TestStealthFetcher_CookieScopedToXHosts(t *testing.T) {
	d := &recordingDoer{}
	f := &StealthFetcher{Client: d, Cookie: "auth_token=secret"}
	for _, u := range []string{"https://x.com/tesla", "https://abs.twimg.com/x-web/x-web/a.js"} {
		if _, err := f.Fetch(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	if got := d.headers["https://x.com/tesla"]["cookie"]; got != "auth_token=secret" {
		t.Fatalf("x.com request cookie = %q, want the session cookie", got)
	}
	if got, ok := d.headers["https://abs.twimg.com/x-web/x-web/a.js"]["cookie"]; ok {
		t.Fatalf("CDN request leaked the session cookie: %q", got)
	}
}

func TestCookieHostOK(t *testing.T) {
	for host, want := range map[string]bool{
		"x.com": true, "www.x.com": true, "twitter.com": true, "abs.twimg.com": false,
		"evilx.com": false, "x.com.evil.example": false,
	} {
		if got := cookieHostOK(host); got != want {
			t.Errorf("cookieHostOK(%q) = %v want %v", host, got, want)
		}
	}
}

func TestParseChunkMap_Accepts16HexHashes(t *testing.T) {
	names, hashes := parseChunkMap(`{1:"main",20113:"ondemand.s"} {20113:"0123456789abcdef",1:"abc1234"}`)
	if hashes["20113"] != "0123456789abcdef" {
		t.Fatalf("16-hex hash dropped: %v", hashes)
	}
	if _, polluted := names["20113"]; polluted && names["20113"] != "ondemand.s" {
		t.Fatalf("hash leaked into names: %v", names)
	}
}
