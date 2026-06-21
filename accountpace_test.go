package twitter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
)

// newRateTestAccount builds an Account wired with a real per-endpoint limiter
// seeded with the measured defaults, exactly as NewClient does. It does NOT
// stand up the HTTP stack — these tests exercise the rate/pace policy directly
// via the same calls request.go's filter and adaptive-sync make.
func newRateTestAccount(username string, cfg ratelimit.Config) *Account {
	a := &Account{Username: username, active: true}
	a.rateLimiter = ratelimit.NewLimiter(cfg)
	seedRateLimits(a)
	a.HealthTracker = pool.DefaultHealthTracker()
	return a
}

// --- (a) adaptive limiter updates from x-rate-limit-* headers ---------------

func TestParseRateLimitLimit(t *testing.T) {
	cases := []struct {
		in       string
		wantN    int
		wantOK   bool
		whatever string
	}{
		{"187", 187, true, "valid"},
		{"500", 500, true, "valid high"},
		{"", 0, false, "absent header"},
		{"abc", 0, false, "malformed"},
		{"0", 0, false, "zero ignored"},
		{"-5", 0, false, "negative ignored"},
	}
	for _, tc := range cases {
		n, ok := parseRateLimitLimit(tc.in)
		if n != tc.wantN || ok != tc.wantOK {
			t.Errorf("parseRateLimitLimit(%q) [%s] = (%d,%v), want (%d,%v)", tc.in, tc.whatever, n, ok, tc.wantN, tc.wantOK)
		}
	}
}

// TestSyncRateLimit_RaisesCapFromHeader proves a 200/429 response carrying
// x-rate-limit-limit raises the account's effective per-endpoint cap above the
// generic static cap — the core adaptive behaviour driven by X's real numbers.
func TestSyncRateLimit_RaisesCapFromHeader(t *testing.T) {
	// Static config cap is 50; seedRateLimits already primes Followers to 187,
	// so start from a NON-seeded endpoint to isolate the header-driven update.
	acc := newRateTestAccount("u", ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute})

	const ep = "SomeUnseededOp" // falls back to static 50 until a header arrives
	acc.SyncRateLimit(ep, map[string]string{xRateLimitLimit: "300"})

	allowed := 0
	for i := 0; i < 320; i++ {
		if acc.AllowRequest(ep) {
			allowed++
		}
	}
	if allowed != 300 {
		t.Fatalf("after SyncRateLimit to 300, expected 300 allowed, got %d", allowed)
	}
}

// TestSyncRateLimit_AbsentHeaderKeepsSeed proves an absent/malformed header is a
// no-op: the seeded default cap stands (never collapses to deny-everything).
func TestSyncRateLimit_AbsentHeaderKeepsSeed(t *testing.T) {
	acc := newRateTestAccount("u", ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute})

	// Followers is seeded to 187. A response with no rate-limit header must not
	// change that.
	acc.SyncRateLimit("Followers", map[string]string{})
	acc.SyncRateLimit("Followers", map[string]string{xRateLimitLimit: "garbage"})

	allowed := 0
	for i := 0; i < 200; i++ {
		if acc.AllowRequest("Followers") {
			allowed++
		}
	}
	if allowed != 187 {
		t.Fatalf("absent/malformed header must keep seeded cap 187, got %d", allowed)
	}
}

// --- seedRateLimits: per-endpoint measured defaults -------------------------

func TestSeedRateLimits_AppliesMeasuredCaps(t *testing.T) {
	acc := newRateTestAccount("u", ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute})

	want := map[string]int{
		"Followers":        187,
		"Following":        187,
		"Retweeters":       500,
		"UserByScreenName": 150,
		"UserTweets":       50,
	}
	for ep, cap := range want {
		allowed := 0
		for i := 0; i < cap+10; i++ {
			if acc.AllowRequest(ep) {
				allowed++
			}
		}
		if allowed != cap {
			t.Errorf("endpoint %s: seeded cap = %d allowed, want %d", ep, allowed, cap)
		}
	}
}

// TestSeedRateLimits_UnmeasuredEndpointFallsBack proves an endpoint not in the
// measured map uses the generic static cap (conservative), refined later by the
// adaptive header sync.
func TestSeedRateLimits_UnmeasuredEndpointFallsBack(t *testing.T) {
	acc := newRateTestAccount("u", ratelimit.Config{RequestsPerWindow: defaultEndpointLimit, WindowDuration: 15 * time.Minute})

	allowed := 0
	for i := 0; i < defaultEndpointLimit+20; i++ {
		if acc.AllowRequest("BrandNewOp") {
			allowed++
		}
	}
	if allowed != defaultEndpointLimit {
		t.Fatalf("unmeasured endpoint should fall back to %d, got %d", defaultEndpointLimit, allowed)
	}
}

// --- (b) per-account pacing keyed by account --------------------------------

func TestBuildAccountPacer_Defaults(t *testing.T) {
	cfg := ClientConfig{}
	cfg.defaults()
	if cfg.AccountPaceMin != defaultAccountPaceMin {
		t.Fatalf("AccountPaceMin default = %v, want %v", cfg.AccountPaceMin, defaultAccountPaceMin)
	}
	if cfg.AccountPaceRandom != defaultAccountPaceRandom {
		t.Fatalf("AccountPaceRandom default = %v, want %v", cfg.AccountPaceRandom, defaultAccountPaceRandom)
	}
	if p := buildAccountPacer(cfg); p == nil {
		t.Fatal("buildAccountPacer returned nil with pacing enabled")
	}
}

func TestBuildAccountPacer_Disabled(t *testing.T) {
	cfg := ClientConfig{AccountPaceDisabled: true}
	cfg.defaults()
	if p := buildAccountPacer(cfg); p != nil {
		t.Fatal("buildAccountPacer should return nil when AccountPaceDisabled")
	}
}

// TestAccountPace_SpacesPerAccountNotGlobal drives the REAL pacer the same way
// doPoolRequest does — Wait(ctx, acc.ID()) — and proves spacing is per-account:
// account A is gated by its own recent request, but account B fires immediately.
// This is the no-starvation property at the pacer level (vs the rejected global
// per-domain gate that would block B behind A).
func TestAccountPace_SpacesPerAccountNotGlobal(t *testing.T) {
	pacer := ratelimit.NewKeyedPacer(50*time.Millisecond, 0)
	ctx := context.Background()

	// First request per account fires immediately.
	if err := pacer.Wait(ctx, "acctA"); err != nil {
		t.Fatalf("acctA first Wait: %v", err)
	}

	// acctB (different key) must fire immediately even though acctA just did and
	// is still inside its 50ms window.
	start := time.Now()
	if err := pacer.Wait(ctx, "acctB"); err != nil {
		t.Fatalf("acctB Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("acctB was gated by acctA's recent request (%v); pace must be per-account", elapsed)
	}
}

// --- (c) MULTI-SCRAPER NO-STARVATION regression -----------------------------

// TestMultiScraper_LowFreqNotStarved reproduces the production incident: a
// low-frequency scraper (seed) sharing the 4-account pool with two
// high-frequency scrapers (KOL + VC) must still acquire accounts. The driver
// uses the EXACT filter request.go applies (AllowRequest gated per-endpoint) and
// the EXACT per-account pacer, against the real pool.Next, so the test exercises
// the shipped no-starvation path, not a copy.
//
// Under the rejected global per-domain gate, the high-frequency callers held the
// single shared timestamp and the low-frequency caller got zero accounts. With
// per-account pacing + per-account-per-endpoint limits, the pool always has a
// ready account for the seed caller.
func TestMultiScraper_LowFreqNotStarved(t *testing.T) {
	cfg := ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute}
	accs := []*Account{
		newRateTestAccount("a", cfg),
		newRateTestAccount("b", cfg),
		newRateTestAccount("c", cfg),
		newRateTestAccount("d", cfg),
	}
	p := pool.New(accs, pool.DefaultConfig())
	pacer := ratelimit.NewKeyedPacer(5*time.Millisecond, 5*time.Millisecond)

	// acquire mirrors request.go: select an account whose per-endpoint limiter
	// allows the call, then pace it by account. Returns whether an account was
	// served.
	acquire := func(ctx context.Context, endpoint string) bool {
		filter := func(a *Account) bool { return a.AllowRequest(endpoint) }
		acc, err := p.Next(filter)
		if err != nil {
			return false
		}
		_ = pacer.Wait(ctx, acc.ID())
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var seedServed, kolServed, vcServed atomic.Int64
	var wg sync.WaitGroup

	// Two high-frequency callers hammer the pool continuously.
	highFreq := func(endpoint string, counter *atomic.Int64) {
		defer wg.Done()
		for ctx.Err() == nil {
			if acquire(ctx, endpoint) {
				counter.Add(1)
			}
		}
	}
	wg.Add(2)
	go highFreq("Retweeters", &kolServed) // KOL
	go highFreq("Following", &vcServed)   // VC

	// Low-frequency caller (seed) requests at a modest cadence. It must be served
	// on essentially every attempt despite the two hammering callers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		attempts := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				attempts++
				if acquire(ctx, "Followers") {
					seedServed.Add(1)
				}
				if attempts >= 40 {
					return
				}
			}
		}
	}()

	wg.Wait()

	seed := seedServed.Load()
	// The seed caller made ~40 attempts; under starvation it would be near 0.
	// Assert it was served on the large majority of its attempts.
	if seed < 30 {
		t.Fatalf("seed scraper starved: served only %d of ~40 attempts (kol=%d, vc=%d)",
			seed, kolServed.Load(), vcServed.Load())
	}
	// Sanity: the high-frequency callers were actually competing for the pool.
	if kolServed.Load() == 0 || vcServed.Load() == 0 {
		t.Fatalf("high-freq callers did not run (kol=%d, vc=%d); test did not exercise contention",
			kolServed.Load(), vcServed.Load())
	}
}

// --- (d) aggregate throughput ≈ Σ per-account-per-endpoint caps -------------

// TestAggregateThroughput_SumsPerAccountCaps proves the pool's total budget for
// one endpoint over a window is the SUM of each account's per-endpoint cap (NOT
// a single global cap). With 4 accounts at the Followers cap of 187, the pool
// must serve ~4*187 = 748 requests before exhaustion — the capacity that the
// static global-style 50/15m bug was starving.
func TestAggregateThroughput_SumsPerAccountCaps(t *testing.T) {
	const n = 4
	cfg := ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute}
	accs := make([]*Account, n)
	for i := range accs {
		accs[i] = newRateTestAccount(string(rune('a'+i)), cfg)
	}
	p := pool.New(accs, pool.DefaultConfig())

	const ep = "Followers" // seeded to 187 per account
	served := 0
	for {
		acc, err := p.Next(func(a *Account) bool { return a.AllowRequest(ep) })
		if err != nil {
			break // all accounts exhausted for this endpoint+window
		}
		_ = acc
		served++
		if served > n*187+50 { // safety stop
			break
		}
	}

	want := n * 187 // 748
	// Allow a small tolerance for round-robin edge effects.
	if served < want-n || served > want {
		t.Fatalf("aggregate Followers throughput = %d, want ≈ %d (Σ per-account caps, not a global cap)", served, want)
	}
}

// --- (e) backward-compat: construction paths still behave -------------------

// TestDefaults_BareConfigFillsPaceAndRateLimit proves a bare ClientConfig (the
// search.go / verify.go construction) gets safe pace + rate-limit defaults and
// does NOT carry any removed DomainPace* fields (compile-time guarantee that the
// migration is complete is the build itself; this asserts runtime defaults).
func TestDefaults_BareConfigFillsPaceAndRateLimit(t *testing.T) {
	cfg := ClientConfig{}
	cfg.defaults()
	if cfg.RateLimit.RequestsPerWindow == 0 || cfg.RateLimit.WindowDuration == 0 {
		t.Fatalf("bare config did not get a default RateLimit: %+v", cfg.RateLimit)
	}
	if cfg.AccountPaceMin <= 0 || cfg.AccountPaceRandom <= 0 {
		t.Fatalf("bare config did not get default account pace: min=%v rnd=%v", cfg.AccountPaceMin, cfg.AccountPaceRandom)
	}
}

// TestDefaults_RateLimitOnlyConfigPreserved proves a RateLimit-only config (the
// social.go / go-hully construction) keeps the caller's RateLimit and still
// fills the pace defaults.
func TestDefaults_RateLimitOnlyConfigPreserved(t *testing.T) {
	cfg := ClientConfig{RateLimit: ratelimit.Config{RequestsPerWindow: 123, WindowDuration: 10 * time.Minute}}
	cfg.defaults()
	if cfg.RateLimit.RequestsPerWindow != 123 || cfg.RateLimit.WindowDuration != 10*time.Minute {
		t.Fatalf("caller RateLimit was overwritten: %+v", cfg.RateLimit)
	}
	if cfg.AccountPaceMin <= 0 {
		t.Fatal("pace default not filled for RateLimit-only config")
	}
}
