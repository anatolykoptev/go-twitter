package twitter

import (
	"context"
	"testing"
	"time"
)

// pacedConfig returns a ClientConfig with small but nonzero pace values so the
// test exercises the real spacing behaviour in milliseconds rather than the
// production 4.5s defaults. defaults() is intentionally NOT called so the small
// values survive (defaults() would only fill zero fields anyway).
func pacedConfig(min, rnd time.Duration) ClientConfig {
	return ClientConfig{DomainPaceMin: min, DomainPaceRandom: rnd}
}

// TestBuildDomainPacer_Defaults proves the production safe defaults are applied
// by defaults(): 4.5s floor + 4.5s jitter for the Twitter hosts. This is the
// stealth-critical invariant — the floor must never silently drop to zero.
func TestBuildDomainPacer_Defaults(t *testing.T) {
	cfg := ClientConfig{}
	cfg.defaults()
	if cfg.DomainPaceMin != 4500*time.Millisecond {
		t.Fatalf("DomainPaceMin default = %v, want 4.5s", cfg.DomainPaceMin)
	}
	if cfg.DomainPaceRandom != 4500*time.Millisecond {
		t.Fatalf("DomainPaceRandom default = %v, want 4.5s", cfg.DomainPaceRandom)
	}
	if p := buildDomainPacer(cfg); p == nil {
		t.Fatal("buildDomainPacer returned nil with pacing enabled")
	}
}

// TestBuildDomainPacer_Disabled proves the explicit kill-switch yields a nil
// pacer (legacy behaviour: only anti-fingerprint jitter applies).
func TestBuildDomainPacer_Disabled(t *testing.T) {
	cfg := ClientConfig{DomainPaceDisabled: true}
	cfg.defaults()
	if p := buildDomainPacer(cfg); p != nil {
		t.Fatal("buildDomainPacer should return nil when DomainPaceDisabled")
	}
}

// TestDomainPace_BurstSpacedAtLeastMinDelay drives the REAL pacer (the same
// instance NewClient builds via buildDomainPacer) the same way doPoolRequest
// does — Wait(ctx, url) before each request — and proves a burst of requests to
// x.com is spaced at least MinDelay apart. The first request fires immediately
// (no prior timestamp); every subsequent one waits >= MinDelay.
func TestDomainPace_BurstSpacedAtLeastMinDelay(t *testing.T) {
	const (
		minDelay = 20 * time.Millisecond
		rndDelay = 20 * time.Millisecond
		n        = 8
	)
	pacer := buildDomainPacer(pacedConfig(minDelay, rndDelay))
	if pacer == nil {
		t.Fatal("nil pacer")
	}
	ctx := context.Background()
	url := "https://x.com/i/api/graphql/abc/Retweeters"

	stamps := make([]time.Time, 0, n)
	for range n {
		if err := pacer.Wait(ctx, url); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		stamps = append(stamps, time.Now())
	}

	// Wait polls every 50ms, so the realized spacing carries scheduler slop; the
	// hard guarantee under test is the MinDelay FLOOR, allowing a small tolerance
	// below it only for clock granularity. We assert each gap >= minDelay - slop.
	const slop = 5 * time.Millisecond
	for i := 1; i < len(stamps); i++ {
		gap := stamps[i].Sub(stamps[i-1])
		if gap < minDelay-slop {
			t.Fatalf("gap[%d]=%v below MinDelay floor %v (burst leaked through pacing)", i, gap, minDelay)
		}
	}
}

// TestDomainPace_SpacingIsVariable proves the spacing is NOT fixed — RandomDelay
// makes consecutive gaps differ, which is the human-like-rhythm property that
// distinguishes this from a constant tick a fingerprinter could detect. Run a
// burst, collect gaps, require at least two distinct values.
func TestDomainPace_SpacingIsVariable(t *testing.T) {
	// DomainLimiter.Wait polls Allow every 50ms, so realized spacing quantizes to
	// the 50ms poll period. To prove genuine variance at THAT resolution, use a
	// random span several poll-buckets wide (50ms floor + up-to-300ms jitter =>
	// realized gaps land across ~6 distinct 50ms buckets). At production values
	// (4.5s + 4.5s) the 50ms quantization is negligible and spacing is finely
	// variable; this scaled-down test asserts the same property runs fast.
	const (
		minDelay = 50 * time.Millisecond
		rndDelay = 300 * time.Millisecond
		n        = 20
	)
	pacer := buildDomainPacer(pacedConfig(minDelay, rndDelay))
	ctx := context.Background()
	url := "https://x.com/i/api/graphql/abc/Followers"

	gaps := make([]time.Duration, 0, n-1)
	prev := time.Now()
	first := true
	for range n {
		if err := pacer.Wait(ctx, url); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		now := time.Now()
		if !first {
			gaps = append(gaps, now.Sub(prev))
		}
		first = false
		prev = now
	}

	// Distinct-gap count: with a 60ms random span and 50ms poll granularity, the
	// realized gaps must span more than one bucket. Require >= 3 distinct values
	// (quantized to 10ms) to assert genuine variance, not a constant tick.
	seen := map[int64]struct{}{}
	for _, g := range gaps {
		seen[int64(g/(50*time.Millisecond))] = struct{}{} // bucket at the 50ms poll period
	}
	if len(seen) < 3 {
		t.Fatalf("spacing not variable enough: only %d distinct 50ms gap buckets across %d gaps (%v)", len(seen), len(gaps), gaps)
	}
}

// TestDomainPace_NonTwitterHostNotPaced proves the pacer only gates the Twitter
// hosts — an unmatched domain returns immediately (no MinDelay), so the pace is
// scoped and never accidentally throttles unrelated traffic sharing the client.
func TestDomainPace_NonTwitterHostNotPaced(t *testing.T) {
	pacer := buildDomainPacer(pacedConfig(500*time.Millisecond, 0))
	ctx := context.Background()
	start := time.Now()
	for range 5 {
		if err := pacer.Wait(ctx, "https://example.com/whatever"); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("non-Twitter host was paced (%v elapsed); pacing must be x.com/twitter.com scoped", elapsed)
	}
}
