package twitter

import (
	"strconv"
	"time"
)

// RateLimitHeadroom is the number of requests X must still report as remaining
// for an endpoint to stay eligible. An endpoint whose window shows
// remaining <= RateLimitHeadroom is held back until the window resets. It is
// exported so consumers do not have to duplicate the constant to compute an
// account's effective budget (see Account.SetBudgetPct).
//
// Why 2 and not 0: a response's remaining counts requests X had served when it
// answered, not requests already in flight. With the 50-per-window caps X gives
// UserTweets/SearchTimeline, two spare slots absorb the handful of concurrent
// calls that share an account (4% of the window) so those calls do not land on
// a 429, which is what draws X's attention to an account. Why not nitter's 10:
// on a 50-request window that would waste a fifth of the budget.
const RateLimitHeadroom = 2

// maxRateLimitReset is the furthest into the future an x-rate-limit-reset is
// believed. X's windows are 15 minutes; 1h leaves 4x slack for clock skew and
// longer windows. A reset beyond it is a malformed value (a far-future sentinel,
// or milliseconds read as seconds) and is treated as unknown, because trusting it
// would hold an account for years, or make a bogus window "authoritative" and
// discard every genuine later one as "earlier".
const maxRateLimitReset = time.Hour

// resetJitterTolerance is how far two resets may differ and still name the same
// window. Real windows are 15 minutes apart, so 5s cannot merge two of them,
// while it absorbs the second-level wobble in X's reset between responses that
// would otherwise let a stale response look like a newer window and raise
// remaining.
const resetJitterTolerance = 5 * time.Second

// rateLimitObs is what ONE response said about its endpoint's window. Absent or
// unparseable headers leave the matching field at its "unknown" value.
type rateLimitObs struct {
	limit     int       // x-rate-limit-limit; 0 = unknown
	remaining int       // x-rate-limit-remaining; -1 = unknown
	resetAt   time.Time // x-rate-limit-reset (unix seconds); zero = unknown
}

// parseRateLimitHeaders reads the three x-rate-limit-* headers off a response.
func parseRateLimitHeaders(hdrs map[string]string) rateLimitObs {
	obs := rateLimitObs{remaining: -1}
	if n, ok := parseRateLimitLimit(hdrs[xRateLimitLimit]); ok {
		obs.limit = n
	}
	if n, ok := rateLimitRemaining(hdrs); ok && n >= 0 {
		obs.remaining = n
	}
	if ts, err := strconv.ParseInt(hdrs[xRateLimitReset], 10, 64); err == nil && ts > 0 {
		if at := time.Unix(ts, 0); at.Sub(time.Now()) <= maxRateLimitReset {
			obs.resetAt = at
		}
	}
	return obs
}

// rateLimitState is an account's last known window for one endpoint.
type rateLimitState struct {
	limit      int
	remaining  int
	resetAt    time.Time
	observedAt time.Time
}

// nowLocked reports the account's clock (tests inject a fake); a.mu must be held.
func (a *Account) nowLocked() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}

// recordWindow folds one response's rate-limit headers into this account's
// state for the endpoint. A response missing remaining or reset leaves the
// state untouched: without both it cannot be placed in a window.
//
// Race rule. Responses on one account can complete out of order, so an older
// response must never raise remaining. A window is identified by its reset
// timestamp (X reports the same reset for every response in a window), which
// makes arrival order irrelevant and needs no send timestamps:
//   - a reset later by more than resetJitterTolerance is a newer window:
//     replace the state outright;
//   - a reset within the tolerance is the same window (X's reset can wobble by
//     a second between responses), where remaining only ever falls: keep the
//     minimum, so a late, stale response cannot raise it, and the later reset;
//   - a reset earlier by more than the tolerance is an older window: ignore it.
//
// Ordering by send time instead would need a timestamp captured per request and
// still mis-orders when two requests are sent in the same instant.
func (a *Account) recordWindow(endpoint string, obs rateLimitObs) {
	if obs.remaining < 0 || obs.resetAt.IsZero() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rlState == nil {
		a.rlState = make(map[string]rateLimitState)
	}
	now := a.nowLocked()
	cur, ok := a.rlState[endpoint]
	switch {
	case !ok || obs.resetAt.After(cur.resetAt.Add(resetJitterTolerance)):
		a.rlState[endpoint] = rateLimitState{limit: obs.limit, remaining: obs.remaining, resetAt: obs.resetAt, observedAt: now}
	case !obs.resetAt.Before(cur.resetAt.Add(-resetJitterTolerance)):
		if obs.resetAt.After(cur.resetAt) {
			cur.resetAt = obs.resetAt
		}
		if obs.remaining < cur.remaining {
			cur.remaining = obs.remaining
		}
		if obs.limit > 0 {
			cur.limit = obs.limit
		}
		cur.observedAt = now
		a.rlState[endpoint] = cur
	}
}

// rateLimitWindow returns the last known window for the endpoint.
func (a *Account) rateLimitWindow(endpoint string) (limit, remaining int, resetAt time.Time, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.rlState[endpoint]
	return st.limit, st.remaining, st.resetAt, ok
}

// latestRateLimit returns the endpoint and window most recently observed on
// this account, preferring `preferred` when it has state.
func (a *Account) latestRateLimit(preferred string) (endpoint string, st rateLimitState, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, ok := a.rlState[preferred]; ok {
		return preferred, st, true
	}
	for ep, s := range a.rlState {
		if !ok || s.observedAt.After(st.observedAt) {
			endpoint, st, ok = ep, s, true
		}
	}
	return endpoint, st, ok
}

// holdIfExhausted reports whether the endpoint is now held on this account,
// persisting the hold when the response showed X's own exhaustion (the merged
// remaining estimate at or below RateLimitHeadroom). The response that
// reported it still succeeded; the NEXT pick for the endpoint rotates to
// another account.
//
// Two different bands share the gate and they persist differently:
//   - X's own exhaustion — a response showing remaining <= RateLimitHeadroom,
//     or an explicit mark on a 429 / code 88 — is written to blockedUntil and
//     stands until the window resets. It is a fact about X; no client-side
//     change releases it early.
//   - the wider budget-share band is DERIVED at read time (see
//     budgetFloorLocked/gateBlockedLocked) and never persisted, so a runtime
//     SetBudgetPct that raises the share releases a budget-only hold on the
//     next pick.
//
// Together with the block's expiry this is the eligibility rule "remaining >
// spent-floor || now >= resetAt": a window with budget left never blocks, and
// the block ends at resetAt on its own. While a window is live the endpoint
// also bypasses the local request counter (see AllowRequest): the headers are
// the authority and the counter is only the cold-start / no-headers estimate.
func (a *Account) holdIfExhausted(endpoint string) bool {
	a.mu.Lock()
	now := a.nowLocked()
	st, ok := a.rlState[endpoint]
	until, exhausted := a.holdLocked(endpoint, now)
	_, blocked := a.gateBlockedLocked(endpoint, st, ok, now)
	rl := a.rateLimiter
	a.mu.Unlock()
	if exhausted && rl != nil {
		rl.MarkRateLimited(endpoint, until)
	}
	return now.Before(blocked)
}

// holdLocked records the hold in blockedUntil when the endpoint's state shows
// X's own exhaustion — the remaining estimate at or below RateLimitHeadroom.
// Only X's word is persisted (this check, fed by responses, or an explicit
// MarkEndpointRateLimited on a 429 / code 88): the wider budget-share band is
// derived at read time by gateBlockedLocked, never persisted, so raising
// SetBudgetPct releases a budget-only hold while real exhaustion stands until
// the reset. The caller mirrors the persisted hold into the local limiter
// after unlocking. a.mu must be held.
func (a *Account) holdLocked(endpoint string, now time.Time) (until time.Time, held bool) {
	st, ok := a.rlState[endpoint]
	if !ok || st.remaining > RateLimitHeadroom || !st.resetAt.After(now) || !a.blockedUntil[endpoint].Before(st.resetAt) {
		return time.Time{}, false
	}
	if a.blockedUntil == nil {
		a.blockedUntil = make(map[string]time.Time)
	}
	a.blockedUntil[endpoint] = st.resetAt
	return st.resetAt, true
}
