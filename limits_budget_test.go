package twitter

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T1 — header-authority gate honors the budget share: with limit 50 and pct 80
// the account may spend down to remaining == RateLimitHeadroom + 50*20/100 ==
// 12, so a window reporting 12 is held and one reporting 13 is allowed. The
// never-set pct keeps today's behaviour (held only at <= headroom).
//
// Mutation: account.go holdLocked — drop "+a.budgetShareLocked(endpoint,
// st.limit)" from the hold comparison => RED (remaining=12 is not held).
func TestBudgetPct_HeaderGateThreshold(t *testing.T) {
	cfg := ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute}
	reset := time.Now().Add(15 * time.Minute)

	report := func(acc *Account, remaining int) {
		acc.recordWindow("UserTweets", rateLimitObs{limit: 50, remaining: remaining, resetAt: reset})
		acc.holdIfExhausted("UserTweets") // mirrors observeRateLimit's post-response hold
	}

	t.Run("pct80/remaining12 held", func(t *testing.T) {
		acc := newRateTestAccount("u", cfg)
		acc.SetBudgetPct(80)
		report(acc, 12)
		assert.False(t, acc.AllowRequest("UserTweets"),
			"remaining 12 <= headroom 2 + withheld 10 must hold the endpoint")
	})

	t.Run("pct80/remaining13 allowed", func(t *testing.T) {
		acc := newRateTestAccount("u", cfg)
		acc.SetBudgetPct(80)
		report(acc, 13)
		assert.True(t, acc.AllowRequest("UserTweets"),
			"remaining 13 > 12 must allow exactly one more request")
		assert.False(t, acc.AllowRequest("UserTweets"),
			"the spend took the estimate to 12: the next pick must be refused")
	})

	t.Run("unset pct keeps headroom-only rule", func(t *testing.T) {
		held := newRateTestAccount("u", cfg)
		report(held, RateLimitHeadroom)
		assert.False(t, held.AllowRequest("UserTweets"),
			"remaining %d <= headroom must hold (today's behaviour)", RateLimitHeadroom)

		allowed := newRateTestAccount("u", cfg)
		report(allowed, RateLimitHeadroom+1)
		assert.True(t, allowed.AllowRequest("UserTweets"),
			"remaining %d > headroom must be allowed (today's behaviour)", RateLimitHeadroom+1)
	})
}

// T2 — cold/local path scales the limiter cap by the budget: a seeded 50 at
// pct 50 admits max(1, 50*50/100) == 25 requests in a window; the 26th is
// refused.
//
// Mutation: account.go scaledCapLocked — return the raw cap instead of
// scaledByPct(raw, a.budgetPct) => RED (50 requests allowed).
func TestBudgetPct_ColdPathScaledCap(t *testing.T) {
	acc := newRateTestAccount("u", ratelimit.Config{RequestsPerWindow: 500, WindowDuration: 15 * time.Minute})
	acc.SetBudgetPct(50)

	allowed := 0
	for i := 0; i < 50; i++ {
		if acc.AllowRequest("UserTweets") { // seeded 50; no header window => local limiter
			allowed++
		}
	}
	assert.Equal(t, 25, allowed, "pct 50 of seeded cap 50 must admit 25 requests")
	assert.False(t, acc.AllowRequest("UserTweets"), "the 26th request in the window must be refused")
}

// T3 — SetBudgetPct lands on a LIVE pooled account: lowering the share makes a
// spent-over-the-new-share account unpickable on the next pick, raising it
// back re-admits it — without rebuilding the pool or the limiter.
//
// Mutation: account.go SetBudgetPct — drop the rlCaps re-scale loop => RED
// (the post-update pick is still allowed by the stale scaled cap).
func TestBudgetPct_RuntimeUpdateChangesNextPick(t *testing.T) {
	acc := newRateTestAccount("u", ratelimit.Config{RequestsPerWindow: 500, WindowDuration: 15 * time.Minute})
	p := pool.New([]*Account{acc}, pool.DefaultConfig())

	pick := func() bool {
		_, err := p.Next(func(a *Account) bool { return a.AllowRequest("UserTweets") })
		return err == nil
	}
	for i := 0; i < 40; i++ {
		require.True(t, pick(), "pick %d: full budget admits up to the seeded 50", i+1)
	}

	acc.SetBudgetPct(10) // scaled cap 50*10/100 == 5; 40 already spent > 5
	assert.False(t, pick(), "pct 10 must deny the next pick once the spend exceeds the share")
	assert.Equal(t, 10, acc.BudgetPct())

	acc.SetBudgetPct(100)
	assert.True(t, pick(), "raising back to full budget re-admits the account")

	// Out-of-range values normalize to the full budget.
	for _, pct := range []int{0, -5, 101, 1000} {
		acc.SetBudgetPct(pct)
		assert.Equal(t, 100, acc.BudgetPct(), "SetBudgetPct(%d) must normalize to 100", pct)
	}
}

// TestBudgetPct_ConcurrentAccess exercises the mutex discipline the pool
// relies on: a live account's budget is rewritten while the pick filter and
// header-driven updates run against it. Deterministic assertions live in the
// tests above; this exists so `go test -race` sees the concurrent path.
func TestBudgetPct_ConcurrentAccess(t *testing.T) {
	acc := newRateTestAccount("u", ratelimit.Config{RequestsPerWindow: 500, WindowDuration: 15 * time.Minute})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			acc.SetBudgetPct(50)
			acc.SetBudgetPct(100)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = acc.AllowRequest("UserTweets")
			_ = acc.BudgetPct()
			acc.UpdateEndpointLimit("UserTweets", 50)
		}
	}()
	wg.Wait()
}

// T4 — HoldAccount: the held account is never picked before `until`, is picked
// again after, a hold on every account returns the same no-account error the
// pool produces today, and the hold charges nothing to account health.
//
// Mutation: account.go AllowRequest — delete the "now.Before(heldUntil)"
// early return => RED (the held account is picked and the call succeeds).
func TestHoldAccount_GatesPoolPick(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	f := newRLFakeX(t, func(string, int) rlWindow {
		return rlWindow{limit: 50, remaining: 40, reset: clk.Now().Add(15 * time.Minute)}
	})
	c, accs := newRLClient(t, 1, 50, clk)

	orig := poolWaitCap
	poolWaitCap = 300 * time.Millisecond
	t.Cleanup(func() { poolWaitCap = orig })

	until := clk.Now().Add(60 * time.Second)
	require.True(t, c.HoldAccount("A", until))
	require.False(t, c.HoldAccount("ghost", until), "no such account must return false")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.rlCall(ctx, f)
	require.Error(t, err, "every account held must surface the pool's no-account error")
	assert.Contains(t, err.Error(), "pool exhausted for "+rlEndpoint+" (requires auth)",
		"the error must be the same one today's empty-pool path returns")
	assert.Zero(t, f.hitsFor("tokA"), "a held account must never be picked")

	_, failed, _ := accs[0].Stats()
	assert.Zero(t, failed, "a hold is not a failure: no health charge")
	assert.True(t, accs[0].IsActive(), "a hold must not deactivate the account")

	clk.Advance(61 * time.Second) // past `until`
	require.NoError(t, c.rlCall(ctx, f), "the account is pickable again after the hold")
	assert.Equal(t, 1, f.hitsFor("tokA"))
}

// TestHoldAccount_WaitWakesAtUntil proves the hold participates in the
// pool's timed wait, not only the pick: a request issued during the hold
// sleeps until the hold expires and is then served.
func TestHoldAccount_WaitWakesAtUntil(t *testing.T) {
	f := newRLFakeX(t, func(string, int) rlWindow {
		return rlWindow{limit: 50, remaining: 40, reset: time.Now().Add(15 * time.Minute)}
	})
	c, _ := newRLClient(t, 1, 50, nil)

	require.True(t, c.HoldAccount("A", time.Now().Add(150*time.Millisecond)))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, c.rlCall(ctx, f), "the pick must wait out the hold and then be served")
	assert.Equal(t, 1, f.hitsFor("tokA"))
}

// T5 — ClientConfig.OnRequest fires once per pooled-account HTTP attempt, on
// the success AND the error path, carrying the shared-classifier outcome and
// the x-rate-limit-* header values.
//
// Mutation: client.go emitRequestEvent — return early on status >= 400 => RED
// (the 429 attempt emits no event; len(events) == 1).
func TestOnRequest_EmitsForSuccessAndError(t *testing.T) {
	reset := time.Now().Add(60 * time.Second).Truncate(time.Second)
	f := newRLFakeX(t, func(_ string, nth int) rlWindow {
		if nth == 1 {
			return rlWindow{limit: 50, remaining: 42, reset: reset}
		}
		return rlWindow{limit: 50, remaining: 0, reset: reset, status: http.StatusTooManyRequests}
	})
	c, _ := newRLClient(t, 1, 50, nil)

	orig := poolWaitCap
	poolWaitCap = 500 * time.Millisecond // bound the post-429 pool wait
	t.Cleanup(func() { poolWaitCap = orig })

	var mu sync.Mutex
	var events []RequestEvent
	c.cfg.OnRequest = func(ev RequestEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, c.rlCall(ctx, f))
	require.Error(t, c.rlCall(ctx, f), "the 429 exhausts the only account: the call fails after its attempt")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 2, "one event per HTTP attempt — success and error alike")

	assert.Equal(t, "A", events[0].Username)
	assert.Equal(t, rlEndpoint, events[0].Endpoint)
	assert.Equal(t, 200, events[0].Status)
	assert.Equal(t, "ok", events[0].Outcome)
	assert.Equal(t, 50, events[0].RateLimit.Limit)
	assert.Equal(t, 42, events[0].RateLimit.Remaining)
	assert.Equal(t, reset.Unix(), events[0].RateLimit.Reset.Unix())

	assert.Equal(t, "A", events[1].Username)
	assert.Equal(t, 429, events[1].Status)
	assert.Equal(t, "rate_limited", events[1].Outcome)
	assert.Equal(t, 0, events[1].RateLimit.Remaining)
}

// T6 — EndpointLimits returns a copy: mutating the result cannot change what
// new accounts are seeded with.
//
// Mutation: ratelimit_adaptive.go EndpointLimits — return endpointDefaultLimit
// itself instead of the copy => RED (the seeded cap collapses to 1).
func TestEndpointLimits_ReturnsCopy(t *testing.T) {
	mut := EndpointLimits()
	mut["UserTweets"] = 1
	mut["SearchTimeline"] = 1

	acc := newRateTestAccount("u", ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute})
	allowed := 0
	for i := 0; i < 60; i++ {
		if acc.AllowRequest("UserTweets") {
			allowed++
		}
	}
	assert.Equal(t, 50, allowed, "mutating the returned map must not change seeding")
	assert.Equal(t, 187, EndpointLimits()["SearchTimeline"], "the table itself must be unchanged")
}

// The device-follow REST call's limiter key is the deviceFollowEndpoint
// constant the request path passes to AllowRequest/observeRateLimit — assert
// the measured table carries it under exactly that key (a drifted literal
// would silently seed nothing).
func TestEndpointLimits_DeviceFollowKey(t *testing.T) {
	assert.Equal(t, 180, EndpointLimits()["DeviceFollow"])
	assert.Equal(t, "DeviceFollow", deviceFollowEndpoint,
		"the table key must be the same identifier the limiter path uses")
	assert.True(t, strings.HasPrefix(deviceFollowEndpoint, "Device"), "key sanity")
}
