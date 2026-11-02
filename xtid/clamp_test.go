package xtid

import (
	"encoding/base64"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// X rejects an x-client-transaction-id whose animation key prints a colour
// channel above 255 as three hex digits ("112" instead of "ff"): the logged-out
// live probe of 2026-11-02 got 404 on 4 of 4 key loads with such a channel and
// 200 on 46 of 46 without. The reference implementation shares the defect, so
// for these keys live X, not the reference, is the oracle. The fixtures here
// are the public reduced live page and its indices script in testdata, with
// the verification key replaced by one chosen to overflow a channel.

const (
	clampPath = "/i/api/graphql/abc123/UserTweetsAndReplies"
	// clampKeyB64 is a 48-byte verification key for live_page.html whose
	// animation drives a colour channel above 255 (see TestClamp_FixtureOverflows).
	clampKeyB64 = "82gCFB4o3UqR2UZV7LutHF1un2u45+FysCFgrRgyrep9mS9XJH14V2PvG0loA+Dr"
	// Expected values for clampKeyB64, computed by the reference's animate with
	// the 255 clamp applied (gen/gen.mjs variantAnimate{eps:1e-5, clamp:true}).
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

// unclamped is the reference's animate: the same computation as ct.animate
// with the channels floored at 0 but not capped at 255. It returns the printed
// channels, the rest of the key (matrix fields and padding) and the raw channel
// values after rounding.
func unclamped(ct *ClientTransaction, frames []int, tt float64) (chans [3]string, rest string, vals [3]int, negative bool) {
	fromColor := []float64{float64(frames[0]), float64(frames[1]), float64(frames[2]), 1}
	toColor := []float64{float64(frames[3]), float64(frames[4]), float64(frames[5]), 1}
	toRotation := []float64{ct.solve(float64(frames[6]), 60.0, 360.0, true)}
	curves := make([]float64, len(frames)-7)
	for i, item := range frames[7:] {
		curves[i] = ct.solve(float64(item), isOdd(i), 1.0, false)
	}
	val := newCubic(curves).getValue(tt)
	color := interpolate(fromColor, toColor, val)
	for i := 0; i < 3; i++ {
		negative = negative || color[i] < 0
		vals[i] = int(math.Round(math.Max(0, color[i])))
		chans[i] = fmt.Sprintf("%x", vals[i])
	}
	var b strings.Builder
	for _, v := range convertRotationToMatrix(interpolate([]float64{0}, toRotation, val)[0]) {
		r := math.Abs(round2(v))
		h := floatToHex(r)
		switch {
		case strings.HasPrefix(h, "."):
			b.WriteString("0" + strings.ToLower(h))
		case h == "":
			b.WriteString("0")
		default:
			b.WriteString(h)
		}
	}
	b.WriteString("00")
	return chans, regexp.MustCompile(`[.-]`).ReplaceAllString(b.String(), ""), vals, negative
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

func TestClamp_FixtureOverflows(t *testing.T) {
	page, js := clampFixture(t, clampKeyB64)
	ct, err := newClientTransaction(page, js)
	if err != nil {
		t.Fatal(err)
	}
	row, tt := rowAndTime(ct)
	chans, rest, vals, _ := unclamped(ct, ct.get2DArray(page)[row], tt)
	if vals[0] <= 255 {
		t.Fatalf("fixture key no longer overflows a channel: %v", vals)
	}
	if got := strings.Join(chans[:], "") + rest; got != clampUnclampedKey {
		t.Fatalf("unclamped key = %q, want %q", got, clampUnclampedKey)
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

// For random verification keys the animation key differs from the reference's
// only where a channel overflows, and there only in that channel, which is
// capped at ff. Everything else, including every key without an overflow, is
// the unclamped computation.
func TestClamp_PropertyRandomKeys(t *testing.T) {
	_, js := clampFixture(t, clampKeyB64)
	r := rand.New(rand.NewPCG(1, 2))
	var over, neg, plain int
	for n := 0; n < 8000; n++ {
		kb := make([]byte, 48)
		for i := range kb {
			kb[i] = byte(r.IntN(256))
		}
		page, _ := clampFixture(t, base64.StdEncoding.EncodeToString(kb))
		ct, err := newClientTransaction(page, js)
		if err != nil {
			t.Fatal(err)
		}
		row, tt := rowAndTime(ct)
		chans, rest, vals, negative := unclamped(ct, ct.get2DArray(page)[row], tt)
		want := chans
		overflow := false
		for i, v := range vals {
			if v > 255 {
				want[i], overflow = "ff", true
			}
		}
		if got := strings.Join(want[:], "") + rest; ct.animationKey != got {
			t.Fatalf("key %d: animation key = %q, want %q (unclamped channels %v)", n, ct.animationKey, got, vals)
		}
		for i, v := range vals {
			if hv, err := strconv.ParseUint(want[i], 16, 8); err != nil || len(want[i]) > 2 {
				t.Fatalf("channel %d = %q (raw %d) exceeds ff: %v %d", i, want[i], v, err, hv)
			}
		}
		switch {
		case overflow:
			over++
		case negative:
			neg++
		default:
			plain++
			if ct.animationKey != strings.Join(chans[:], "")+rest {
				t.Fatalf("key %d changed without an overflow", n)
			}
		}
	}
	if over < 100 || neg < 100 || plain < 4000 {
		t.Fatalf("sample lost a class: overflow=%d negative=%d plain=%d", over, neg, plain)
	}
}
