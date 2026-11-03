package xtid

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The animation key is what X's own script prints after running a CSS animation
// in the user's browser, so a real browser is its oracle: testdata/browser runs
// X's shipped indices script, unmodified, in headless Chromium on the public
// live page with a chosen verification key and records the string it hashes.
// TestBrowserGolden holds production to those answers exactly, with no
// allowance. Regenerate with testdata/browser/README.md.

const browserDir = "testdata/browser"

type browserVectors struct {
	Provenance struct {
		Browser      string         `json:"browser"`
		Playwright   string         `json:"playwright"`
		GeneratedAt  string         `json:"generatedAt"`
		Platform     string         `json:"platform"`
		Seed         string         `json:"seed"`
		PageSHA256   string         `json:"pageSha256"`
		ScriptSHA256 string         `json:"scriptSha256"`
		RunnerSHA256 string         `json:"runnerSha256"`
		KeysSHA256   string         `json:"keysSha256"`
		Classes      map[string]int `json:"classes"`
	} `json:"provenance"`
	Keys [][2]string `json:"keys"` // [verification key (base64), animation key]
}

// keyClass says which hard cases a verification key exercises.
type keyClass struct {
	overflow bool // a colour channel rounds above 255 before the cap
	negative bool // a colour channel rounds below 0 before the floor
	solver   bool // the key changes if the easing is solved by plain bisection (Lqm1's method) to 1e-5, 1e-6 or 1e-7
}

// classify evaluates the animation for ct's key and reports the hard cases.
func classify(ct *ClientTransaction, page string) keyClass {
	row, tt := rowAndTime(ct)
	frames := ct.get2DArray(page)[row]
	curves := make([]float64, len(frames)-7)
	for i, item := range frames[7:] {
		curves[i] = ct.solve(float64(item), isOdd(i), 1.0, false)
	}
	c := newCubic(curves)
	val := c.getValue(tt)
	from := []float64{float64(frames[0]), float64(frames[1]), float64(frames[2]), 1}
	to := []float64{float64(frames[3]), float64(frames[4]), float64(frames[5]), 1}
	var k keyClass
	for _, v := range interpolate(from, to, val)[:3] {
		k.overflow = k.overflow || math.Round(v) > 255
		k.negative = k.negative || math.Round(v) < 0
	}
	base := ct.render(frames, val)
	// At t = 0 and t >= 1 the reference uses the end gradients, not the solver.
	for _, eps := range []float64{1e-5, 1e-6, 1e-7} {
		if tt > 0 && tt < 1 && ct.render(frames, c.sampleY(bisectX(c, tt, eps))) != base {
			k.solver = true
		}
	}
	return k
}

// bisectX solves x(t) = x by plain bisection to eps, as the Lqm1 reference does
// (with eps = 1e-5). It is the alternative method the solver class is defined
// against, not production code.
func bisectX(c *Cubic, x, eps float64) float64 {
	lo, hi, mid := 0.0, 1.0, 0.0
	for lo < hi {
		mid = (lo + hi) / 2
		xe := c.sampleX(mid)
		if math.Abs(x-xe) < eps {
			return mid
		}
		if xe < x {
			lo = mid
		} else {
			hi = mid
		}
	}
	return mid
}

func fileSHA(t testing.TB, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestBrowserGolden(t *testing.T) {
	var v browserVectors
	raw, err := os.ReadFile(filepath.Join(browserDir, "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	p := v.Provenance
	if !strings.HasPrefix(p.Browser, "Chrom") || p.GeneratedAt == "" || p.Playwright == "" {
		t.Fatalf("vectors carry no provenance: %+v", p)
	}
	ref := filepath.Join("testdata", "reference")
	if got := fileSHA(t, ref, "live_page.html"); got != p.PageSHA256 {
		t.Fatalf("live_page.html changed (sha256 %s) since the vectors were recorded (%s): regenerate them", got, p.PageSHA256)
	}
	if got := fileSHA(t, ref, "live_ondemand.js"); got != p.ScriptSHA256 {
		t.Fatalf("live_ondemand.js changed (sha256 %s) since the vectors were recorded (%s): regenerate them", got, p.ScriptSHA256)
	}
	if got := fileSHA(t, browserDir, "run.mjs"); got != p.RunnerSHA256 {
		t.Fatalf("run.mjs changed (sha256 %s) since the vectors were recorded (%s): regenerate them", got, p.RunnerSHA256)
	}
	if got := fileSHA(t, browserDir, "keys.json"); got != p.KeysSHA256 {
		t.Fatalf("keys.json changed (sha256 %s) since the vectors were recorded (%s): regenerate them", got, p.KeysSHA256)
	}

	// The vectors answer exactly the keys in keys.json, in order.
	var kf struct {
		Keys []string `json:"keys"`
	}
	kraw, err := os.ReadFile(filepath.Join(browserDir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(kraw, &kf); err != nil {
		t.Fatal(err)
	}
	if len(kf.Keys) != len(v.Keys) {
		t.Fatalf("keys.json has %d keys, vectors.json %d", len(kf.Keys), len(v.Keys))
	}
	for i := range kf.Keys {
		if kf.Keys[i] != v.Keys[i][0] {
			t.Fatalf("vector %d answers key %s..., keys.json has %s...", i, v.Keys[i][0][:8], kf.Keys[i][:8])
		}
	}

	var bad []string
	got := map[string]int{}
	pinned := false
	for _, kv := range v.Keys {
		page, js := clampFixture(t, kv[0])
		ct, err := newClientTransaction(page, js)
		if err != nil {
			t.Fatal(err)
		}
		if ct.animationKey != kv[1] {
			bad = append(bad, fmt.Sprintf("key %s... = %q, browser %q", kv[0][:8], ct.animationKey, kv[1]))
		}
		c := classify(ct, page)
		for name, on := range map[string]bool{"overflow": c.overflow, "negative": c.negative, "solver": c.solver} {
			if on {
				got[name]++
			}
		}
		if kv[0] == clampKeyB64 {
			pinned = kv[1] == clampWantKey
		}
	}
	if len(bad) > 0 {
		shown := bad
		if len(shown) > 5 {
			shown = append(shown[:5:5], fmt.Sprintf("... and %d more", len(bad)-5))
		}
		t.Fatalf("%d of %d keys differ from the browser:\n  %s", len(bad), len(v.Keys), strings.Join(shown, "\n  "))
	}
	if !pinned {
		t.Fatalf("the clamp fixture key is not a vector, or the browser's answer is not %q", clampWantKey)
	}
	// The sample must keep its hard cases; the recorded counts are those of the
	// generator and must be what the code classifies today.
	for name, want := range p.Classes {
		if name != "plain" && got[name] != want {
			t.Fatalf("class %s: %d keys now, vectors recorded %d", name, got[name], want)
		}
	}
	for name, min := range map[string]int{"overflow": 200, "negative": 100, "solver": 80} {
		if got[name] < min {
			t.Fatalf("only %d %s keys among %d vectors, want >= %d", got[name], name, len(v.Keys), min)
		}
	}
	if len(v.Keys) < 1000 {
		t.Fatalf("%d vectors, want >= 1000", len(v.Keys))
	}
}

// TestBrowserKeysGen writes testdata/browser/keys.json: seeded random
// verification keys with the hard cases included by construction. It runs only
// when BROWSER_KEYS_OUT names the output file (testdata/browser/README.md).
func TestBrowserKeysGen(t *testing.T) {
	out := os.Getenv("BROWSER_KEYS_OUT")
	if out == "" {
		t.Skip("set BROWSER_KEYS_OUT to regenerate keys.json")
	}
	const (
		seedA, seedB = 20261102, 156
		wantSolver   = 100 // keys the 1e-5 and 1e-6 solvers get wrong
		wantOver     = 250 // channel above 255
		wantNeg      = 150 // channel below 0
		wantPlain    = 499 // none of those
	)
	r := rand.New(rand.NewPCG(seedA, seedB))
	_, js := clampFixture(t, clampKeyB64)
	keys := []string{clampKeyB64}
	classes := map[string]int{}
	count := func(c keyClass) {
		for name, on := range map[string]bool{"overflow": c.overflow, "negative": c.negative, "solver": c.solver} {
			if on {
				classes[name]++
			}
		}
	}
	page0, _ := clampFixture(t, clampKeyB64)
	ct0, err := newClientTransaction(page0, js)
	if err != nil {
		t.Fatal(err)
	}
	count(classify(ct0, page0))
	var solver, over, neg, plain int
	for n := 0; n < 5_000_000 && (solver < wantSolver || over < wantOver || neg < wantNeg || plain < wantPlain); n++ {
		kb := make([]byte, 48)
		for i := range kb {
			kb[i] = byte(r.IntN(256))
		}
		b64 := base64.StdEncoding.EncodeToString(kb)
		page, _ := clampFixture(t, b64)
		ct, err := newClientTransaction(page, js)
		if err != nil {
			t.Fatal(err)
		}
		c := classify(ct, page)
		take := false
		switch {
		case c.solver && solver < wantSolver:
			solver, take = solver+1, true
		case c.overflow && over < wantOver:
			over, take = over+1, true
		case c.negative && neg < wantNeg:
			neg, take = neg+1, true
		case !c.solver && !c.overflow && !c.negative && plain < wantPlain:
			plain, take = plain+1, true
		}
		if take {
			keys = append(keys, b64)
			count(c)
		}
	}
	if solver < wantSolver || over < wantOver || neg < wantNeg || plain < wantPlain {
		t.Fatalf("quotas not met: solver %d over %d neg %d plain %d", solver, over, neg, plain)
	}
	classes["plain"] = plain
	b, _ := json.MarshalIndent(map[string]any{
		"seed":    fmt.Sprintf("pcg(%d,%d)", seedA, seedB),
		"classes": classes,
		"keys":    keys,
	}, "", " ")
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d keys, classes %v", len(keys), classes)
}
