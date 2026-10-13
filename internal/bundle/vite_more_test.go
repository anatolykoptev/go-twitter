package bundle

import (
	"context"
	"errors"
	"io"
	"net/url"
	"testing"
)

func TestViteScripts_RootRelative(t *testing.T) {
	page := `<script src="/x-web/root-1.js"></script>`
	got := ViteScripts(page, "https://x.com/tesla")
	if len(got) != 1 || got[0] != "https://x.com/x-web/root-1.js" {
		t.Fatalf("ViteScripts = %v, want the root-relative script resolved against the page", got)
	}
}

func TestViteScripts_ProtocolRelative(t *testing.T) {
	page := `<link rel="modulepreload" href="//abs.twimg.com/x-web/x-web/proto-2.js">` +
		`<script src="//evil.example/x-web/x-web/bad.js"></script>` +
		`<script src="http://abs.twimg.com/x-web/x-web/downgrade.js"></script>`
	got := ViteScripts(page, "https://x.com/tesla")
	if len(got) != 1 || got[0] != "https://abs.twimg.com/x-web/x-web/proto-2.js" {
		t.Fatalf("ViteScripts = %v, want only the allowlisted protocol-relative script", got)
	}
}

// The indices file is referenced only by a chunk that a LISTED chunk imports.
func TestFindIndicesURL_OneLevelOfDynamicImports(t *testing.T) {
	const base = "https://abs.twimg.com/x-web/x-web/"
	f := &mapFetcher{routes: map[string][]byte{
		base + "a.js":       []byte(`import("./child-1.js")`),
		base + "child-1.js": []byte(`import("./sign.o-0123456789abcdef.js")`),
	}}
	got, err := FindIndicesURL(context.Background(), f, []string{base + "a.js"}, 2, 10)
	if err != nil || got != base+"sign.o-0123456789abcdef.js" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFindIndicesURL_NoThirdLevel(t *testing.T) {
	const base = "https://abs.twimg.com/x-web/x-web/"
	f := &mapFetcher{routes: map[string][]byte{
		base + "a.js":       []byte(`import("./child-1.js")`),
		base + "child-1.js": []byte(`import("./child-2.js")`),
		base + "child-2.js": []byte(`import("./sign.o-0123456789abcdef.js")`),
	}}
	if _, err := FindIndicesURL(context.Background(), f, []string{base + "a.js"}, 2, 10); !errors.Is(err, ErrIndicesNotFound) {
		t.Fatalf("err = %v, want ErrIndicesNotFound beyond one level", err)
	}
}

func TestFindIndicesURL_ChildrenRespectScanCap(t *testing.T) {
	const base = "https://abs.twimg.com/x-web/x-web/"
	f := &mapFetcher{routes: map[string][]byte{
		base + "a.js":       []byte(`import("./child-1.js")`),
		base + "child-1.js": []byte(`import("./sign.o-0123456789abcdef.js")`),
	}}
	if _, err := FindIndicesURL(context.Background(), f, []string{base + "a.js"}, 2, 1); !errors.Is(err, ErrIndicesNotFound) {
		t.Fatalf("err = %v, want ErrIndicesNotFound when the cap leaves no room for children", err)
	}
}

func TestCookieURLOK_HTTPSOnly(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://x.com/a": true, "http://x.com/a": false, "https://abs.twimg.com/a": false,
	} {
		u, _ := url.Parse(raw)
		if got := cookieURLOK(u); got != want {
			t.Errorf("cookieURLOK(%q) = %v want %v", raw, got, want)
		}
	}
}

type bigDoer struct{}

func (bigDoer) DoCtx(context.Context, string, string, map[string]string, io.Reader) ([]byte, map[string]string, int, error) {
	return make([]byte, maxBundleBytes+1), nil, 200, nil
}

func TestStealthFetcher_CapsBody(t *testing.T) {
	f := &StealthFetcher{Client: bigDoer{}}
	if _, err := f.Fetch(context.Background(), "https://x.com/"); err == nil {
		t.Fatal("oversized body accepted")
	}
}
