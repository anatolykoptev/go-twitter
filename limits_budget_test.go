package twitter

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T1 — header-authority gate honours the budget share: with limit 50 and
// pct 80 the account may spend max(1, 50*80/100) == 40 requests, so the gate
// holds the endpoint once the window's remaining estimate reaches
// 50-40 == 10 — the exact complement of the scaled cap the cold path enforces
// (see TestBudgetPct_GateParity). The never-set pct keeps today's behaviour:
// only a response at or below RateLimitHeadroom persists a hold.
//
// Mutation: account.go gateBlockedLocked — drop the budget-floor union
// (return a.blockedUntil[endpoint] unchanged) => RED (a budget-spent window
// is allowed).
func TestBudgetPct_HeaderGateThreshold(t *testing.T) {
	cfg := ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute}
	reset := time.Now().Add(15 * time.Minute)

	report := func(acc *Account, remaining int) {
		acc.recordWindow("UserTweets", rateLimitObs{limit: 50, remaining: remaining, resetAt: reset})
		acc.holdIfExhausted("UserTweets") // mirrors observeRateLimit's post-response hold
	}

	t.Run("pct80/remaining10 held", func(t *testing.T) {
		acc := newRateTestAccount("u", cfg)
		acc.SetBudgetPct(80)
		report(acc, 10)
		assert.False(t, acc.AllowRequest("UserTweets"),
			"remaining 10 <= withheld floor 10 (limit 50 − spend 40) must hold the endpoint")
	})

	t.Run("pct80/remaining11 allowed once", func(t *testing.T) {
		acc := newRateTestAccount("u", cfg)
		acc.SetBudgetPct(80)
		report(acc, 11)
		assert.True(t, acc.AllowRequest("UserTweets"),
			"remaining 11 > 10 must allow exactly one more request")
		assert.False(t, acc.AllowRequest("UserTweets"),
			"the spend took the estimate to 10: the next pick must be refused")
	})

	t.Run("unset pct keeps reported-exhaustion rule", func(t *testing.T) {
		held := newRateTestAccount("u", cfg)
		report(held, RateLimitHeadroom)
		assert.False(t, held.AllowRequest("UserTweets"),
			"a response at remaining %d <= headroom persists the hold", RateLimitHeadroom)

		allowed := newRateTestAccount("u", cfg)
		report(allowed, RateLimitHeadroom+1)
		assert.True(t, allowed.AllowRequest("UserTweets"),
			"remaining %d > headroom must be allowed (today's behaviour)", RateLimitHeadroom+1)
	})
}

// A runtime SetBudgetPct re-gates the NEXT pick: a hold owed only to the
// budget share is derived from rlState under the CURRENT pct and never
// persisted, so raising the share releases it at once — while X's own
// exhaustion (a response at or below RateLimitHeadroom, or a real 429) keeps
// the endpoint held until the window resets.
//
// Mutation: account.go gateBlockedLocked — persist the derived band again
// (a.blockedUntil[endpoint] = st.resetAt inside the budget-floor branch) =>
// RED (the raise no longer releases the account).
func TestBudgetPct_SetBudgetPctReleasesBudgetHold(t *testing.T) {
	cfg := ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute}
	reset := time.Now().Add(15 * time.Minute)

	report := func(acc *Account, remaining int) {
		acc.recordWindow("UserTweets", rateLimitObs{limit: 50, remaining: remaining, resetAt: reset})
		acc.holdIfExhausted("UserTweets")
	}

	t.Run("raise releases a budget-only hold", func(t *testing.T) {
		acc := newRateTestAccount("u", cfg)
		acc.SetBudgetPct(80)
		report(acc, 8) // floor = 50-40 = 10: 8 <= 10 => budget-held
		require.False(t, acc.AllowRequest("UserTweets"),
			"remaining 8 <= floor 10 must hold at pct 80")
		acc.SetBudgetPct(100)
		assert.True(t, acc.AllowRequest("UserTweets"),
			"raising the share must release a budget-only hold on the next pick")
	})

	t.Run("lower holds on the next pick", func(t *testing.T) {
		acc := newRateTestAccount("u", cfg)
		report(acc, 8) // pct 100: floor is the headroom 2; 8 > 2 => allowed
		require.True(t, acc.AllowRequest("UserTweets"),
			"pct 100 admits while the window has remaining")
		acc.SetBudgetPct(80)
		assert.False(t, acc.AllowRequest("UserTweets"),
			"lowering to 80 must hold the next pick (estimate 7 <= floor 10)")
	})

	t.Run("reported exhaustion is not released", func(t *testing.T) {
		acc := newRateTestAccount("u", cfg)
		report(acc, RateLimitHeadroom) // X's own exhaustion: persisted
		require.False(t, acc.AllowRequest("UserTweets"))
		acc.SetBudgetPct(100)
		assert.False(t, acc.AllowRequest("UserTweets"),
			"a response at <= headroom is X's word, not the budget: raising pct must not release it")
	})

	t.Run("a real 429 hold is not released", func(t *testing.T) {
		acc := newRateTestAccount("u", cfg)
		acc.SetBudgetPct(50)
		report(acc, 30)                                  // floor = 50-25 = 25: 30 > 25 => not budget-held
		acc.MarkEndpointRateLimited("UserTweets", reset) // the 429 arm's persist
		acc.SetBudgetPct(100)
		assert.False(t, acc.AllowRequest("UserTweets"),
			"an explicit 429 mark survives a budget raise until the reset")
		assert.True(t, acc.IsEndpointRateLimited("UserTweets"))
	})
}

// The header gate and the cold-path counter enforce ONE budget formula: an
// account may spend max(1, limit*pct/100) requests of a window on either
// path, so from a fresh window (remaining == limit) both gates must admit the
// same count at every pct — with one documented exception: the header band
// always keeps RateLimitHeadroom slots for requests already in flight (X's
// remaining does not count them), so at pct 99/100 it admits 48 of 50, up to
// 2 fewer than the cold counter.
//
// Mutation: account.go budgetFloorLocked — return
// RateLimitHeadroom+limit-scaledByPct(limit, a.budgetPct) (the old
// headroom+withheld threshold) => RED (the header column comes up short);
// drop the max(RateLimitHeadroom, ...) clamp => RED at pct 99/100 (the header
// column spends the headroom).
func TestBudgetPct_GateParity(t *testing.T) {
	cfg := ratelimit.Config{RequestsPerWindow: 500, WindowDuration: 15 * time.Minute}
	for _, pct := range []int{1, 5, 50, 80, 99, 100} {
		t.Run(fmt.Sprintf("pct%d", pct), func(t *testing.T) {
			want := scaledByPct(50, pct) // max(1, 50*pct/100) — the one formula
			// Documented headroom: the header band never spends the last
			// RateLimitHeadroom slots of the reported window, at any share.
			headerWant := min(want, 50-RateLimitHeadroom)

			cold := newRateTestAccount("u", cfg)
			cold.SetBudgetPct(pct)
			coldN := 0
			for i := 0; i < 60 && cold.AllowRequest("UserTweets"); i++ {
				coldN++ // seeded 50, no header window => the local counter gates
			}

			header := newRateTestAccount("u", cfg)
			header.SetBudgetPct(pct)
			header.recordWindow("UserTweets", rateLimitObs{
				limit: 50, remaining: 50, resetAt: time.Now().Add(15 * time.Minute)})
			headerN := 0
			for i := 0; i < 60 && header.AllowRequest("UserTweets"); i++ {
				headerN++
			}

			assert.Equal(t, want, coldN, "cold path must admit the scaled cap")
			assert.Equal(t, headerWant, headerN,
				"header gate must admit the scaled share, capped at limit-headroom")
		})
	}
}

// The never-set pct is the full budget, but the header band still keeps
// RateLimitHeadroom slots for requests already in flight: X's remaining does
// not count them, so letting a concurrent burst spend the whole reported
// window overshoots into a 429 — the pre-PR gate held at
// remaining <= RateLimitHeadroom and the zero value must keep that exactly.
// Reported remaining 3: two concurrent picks admit only one.
//
// Mutation: account.go:346 — drop the max(RateLimitHeadroom, ...) clamp
// (return limit-scaledByPct(limit, a.budgetPct)) => RED (both picks pass).
func TestBudgetPct_DefaultPctKeepsHeaderHeadroom(t *testing.T) {
	acc := newRateTestAccount("u", ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute})
	acc.recordWindow("UserTweets", rateLimitObs{limit: 50, remaining: 3, resetAt: time.Now().Add(15 * time.Minute)})

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if acc.AllowRequest("UserTweets") {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.EqualValues(t, 1, allowed.Load(),
		"remaining 3 with headroom 2 admits exactly one pick; the second must be refused")
	assert.True(t, acc.IsEndpointRateLimited("UserTweets"),
		"the estimate at the headroom holds the endpoint until reset")
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
// Mutation: account.go endpointGate — drop the writeScaledCapLocked call (or
// its rl.UpdateLimit) => RED (the post-update pick is still allowed by the
// stale scaled cap).
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

// HoldAccount is max-wins — a second call with an earlier `until` must not
// shorten a hold already in place — and ReleaseAccount is the explicit early
// lift.
//
// Mutation: account.go setHeldUntil — assign unconditionally
// (a.heldUntil = t) => RED (the earlier `until` shortens the hold).
func TestHoldAccount_MaxWinsAndRelease(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	f := newRLFakeX(t, func(string, int) rlWindow {
		return rlWindow{limit: 50, remaining: 40, reset: clk.Now().Add(15 * time.Minute)}
	})
	c, accs := newRLClient(t, 1, 50, clk)

	later := clk.Now().Add(60 * time.Second)
	require.True(t, c.HoldAccount("A", later))
	require.True(t, c.HoldAccount("A", clk.Now().Add(30*time.Second)),
		"a shorter re-hold must be accepted but must not shorten the existing one")
	assert.Equal(t, later.Unix(), accs[0].EndpointAvailableAt(rlEndpoint).Unix(),
		"the earlier `until` must not shorten the existing hold")

	// A later `until` extends; a re-hold after ReleaseAccount starts fresh.
	require.True(t, c.HoldAccount("A", clk.Now().Add(90*time.Second)))
	assert.Equal(t, clk.Now().Add(90*time.Second).Unix(), accs[0].EndpointAvailableAt(rlEndpoint).Unix())

	require.False(t, c.ReleaseAccount("ghost"), "no such account must return false")
	require.True(t, c.ReleaseAccount("A"), "the held account must release")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.rlCall(ctx, f), "the released account is pickable at once")
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

// OnRequest covers the account-pinned write path too: doPOST's attempts go
// through observedRequestWithBody — same observation point, same classifier.
//
// Mutation: client.go observedRequestWithBody — drop the emitRequestEvent
// call => RED (no event reaches the hook).
func TestOnRequest_POSTPathEmits(t *testing.T) {
	f := newRLFakeX(t, func(string, int) rlWindow {
		return rlWindow{limit: 50, remaining: 7, reset: time.Now().Add(15 * time.Minute)}
	})
	c, accs := newRLClient(t, 1, 50, nil)

	var events []RequestEvent
	c.cfg.OnRequest = func(ev RequestEvent) { events = append(events, ev) }

	_, err := c.doPOST(context.Background(), accs[0], "CreateTweet", f.url(), []byte(`{}`))
	require.NoError(t, err)

	require.Len(t, events, 1, "one POST attempt must emit exactly one event")
	assert.Equal(t, "A", events[0].Username)
	assert.Equal(t, "CreateTweet", events[0].Endpoint)
	assert.Equal(t, 200, events[0].Status)
	assert.Equal(t, outcomeOK, events[0].Outcome)
	assert.Equal(t, 7, events[0].RateLimit.Remaining)
}

// A transport failure still emits — Status 0, outcome "transport", no
// headers — once per attempt across the retry loop.
//
// Mutation: client.go observedRequestWithBody — return before
// emitRequestEvent when err != nil => RED (no events).
func TestOnRequest_TransportErrorEmitsStatus0(t *testing.T) {
	c, accs := newRLClient(t, 1, 50, nil)

	var events []RequestEvent
	c.cfg.OnRequest = func(ev RequestEvent) { events = append(events, ev) }

	_, err := c.doPOST(context.Background(), accs[0], "CreateTweet",
		"http://127.0.0.1:1/x", []byte(`{}`)) // port 1: refused before any response
	require.Error(t, err)

	require.NotEmpty(t, events, "every failed attempt still emits an event")
	for i, ev := range events {
		assert.Equal(t, 0, ev.Status, "event %d: a transport failure carries Status 0", i)
		assert.Equal(t, outcomeTransport, ev.Outcome, "event %d", i)
		assert.Equal(t, -1, ev.RateLimit.Remaining, "event %d: no headers on a transport failure", i)
	}
}

// The guest-token fallback path calls observedRequest with a nil account: the
// hook only ever observes pooled accounts.
//
// Mutation: client.go emitRequestEvent — drop the `acc == nil` guard => RED.
func TestOnRequest_GuestAttemptEmitsNothing(t *testing.T) {
	f := newRLFakeX(t, func(string, int) rlWindow { return rlWindow{} })
	c, _ := newRLClient(t, 1, 50, nil)

	var events []RequestEvent
	c.cfg.OnRequest = func(ev RequestEvent) { events = append(events, ev) }

	_, _, _, err := c.observedRequest(context.Background(), nil, rlEndpoint, c.client, "GET", f.url(), map[string]string{})
	require.NoError(t, err)
	assert.Empty(t, events, "an attempt with no pooled account must emit no event")
}

// Media-upload attempts run through the same observed wrapper: an upload
// attempt emits an event under the "UploadMedia" endpoint label.
//
// Mutation: media.go doMediaRequest — call c.doPoolReq directly instead of
// c.observedMediaReq => RED (no event).
func TestOnRequest_MediaAttemptEmits(t *testing.T) {
	f := newRLFakeX(t, func(string, int) rlWindow {
		return rlWindow{limit: 50, remaining: 9, reset: time.Now().Add(15 * time.Minute)}
	})
	c, accs := newRLClient(t, 1, 50, nil)

	var events []RequestEvent
	c.cfg.OnRequest = func(ev RequestEvent) { events = append(events, ev) }

	_, err := c.doMediaRequest(context.Background(), accs[0], "POST",
		f.srv.URL+"/i/media/upload.json", []byte(`x`), "application/octet-stream")
	require.NoError(t, err)

	require.Len(t, events, 1, "one upload attempt must emit exactly one event")
	assert.Equal(t, "A", events[0].Username)
	assert.Equal(t, epUploadMedia, events[0].Endpoint)
	assert.Equal(t, 200, events[0].Status)
	assert.Equal(t, outcomeOK, events[0].Outcome)
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
