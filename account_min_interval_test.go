package twitter

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minIntervalFake answers every request with a healthy window, so the only
// thing gating the pick under test is the minimum interval.
func minIntervalFake(t *testing.T, clk *rlFakeClock) *rlFakeX {
	t.Helper()
	now := time.Now
	if clk != nil {
		now = clk.Now
	}
	return newRLFakeX(t, func(string, int) rlWindow {
		return rlWindow{limit: 50, remaining: 40, reset: now().Add(15 * time.Minute)}
	})
}

// I1 (journey: warm-up account A is limited to one pick per interval while the
// pool serves reads): through the production pool path with two accounts, the
// limited account is picked at most once per interval, the other serves the
// rest, and no call waits.
//
// Mutation: request.go doPoolRequest poolFilter, delete the
// `rsv, ok := a.tryReserveInterval()` refusal (make ok always true) => RED
// (TestMinInterval_LimitedAccountPickedOncePerInterval: tokA gets ~half the hits).
func TestMinInterval_LimitedAccountPickedOncePerInterval(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	f := minIntervalFake(t, clk)
	c, accs := newRLClient(t, 2, 50, clk)
	accs[0].SetMinInterval(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	for range 8 {
		require.NoError(t, c.rlCall(ctx, f))
	}
	assert.LessOrEqual(t, f.hitsFor("tokA"), 1, "limited account picked more than once inside its interval")
	assert.Equal(t, 8, f.hitsFor("tokA")+f.hitsFor("tokB"))
	assert.Less(t, time.Since(start), 4*time.Second, "calls must go to the other account, not wait")

	// Next interval: the limited account may be picked once more, no more.
	before := f.hitsFor("tokA")
	clk.Advance(time.Hour + time.Second)
	for range 8 {
		require.NoError(t, c.rlCall(ctx, f))
	}
	assert.LessOrEqual(t, f.hitsFor("tokA")-before, 1)
}

// I2: concurrent picks of one account with an interval never let two through
// inside the interval (run under -race).
//
// Mutation: account.go tryReserveInterval, unlock a.mu between the
// `now.Before(a.lastPick.Add(d))` check and `a.lastPick = now` (re-lock for the
// stamp) => RED (TestMinInterval_ConcurrentPicksPassOnce: more than one passes).
func TestMinInterval_ConcurrentPicksPassOnce(t *testing.T) {
	const workers, rounds = 32, 200
	clk := &rlFakeClock{t: time.Now()}
	a := newRateTestAccount("A", rlCfgForTest())
	a.clock = clk.Now
	a.SetMinInterval(time.Minute)

	for r := range rounds {
		clk.Advance(2 * time.Minute) // a fresh interval each round
		var passed atomic.Int32
		var start sync.WaitGroup
		start.Add(1)
		var done sync.WaitGroup
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

// I3: a single account — the second call waits about the interval (not the full
// pool cap) and ends early when its context is cancelled.
//
// Mutation: account.go intervalReadyAt, return a.nowLocked().Add(poolWaitCap)
// instead of ready => RED (TestMinInterval_SingleAccountWaitsInterval: the wake
// lies beyond the wait budget, the call fails with "pool exhausted").
func TestMinInterval_SingleAccountWaitsInterval(t *testing.T) {
	// Longer than one request round trip, so the second pick really is early.
	const interval = 2 * time.Second
	f := minIntervalFake(t, nil)
	c, accs := newRLClient(t, 1, 50, nil)
	accs[0].SetMinInterval(interval)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	require.NoError(t, c.rlCall(ctx, f))
	require.NoError(t, c.rlCall(ctx, f), "second call must wait for the interval, not fail")
	elapsed := time.Since(t0)

	assert.GreaterOrEqual(t, elapsed, interval, "second pick must not land inside the interval")
	assert.Less(t, elapsed, 8*time.Second, "must wake at the interval, not sleep toward the pool cap")
	assert.Equal(t, 2, f.hitsFor("tokA"))
}

func TestMinInterval_WaitEndsOnContextCancel(t *testing.T) {
	f := minIntervalFake(t, nil)
	c, accs := newRLClient(t, 1, 50, nil)
	accs[0].SetMinInterval(time.Minute) // inside poolWaitCap, so the call waits

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

// I4: changing the interval at runtime applies to the next pick, and
// SetMinInterval(0) releases the account.
//
// Mutation: account.go SetMinInterval, store only when d > current
// (`if d > time.Duration(a.minIntervalNs.Load())`) => RED
// (TestMinInterval_RuntimeChangeAppliesNextPick: the lowered interval is ignored).
func TestMinInterval_RuntimeChangeAppliesNextPick(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	f := minIntervalFake(t, clk)
	c, accs := newRLClient(t, 1, 50, clk)
	a := accs[0]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a.SetMinInterval(time.Hour)
	require.NoError(t, c.rlCall(ctx, f)) // stamps the pick
	clk.Advance(time.Minute)
	a.SetMinInterval(30 * time.Second) // lowered: a minute ago is now long enough
	require.NoError(t, c.rlCall(ctx, f), "a lowered interval must apply to the next pick")

	a.SetMinInterval(time.Hour)
	err := c.rlCall(ctx, f)
	require.Error(t, err, "a raised interval must apply to the next pick")
	assert.Contains(t, err.Error(), "pool exhausted")

	a.SetMinInterval(0)
	require.NoError(t, c.rlCall(ctx, f), "0 must release the account")
	assert.Equal(t, time.Duration(0), a.MinInterval())
	a.SetMinInterval(-time.Second)
	assert.Equal(t, time.Duration(0), a.MinInterval(), "negative means off")
	assert.Equal(t, 3, f.hitsFor("tokA"))
}

// I5: an account refused for being too soon spends no rate-limit budget.
//
// Mutation: request.go poolFilter, move the `rsv, ok := a.tryReserveInterval()`
// block after the `a.AllowRequest(endpoint)` call (and drop the unreserve) =>
// RED (TestMinInterval_RefusedPickSpendsNoBudget: remaining falls below 40).
func TestMinInterval_RefusedPickSpendsNoBudget(t *testing.T) {
	f := minIntervalFake(t, nil)
	c, accs := newRLClient(t, 1, 50, nil)
	a := accs[0]
	a.SetMinInterval(time.Minute)
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

// A pick refused by AllowRequest (after the interval passed) gives the stamp
// back: a pick that never happened must not delay the next one.
func TestMinInterval_AllowRequestRefusalReturnsStamp(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	a := newRateTestAccount("A", rlCfgForTest())
	a.clock = clk.Now
	a.SetMinInterval(time.Hour)

	rsv, ok := a.tryReserveInterval()
	require.True(t, ok)
	_, ok = a.tryReserveInterval()
	require.False(t, ok)
	a.unreserveInterval(rsv)
	_, ok = a.tryReserveInterval()
	assert.True(t, ok, "stamp must be restored after an undone pick")
	assert.True(t, a.intervalReadyAt().After(clk.Now()))

	off := newRateTestAccount("B", rlCfgForTest())
	_, ok = off.tryReserveInterval()
	assert.True(t, ok)
	assert.True(t, off.lastPick.IsZero(), "off: nothing stamped")
	assert.True(t, off.intervalReadyAt().IsZero())
}

func rlCfgForTest() ratelimit.Config {
	return ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute}
}
