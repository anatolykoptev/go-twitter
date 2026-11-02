package twitter

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const guestEndpoint = "UserByRestId" // !requiresAuth: the cap-only pick path

func rlCfgForTest() ratelimit.Config {
	return ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute}
}

// paceFake answers every request with a healthy window, so the only thing
// gating the pick under test is the pick pace.
func paceFake(t *testing.T, clk *rlFakeClock) *rlFakeX {
	t.Helper()
	now := time.Now
	if clk != nil {
		now = clk.Now
	}
	return newRLFakeX(t, func(string, int) rlWindow {
		return rlWindow{limit: 50, remaining: 40, reset: now().Add(15 * time.Minute)}
	})
}

// guestCall issues a call on the guest-eligible (cap-only) pick path.
func (c *Client) guestCall(ctx context.Context, f *rlFakeX) error {
	_, _, err := c.doPoolRequest(ctx, "GET", guestEndpoint, f.srv.URL+"/i/api/graphql/x/"+guestEndpoint, nil)
	return err
}

// I1 (journey: warm-up account A is paced while the pool serves reads): through
// the production pool path with two accounts, the paced account is picked at
// most once per gap, the other serves the rest, and no call waits.
//
// Mutation: request.go doPoolRequest poolFilter, add `ok = true` after
// `rsv, ok := a.tryReserveInterval()` => RED
// (TestPickPace_PacedAccountPickedOncePerGap: tokA gets ~half the hits).
func TestPickPace_PacedAccountPickedOncePerGap(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	f := paceFake(t, clk)
	c, accs := newRLClient(t, 2, 50, clk)
	accs[0].SetPickPace(stealth.Jitter{Min: time.Hour, Max: 2 * time.Hour})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	for range 8 {
		require.NoError(t, c.rlCall(ctx, f))
	}
	assert.LessOrEqual(t, f.hitsFor("tokA"), 1, "paced account picked more than once inside its gap")
	assert.Equal(t, 8, f.hitsFor("tokA")+f.hitsFor("tokB"))
	assert.Less(t, time.Since(start), 5*time.Second, "calls must go to the other account, not wait")

	// Next gap: the paced account may be picked once more, no more.
	before := f.hitsFor("tokA")
	clk.Advance(2*time.Hour + time.Second)
	for range 8 {
		require.NoError(t, c.rlCall(ctx, f))
	}
	assert.LessOrEqual(t, f.hitsFor("tokA")-before, 1)
}

// I2: concurrent picks of one account with a pace never let two through inside
// the gap (run under -race).
//
// Mutation: account.go tryReserveInterval, unlock a.mu between the
// `now.Before(a.nextPick)` check and the stamp (re-lock for the stamp) => RED
// (TestPickPace_ConcurrentPicksPassOnce: more than one passes).
func TestPickPace_ConcurrentPicksPassOnce(t *testing.T) {
	const workers, rounds = 32, 200
	clk := &rlFakeClock{t: time.Now()}
	a := newRateTestAccount("A", rlCfgForTest())
	a.clock = clk.Now
	a.SetPickPace(stealth.Jitter{Min: time.Minute, Max: 2 * time.Minute})

	for r := range rounds {
		clk.Advance(3 * time.Minute) // a fresh gap each round
		var passed atomic.Int32
		var start, done sync.WaitGroup
		start.Add(1)
		for range workers {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				if _, ok := a.tryReserveInterval(); ok {
					passed.Add(1)
				}
			}()
		}
		start.Done()
		done.Wait()
		require.EqualValues(t, 1, passed.Load(), "round %d: exactly one concurrent pick may pass", r)
	}
}

// I3: a single account — the second call waits the drawn gap (not the full
// pool cap) and ends early when its context is cancelled.
//
// Mutation: account.go intervalRemaining, return poolWaitCap + time.Hour for a
// pending gap (or in request.go nextUsableAt drop the `rem > 0` hook) => RED
// (TestPickPace_SingleAccountWaitsGap: "second call must wait for the gap, not fail").
func TestPickPace_SingleAccountWaitsGap(t *testing.T) {
	// Longer than one request round trip, so the second pick really is early.
	pace := stealth.Jitter{Min: 2 * time.Second, Max: 3 * time.Second}
	f := paceFake(t, nil)
	c, accs := newRLClient(t, 1, 50, nil)
	accs[0].SetPickPace(pace)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	t0 := time.Now()
	require.NoError(t, c.rlCall(ctx, f))
	require.NoError(t, c.rlCall(ctx, f), "second call must wait for the gap, not fail")
	elapsed := time.Since(t0)

	assert.GreaterOrEqual(t, elapsed, pace.Min, "second pick must not land inside the gap")
	assert.Less(t, elapsed, 12*time.Second, "must wake at the gap, not sleep toward the pool cap")
	assert.Equal(t, 2, f.hitsFor("tokA"))
}

func TestPickPace_WaitEndsOnContextCancel(t *testing.T) {
	f := paceFake(t, nil)
	c, accs := newRLClient(t, 1, 50, nil)
	accs[0].SetPickPace(stealth.Jitter{Min: time.Minute, Max: 2 * time.Minute}) // inside poolWaitCap

	require.NoError(t, c.rlCall(context.Background(), f))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	err := c.rlCall(ctx, f)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "want ctx deadline error, got %v", err)
	assert.Less(t, time.Since(t0), 2*time.Second, "cancel must end the wait early")
	assert.Equal(t, 1, f.hitsFor("tokA"))
}

// I4: changing the pace at runtime applies to the next pick, and the zero
// Jitter releases the account.
//
// Mutation: account.go SetPickPace, drop the `a.nextPick = a.lastPick.Add(...)`
// redraw (and/or store only when the new Max is larger) => RED
// (TestPickPace_RuntimeChangeAppliesNextPick: "a lowered pace must apply to the next pick").
func TestPickPace_RuntimeChangeAppliesNextPick(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	f := paceFake(t, clk)
	c, accs := newRLClient(t, 1, 50, clk)
	a := accs[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a.SetPickPace(stealth.Jitter{Min: time.Hour, Max: 2 * time.Hour})
	require.NoError(t, c.rlCall(ctx, f)) // stamps the pick
	clk.Advance(time.Minute)
	a.SetPickPace(stealth.Jitter{Min: 10 * time.Second, Max: 20 * time.Second}) // lowered: a minute ago is long enough
	require.NoError(t, c.rlCall(ctx, f), "a lowered pace must apply to the next pick")

	a.SetPickPace(stealth.Jitter{Min: time.Hour, Max: 2 * time.Hour})
	err := c.rlCall(ctx, f)
	require.Error(t, err, "a raised pace must apply to the next pick")
	assert.Contains(t, err.Error(), "pool exhausted")

	a.SetPickPace(stealth.Jitter{})
	require.NoError(t, c.rlCall(ctx, f), "the zero Jitter must release the account")
	assert.Equal(t, stealth.Jitter{}, a.PickPace())
	assert.Equal(t, 3, f.hitsFor("tokA"))

	a.SetPickPace(stealth.Jitter{Min: -time.Second, Max: -time.Second})
	assert.Equal(t, stealth.Jitter{}, a.PickPace(), "a negative range means off")
}

// A degenerate range is a FIXED gap of Min, not "no spacing" (Jitter.Duration
// returns 0 for Max <= Min).
//
// Mutation: account.go pickGap, return j.Duration() without the Max <= Min
// branch => RED (TestPickPace_DegenerateRangeIsFixedGap: second pick passes at once).
func TestPickPace_DegenerateRangeIsFixedGap(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	a := newRateTestAccount("A", rlCfgForTest())
	a.clock = clk.Now
	a.SetPickPace(stealth.Jitter{Min: 3 * time.Second, Max: 3 * time.Second})
	assert.Equal(t, stealth.Jitter{Min: 3 * time.Second, Max: 3 * time.Second}, a.PickPace())

	_, ok := a.tryReserveInterval()
	require.True(t, ok)
	assert.Equal(t, 3*time.Second, a.intervalRemaining())
	_, ok = a.tryReserveInterval()
	assert.False(t, ok, "a fixed 3s gap must refuse an immediate second pick")
	clk.Advance(2900 * time.Millisecond)
	_, ok = a.tryReserveInterval()
	assert.False(t, ok)
	clk.Advance(200 * time.Millisecond)
	_, ok = a.tryReserveInterval()
	assert.True(t, ok)

	// Max below Min is normalised to the same fixed gap.
	b := newRateTestAccount("B", rlCfgForTest())
	b.SetPickPace(stealth.Jitter{Min: 3 * time.Second, Max: time.Second})
	assert.Equal(t, stealth.Jitter{Min: 3 * time.Second, Max: 3 * time.Second}, b.PickPace())
}

// The drawn gaps vary and stay inside [Min, Max). Bounds are deterministic; the
// "varies" check needs 2000 uniform draws over a 3s range to all land within
// 1.5s of each other, a probability far below any flake budget.
//
// Mutation: account.go pickGap, return j.Min for every draw => RED
// (TestPickPace_GapsVaryWithinRange: "gaps must vary").
func TestPickPace_GapsVaryWithinRange(t *testing.T) {
	const lo, hi = time.Second, 4 * time.Second
	clk := &rlFakeClock{t: time.Now()}
	a := newRateTestAccount("A", rlCfgForTest())
	a.clock = clk.Now
	a.SetPickPace(stealth.Jitter{Min: lo, Max: hi})

	minGap, maxGap := time.Duration(1<<62), time.Duration(0)
	for range 2000 {
		_, ok := a.tryReserveInterval()
		require.True(t, ok)
		gap := a.intervalRemaining()
		require.GreaterOrEqual(t, gap, lo)
		require.Less(t, gap, hi)
		minGap, maxGap = min(minGap, gap), max(maxGap, gap)
		clk.Advance(hi) // the gap has certainly elapsed
	}
	assert.Greater(t, maxGap-minGap, 1500*time.Millisecond, "gaps must vary across the range")
}

// I5: an account refused for being too early spends no rate-limit budget.
//
// Mutation: request.go poolFilter, move the `rsv, ok := a.tryReserveInterval()`
// block after the `a.AllowRequest(endpoint)` call (and drop the unreserve) =>
// RED (TestPickPace_RefusedPickSpendsNoBudget: remaining falls below 40).
func TestPickPace_RefusedPickSpendsNoBudget(t *testing.T) {
	f := paceFake(t, nil)
	c, accs := newRLClient(t, 1, 50, nil)
	a := accs[0]
	a.SetPickPace(stealth.Jitter{Min: time.Minute, Max: 2 * time.Minute})
	require.NoError(t, c.rlCall(context.Background(), f))

	remaining := func() int {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.rlState[rlEndpoint].remaining
	}
	require.Equal(t, 40, remaining(), "response window seeds the header-authority state")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.Error(t, c.rlCall(ctx, f)) // refused picks only: the wait ends on ctx
	assert.Equal(t, 40, remaining(), "refused picks must not spend window budget")
	assert.Equal(t, 1, f.hitsFor("tokA"))
}

// A pick refused by AllowRequest (endpoint budget) after the pace passed gives
// the stamp back through the production poolFilter: the next pick is not
// delayed by a pick that never happened.
//
// Mutation: request.go poolFilter, replace `a.unreserveInterval(rsv)` with
// `_ = rsv` => RED (TestPickPace_BudgetRefusalGivesStampBack: the call waits
// out the unreturned gap and fails on the context deadline).
func TestPickPace_BudgetRefusalGivesStampBack(t *testing.T) {
	f := paceFake(t, nil)
	c, accs := newRLClient(t, 1, 50, nil)
	a := accs[0]
	a.SetPickPace(stealth.Jitter{Min: time.Minute, Max: 2 * time.Minute})
	a.MarkEndpointRateLimited(rlEndpoint, time.Now().Add(300*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, c.rlCall(ctx, f), "the pace was not consumed by the budget-refused picks")
	assert.Equal(t, 1, f.hitsFor("tokA"))
}

// Account-level twin of the above, plus the off state.
func TestPickPace_UnreserveRestoresAndOffIsFree(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	a := newRateTestAccount("A", rlCfgForTest())
	a.clock = clk.Now
	a.SetPickPace(stealth.Jitter{Min: time.Hour, Max: 2 * time.Hour})

	rsv, ok := a.tryReserveInterval()
	require.True(t, ok)
	_, ok = a.tryReserveInterval()
	require.False(t, ok)
	a.unreserveInterval(rsv)
	assert.Zero(t, a.intervalRemaining(), "undone pick leaves no gap")
	_, ok = a.tryReserveInterval()
	assert.True(t, ok)

	off := newRateTestAccount("B", rlCfgForTest())
	_, ok = off.tryReserveInterval()
	assert.True(t, ok)
	assert.True(t, off.lastPick.IsZero() && off.nextPick.IsZero(), "off: nothing stamped")
	assert.Zero(t, off.intervalRemaining())
}

// Guest-eligible (cap-only) path, single account in its gap, guest fallback
// off: the second call waits the gap and is served by the account.
//
// Mutation: request.go doPoolRequest, drop `|| c.anyCapWaitable(paceWaitable)`
// => RED (TestPickPace_GuestEligiblePathWaitsOutGap: "pool exhausted").
func TestPickPace_GuestEligiblePathWaitsOutGap(t *testing.T) {
	pace := stealth.Jitter{Min: 2 * time.Second, Max: 3 * time.Second}
	f := paceFake(t, nil)
	c, accs := newRLClient(t, 1, 50, nil)
	accs[0].SetPickPace(pace)
	require.True(t, c.cfg.DisableGuestFallback)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	t0 := time.Now()
	require.NoError(t, c.guestCall(ctx, f))
	require.NoError(t, c.guestCall(ctx, f), "the second call must wait out the gap, not fail or fall to a guest token")
	assert.GreaterOrEqual(t, time.Since(t0), pace.Min)
	assert.Equal(t, 2, f.hitsFor("tokA"))
}

// rotateNow retry (endpoint fault) in a mixed pool: A faulted, B inside its
// gap — the call is served by B once its gap ends.
//
// Mutation: same line as above => RED
// (TestPickPace_EndpointFaultRetryWaitsForPacedAccount: "pool exhausted").
func TestPickPace_EndpointFaultRetryWaitsForPacedAccount(t *testing.T) {
	f := newRLFakeX(t, func(tok string, nth int) rlWindow {
		w := rlWindow{limit: 50, remaining: 40, reset: time.Now().Add(15 * time.Minute)}
		if tok == "tokA" {
			w.status = http.StatusNotFound // endpoint fault: A is excluded for the retry
		}
		return w
	})
	c, accs := newRLClient(t, 2, 50, nil)
	accs[1].SetPickPace(stealth.Jitter{Min: 2 * time.Second, Max: 3 * time.Second})
	_, ok := accs[1].tryReserveInterval() // B starts inside its gap
	require.True(t, ok)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, c.rlCall(ctx, f), "A faults, B must serve after its gap")
	assert.Equal(t, 1, f.hitsFor("tokA"))
	assert.Equal(t, 1, f.hitsFor("tokB"))
}

// The cap-only wait is for the pace ONLY: an endpoint-limited account on a
// guest-eligible path still does not wait.
//
// Mutation: request.go doPoolRequest paceWaitable, delete the
// `!a.IsEndpointRateLimited(endpoint) &&` line => RED
// (TestPickPace_GuestEligiblePathStillSkipsEndpointLimit: waits on the
// context instead of failing fast).
func TestPickPace_GuestEligiblePathStillSkipsEndpointLimit(t *testing.T) {
	f := paceFake(t, nil)
	c, accs := newRLClient(t, 1, 50, nil)
	a := accs[0]
	a.SetPickPace(stealth.Jitter{Min: time.Minute, Max: 2 * time.Minute})
	require.NoError(t, c.guestCall(context.Background(), f)) // stamps the pace
	a.MarkEndpointRateLimited(guestEndpoint, time.Now().Add(time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t0 := time.Now()
	err := c.guestCall(ctx, f)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pool exhausted", "an endpoint limit must fail fast on this path")
	assert.Less(t, time.Since(t0), 2*time.Second, "must not wait for an endpoint limit")
	assert.Equal(t, 1, f.hitsFor("tokA"))
}
