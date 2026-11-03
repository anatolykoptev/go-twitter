package xtid

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// X rejects an x-client-transaction-id whose animation key prints a colour
// channel above 255 as three hex digits ("125" instead of "ff"): the live probe
// of 2026-11-02 got 404 on 4 of 4 key loads with such a channel and 200 on 46 of
// 46 without, and a real browser (testdata/browser) agrees with the capped key
// on all of them. The fixtures here are the public reduced live page and its
// indices script in testdata, with the verification key replaced by one chosen
// to overflow a channel; TestBrowserGolden holds the same key.

const (
	clampPath = "/i/api/graphql/abc123/UserTweetsAndReplies"
	// clampKeyB64 is a 48-byte verification key for live_page.html whose
	// animation drives a colour channel above 255 (see TestClamp_FixtureOverflows).
	clampKeyB64 = "82gCFB4o3UqR2UZV7LutHF1un2u45+FysCFgrRgyrep9mS9XJH14V2PvG0loA+Dr"
	// Expected key for clampKeyB64, as the browser computes it (it is also a
	// vector in testdata/browser/vectors.json, checked by TestBrowserGolden).
	clampWantKey = "ffe600a8f5c28f5c28f80c28f5c28f5c290c28f5c28f5c290a8f5c28f5c28f800"
	// clampWantID is the id for clampWantKey at t=123456789 s, random byte 7.
	clampWantID = "B/RvBRMZL9pNlt5BUuu8qhtaaZhsv+DmdbcmZ6ofNartep4oUCN6f1Bk6BxObwTn7BLKXABAkLYikBWUCc0Pn3fzRIpGBA"
	// clampUnclampedKey is the same key without the cap: channel 0 is 293, "125".
	clampUnclampedKey = "125e600a8f5c28f5c28f80c28f5c28f5c290c28f5c28f5c290a8f5c28f5c28f800"
)

var verificationMetaRe = regexp.MustCompile(`(<meta[^>]*name=["']twitter-site-verification["'][^>]*content=["'])[^"']+`)

func clampFixture(t testing.TB, keyB64 string) (page, js string) {
	t.Helper()
	dir := filepath.Join("testdata", "reference")
	p, err := os.ReadFile(filepath.Join(dir, "live_page.html"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := os.ReadFile(filepath.Join(dir, "live_ondemand.js"))
	if err != nil {
		t.Fatal(err)
	}
	page = verificationMetaRe.ReplaceAllString(string(p), "${1}"+keyB64)
	if page == string(p) && !strings.Contains(string(p), keyB64) {
		t.Fatal("verification meta not replaced")
	}
	return page, string(j)
}

// rowAndTime repeats buildAnimationKey's choice of row and time from the key
// bytes, so the oracle computation runs on the same inputs.
func rowAndTime(ct *ClientTransaction) (row int, tt float64) {
	row = int(ct.keyBytes[ct.rowIndex]) % 16
	ft := 1.0
	for _, i := range ct.keyBytesIndices {
		ft *= float64(int(ct.keyBytes[i]) % 16)
	}
	return row, jsRound(ft/10) * 10 / 4096
}

// The fixture key must keep overflowing a channel, or the tests below stop
// covering the clamp.
func TestClamp_FixtureOverflows(t *testing.T) {
	page, js := clampFixture(t, clampKeyB64)
	ct, err := newClientTransaction(page, js)
	if err != nil {
		t.Fatal(err)
	}
	if c := classify(ct, page); !c.overflow {
		t.Fatalf("fixture key no longer overflows a colour channel: %+v", c)
	}
	if ct.animationKey == clampUnclampedKey {
		t.Fatalf("animation key %q is the unclamped one", ct.animationKey)
	}
}

// Through the production constructor: the animation key and the id built on it.
func TestClamp_AnimationKeyAndID(t *testing.T) {
	page, js := clampFixture(t, clampKeyB64)
	ct, err := newClientTransaction(page, js)
	if err != nil {
		t.Fatal(err)
	}
	if ct.animationKey != clampWantKey {
		t.Fatalf("animation key = %q, want %q", ct.animationKey, clampWantKey)
	}
	ct.now = func() time.Time { return time.UnixMilli(1682924400000 + 123456789*1000) }
	ct.randByte = func() byte { return 7 }
	got := ct.GenerateID("GET", clampPath)
	if got != clampWantID {
		t.Fatalf("id = %q, want %q", got, clampWantID)
	}
	ct.animationKey = clampUnclampedKey
	if bad := ct.GenerateID("GET", clampPath); bad == got {
		t.Fatal("id equals the one built on the unclamped key")
	}
}

// Through the manager, as the client uses it: the key a Manager serves.
func TestClamp_Manager(t *testing.T) {
	page, js := clampFixture(t, clampKeyB64)
	m := NewManager(&goldenFetcher{page: []byte(page), js: []byte(js)})
	m.SetKeySource(KeySourceLoggedOut)
	if err := m.Initialize(); err != nil {
		t.Fatal(err)
	}
	if m.ct.animationKey != clampWantKey {
		t.Fatalf("manager animation key = %q, want %q", m.ct.animationKey, clampWantKey)
	}
}
