package xtid

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Golden vectors produced by the reference implementation
// (testdata/reference/gen/README.md): every value below was computed by
// x-client-transaction-id, not by this package, and each must match byte for
// byte. Regenerate with `node gen.mjs` in testdata/reference/gen.

type goldenMeta struct {
	Name             string `json:"name"`
	Page             string `json:"page"`
	Ondemand         string `json:"ondemand"`
	OndemandURL      string `json:"ondemandUrl"`
	RealAnimationKey string `json:"realAnimationKey"`
	Stats            struct {
		ClampCases      int `json:"clampCases"`
		EpsilonCases    int `json:"epsilonCases"`
		KeyClampCases   int `json:"keyClampCases"`
		KeyEpsilonCases int `json:"keyEpsilonCases"`
		KeyTieCases     int `json:"keyTieCases"`
	} `json:"stats"`
}

type goldenSet struct {
	Meta    goldenMeta  `json:"meta"`
	Animate [][]any     `json:"animate"` // [frame, row, t, key]
	Keys    [][2]string `json:"keys"`    // [key bytes (base64), animation key]
	IDs     [][]any     `json:"ids"`     // [method, path, timeNow, randomByte, id]
}

type goldenFile struct {
	Reference struct {
		Package, Version, Integrity string
	} `json:"reference"`
	Round2   [][2]float64 `json:"round2"`
	Runtimes [][]any      `json:"runtimes"` // [html, url or null]
	Sets     []goldenSet  `json:"sets"`
}

func loadGolden(t *testing.T) (goldenFile, string) {
	t.Helper()
	dir := filepath.Join("testdata", "reference")
	raw, err := os.ReadFile(filepath.Join(dir, "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("vectors.json: %v", err)
	}
	return g, dir
}

// report fails t with the first few mismatches and the total.
func report(t *testing.T, what string, n int, bad []string) {
	t.Helper()
	if len(bad) == 0 {
		return
	}
	if len(bad) > 5 {
		bad = append(bad[:5], fmt.Sprintf("... and %d more", len(bad)-5))
	}
	t.Fatalf("%s: %d of %d vectors differ from the reference:\n  %s", what, len(bad), n, strings.Join(bad, "\n  "))
}

func TestReferenceGolden(t *testing.T) {
	g, dir := loadGolden(t)
	if g.Reference.Package != "x-client-transaction-id" || g.Reference.Integrity == "" {
		t.Fatalf("vectors.json must name its reference package and integrity, got %+v", g.Reference)
	}

	t.Run("round2", func(t *testing.T) {
		var bad []string
		for _, v := range g.Round2 {
			if got := round2(v[0]); got != v[1] {
				bad = append(bad, fmt.Sprintf("round2(%v) = %v, want %v", v[0], got, v[1]))
			}
		}
		if len(g.Round2) < 50 {
			t.Fatalf("only %d tie vectors", len(g.Round2))
		}
		report(t, "round2", len(g.Round2), bad)
	})

	t.Run("runtimes", func(t *testing.T) {
		var bad []string
		for _, v := range g.Runtimes {
			want := ""
			if v[1] != nil {
				want = v[1].(string)
			}
			if got := onDemandURLFromRuntime(v[0].(string)); got != want {
				bad = append(bad, fmt.Sprintf("onDemandURLFromRuntime = %q, want %q for %s", got, want, v[0]))
			}
		}
		report(t, "runtimes", len(g.Runtimes), bad)
	})

	totalKeys := 0
	for _, set := range g.Sets {
		totalKeys += len(set.Keys)
	}
	if totalKeys < 1000 {
		t.Fatalf("golden file holds %d end-to-end keys, want >= 1000", totalKeys)
	}

	for _, set := range g.Sets {
		set := set
		t.Run(set.Meta.Name, func(t *testing.T) {
			page := readText(t, filepath.Join(dir, set.Meta.Page))
			js := readText(t, filepath.Join(dir, set.Meta.Ondemand))
			ct, err := newClientTransaction(page, js)
			if err != nil {
				t.Fatal(err)
			}
			frames := getSVGFrames(page)
			s := set.Meta.Stats
			if s.ClampCases == 0 || s.EpsilonCases == 0 || s.KeyClampCases == 0 || s.KeyEpsilonCases == 0 || s.KeyTieCases == 0 {
				t.Fatalf("vector sample lost a case class: %+v", s)
			}

			t.Run("real_key", func(t *testing.T) {
				if ct.animationKey != set.Meta.RealAnimationKey {
					t.Fatalf("animation key = %q, want %q", ct.animationKey, set.Meta.RealAnimationKey)
				}
			})

			t.Run("animate", func(t *testing.T) {
				var bad []string
				for _, v := range set.Animate {
					f, r, tt, want := int(v[0].(float64)), int(v[1].(float64)), v[2].(float64), v[3].(string)
					if got := ct.animate(frames[f].data[r], tt); got != want {
						bad = append(bad, fmt.Sprintf("animate(frame %d, row %d, t=%v) = %q, want %q", f, r, tt, got, want))
					}
				}
				report(t, "animate", len(set.Animate), bad)
			})

			t.Run("keys", func(t *testing.T) {
				var bad []string
				for _, v := range set.Keys {
					kb, err := base64.StdEncoding.DecodeString(v[0])
					if err != nil {
						t.Fatal(err)
					}
					ct.keyBytes = kb
					got, err := ct.buildAnimationKey(page)
					if err != nil || got != v[1] {
						bad = append(bad, fmt.Sprintf("key %s... = %q (err %v), want %q", v[0][:8], got, err, v[1]))
					}
				}
				report(t, "keys", len(set.Keys), bad)
			})

			t.Run("ids", func(t *testing.T) {
				ct2, err := newClientTransaction(page, js)
				if err != nil {
					t.Fatal(err)
				}
				var bad []string
				for _, v := range set.IDs {
					m, p, tn, rv, want := v[0].(string), v[1].(string), int64(v[2].(float64)), byte(v[3].(float64)), v[4].(string)
					ct2.now = func() time.Time { return time.UnixMilli(1682924400000 + tn*1000) }
					ct2.randByte = func() byte { return rv }
					if got := ct2.GenerateID(m, p); got != want {
						bad = append(bad, fmt.Sprintf("GenerateID(%s, %s, t=%d, r=%d) = %q, want %q", m, p, tn, rv, got, want))
					}
				}
				report(t, "ids", len(set.IDs), bad)
			})

			if set.Meta.OndemandURL != "" {
				t.Run("manager_logged_out", func(t *testing.T) {
					f := &goldenFetcher{page: []byte(page), js: []byte(js)}
					m := NewManager(f)
					m.SetKeySource(KeySourceLoggedOut)
					if err := m.Initialize(); err != nil {
						t.Fatal(err)
					}
					if len(f.seen) != 2 || f.seen[0] != LoggedOutPageURL || f.seen[1] != set.Meta.OndemandURL {
						t.Fatalf("fetched %v, want [%s %s]", f.seen, LoggedOutPageURL, set.Meta.OndemandURL)
					}
					if m.ct.animationKey != set.Meta.RealAnimationKey {
						t.Fatalf("manager animation key = %q, want %q", m.ct.animationKey, set.Meta.RealAnimationKey)
					}
				})
			}
		})
	}
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// goldenFetcher serves the saved logged-out page and its indices script, and
// records every URL requested.
type goldenFetcher struct {
	mu       sync.Mutex
	page, js []byte
	seen     []string
}

func (f *goldenFetcher) Fetch(_ context.Context, u string) ([]byte, error) {
	f.mu.Lock()
	f.seen = append(f.seen, u)
	f.mu.Unlock()
	if u == LoggedOutPageURL {
		return f.page, nil
	}
	if strings.Contains(u, "ondemand.s") {
		return f.js, nil
	}
	return nil, fmt.Errorf("unexpected URL %s (HTTP 404)", u)
}
