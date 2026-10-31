package xtid

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The animation path is found by its position (svg > g > second path), not by a
// fill colour that a redesign can change. Recolouring every fill in the real
// captured page must not change the extracted frames or the animation key.
func TestGetSVGFrames_StructuralLookupSurvivesFillChange(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "home_real.html"))
	if err != nil {
		t.Fatal(err)
	}
	orig := string(b)
	const fill = `fill="#1d9bf008"`
	if !strings.Contains(orig, fill) {
		t.Fatal("fixture no longer carries the animation fill; test would be vacuous")
	}
	recolored := strings.ReplaceAll(orig, fill, `fill="#abcdef12"`)
	if strings.Contains(recolored, "1d9bf008") {
		t.Fatal("recolouring left the old fill behind")
	}

	want := getSVGFrames(orig)
	got := getSVGFrames(recolored)
	for i, f := range want {
		if len(f.data) == 0 {
			t.Fatalf("original fixture frame %d is empty", i)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recoloured frames differ from the original")
	}

	// Same 2D array and animation key through the full ClientTransaction path.
	js, err := os.ReadFile(filepath.Join("testdata", "ondemand_real.js"))
	if err != nil {
		t.Fatal(err)
	}
	// Page needs the verification meta; reuse the fixture's own, else a fixed one.
	page := func(h string) string {
		if getVerificationKey(h) != "" {
			return h
		}
		return h + metaTag(realFixtureKey())
	}
	ctA, errA := newClientTransaction(page(orig), string(js))
	ctB, errB := newClientTransaction(page(recolored), string(js))
	if errA != nil || errB != nil {
		t.Fatalf("newClientTransaction: %v / %v", errA, errB)
	}
	if ctA.animationKey == "" || ctA.animationKey != ctB.animationKey {
		t.Fatalf("animation key changed with the fill: %q vs %q", ctA.animationKey, ctB.animationKey)
	}
}

// realFixtureKey is a 48-byte key so the real bundle's indices (up to 47) resolve.
func realFixtureKey() []byte {
	k := make([]byte, 48)
	for i := range k {
		k[i] = byte(i*7 + 3)
	}
	return k
}

// The fill match stays as a fallback for markup without the structural shape
// (the hand-authored fixtures have no <g> wrapper).
func TestGetSVGFrames_FillFallback(t *testing.T) {
	html := `<svg id="loading-x-anim-0"><path fill="#1d9bf008" d="M0 0C1 2 3 4 5 6 7 8 9 10 11 12"></path></svg>`
	got := getSVGFrames(html)[0].data
	want := [][]int{{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback frame = %v, want %v", got, want)
	}
}
