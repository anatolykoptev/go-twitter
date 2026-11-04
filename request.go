package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/internal/parse"
	"github.com/anatolykoptev/go-twitter/internal/telemetry"
)

const maxRetries = 3

// dumpDir, when set via TWITTER_DUMP_DIR, makes every successful X response
// land in <dir>/<endpoint>.json for upstream-shape forensics — the ground
// truth when a parse silently zeroes fields after a server-side migration.
var dumpDir = os.Getenv("TWITTER_DUMP_DIR")

func dumpBody(endpoint string, body []byte) {
	if dumpDir == "" {
		return
	}
	if err := os.MkdirAll(dumpDir, 0o755); err != nil {
		return
	}
	// Per-endpoint single file, last-writer-wins: the goal is a current
	// specimen, not a log.
	_ = os.WriteFile(filepath.Join(dumpDir, endpoint+".json"), body, 0o600)
}

// doGET executes a GET request with multi-account retry, ct0 rotation, relogin,
// and guest-token fallback.
func (c *Client) doGET(ctx context.Context, endpoint, url string) ([]byte, map[string]string, error) {
	return c.doPoolRequest(ctx, "GET", endpoint, url, nil)
}

// doPoolPOST executes a POST request with the same pool-rotation, retry, and
// fallback logic as doGET. The payload is sent as the request body.
func (c *Client) doPoolPOST(ctx context.Context, endpoint, url string, payload []byte) ([]byte, map[string]string, error) {
	return c.doPoolRequest(ctx, "POST", endpoint, url, payload)
}

// poolWaitCap bounds the total time ONE request (all its retry attempts
// together) spends waiting for a pool account to become usable for its endpoint.
// A var only so tests can shrink it.
var poolWaitCap = 5 * time.Minute

// poolWakeJitterMax bounds the random delay added to a pool wait before
// retrying selection, so N waiters parked on one freed account do not all
// re-select at the same instant. 250 ms spreads the herd while staying far
// under the per-account pace (~1 s) and any realistic endpoint-limit wait.
const poolWakeJitterMax = 250 * time.Millisecond

// poolWakeJitter samples the actual per-wait delay in [0, poolWakeJitterMax).
// A var (like poolWaitCap) only so tests can lock it.
var poolWakeJitter = func() time.Duration { return rand.N(poolWakeJitterMax) }

// flightCapPoll bounds one cap-wait sleep when there is no timed wake to
// honour: a release landing between the failed pick and the channel grab is
// caught by the broadcast generation, and the poll re-checks regardless, so
// the wait can never outlive this bound even if a wake were somehow lost.
const flightCapPoll = 500 * time.Millisecond

// nextAccountWithWait selects an account for an auth-only endpoint, waiting
// (context-aware, never past deadline) when every account is unusable for now.
// The caller computes deadline once per request, so retries share one budget.
//
// go-stealth's NextWithWait only waits for deactivated accounts: its AvailableIn
// returns 0 as soon as any account is active, so a pool whose accounts are all
// active but rate-limited on this endpoint fails at once. After NextWithWait
// gives up, this waits until the earliest moment an account could serve the
// endpoint (see nextUsableAt) and retries. With no active account the original
// error is returned unchanged.
//
// capWaitable marks accounts that are blocked ONLY by their in-flight cap for
// this call (not faulted, not endpoint-limited, not proxy-backed-off): when
// no timed wake applies they have no usable-at time, so the wait listens on
// the slot-release broadcast instead, still bounded by deadline and ctx.
//
// capOnly restricts the wait to the cap case: no wait for deactivated accounts
// and no timed wake on rate limits or proxy backoff — only cap-blocked accounts
// are waited on. The endpoint-fault retry and the guest-eligible endpoints use
// it: they want "an account usable NOW, or a slot to free", never a long sleep.
// The one timed wake they keep is for paceWaitable accounts — blocked only by
// their pick pace (Account.SetPickPace) — which wait for exactly the end of the
// drawn gap, ctx- and deadline-bounded.
func (c *Client) nextAccountWithWait(ctx context.Context, endpoint string, filter func(*Account) bool, capWaitable, paceWaitable func(*Account) bool, capOnly bool, deadline time.Time) (acc *Account, err error) {
	// twitter.client.pool.wait.duration: one sample per call that actually
	// waited (slept for a usable account), labelled by how the wait ended.
	var waited time.Duration
	defer func() {
		if waited <= 0 {
			return
		}
		result := telemetry.WaitResultAcquired
		if err != nil {
			if ctx.Err() != nil {
				result = telemetry.WaitResultCtxDone
			} else {
				result = telemetry.WaitResultGaveUp
			}
		}
		c.metrics.recordPoolWait(endpoint, waited, result)
	}()
	for {
		// Grab the release generation BEFORE the pick: a slot freed between a
		// failed pick and the sleep then closes exactly this channel and wakes
		// the wait instead of being missed.
		flight := c.flightChan()
		if capOnly {
			acc, err = c.pool.Next(filter)
		} else {
			acc, err = c.pool.NextWithWait(ctx, filter, time.Until(deadline))
		}
		if err == nil {
			return acc, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var wait time.Duration
		timed := false
		if !capOnly {
			wake, ok := c.nextUsableAt(endpoint)
			wait = time.Until(wake)
			timed = ok && wait > 0 && wait <= time.Until(deadline)
		} else if rem, ok := c.nextPaceWait(paceWaitable); ok {
			// Cap-only picks never sleep for an endpoint limit or a hold, but
			// an account refused ONLY by its pick pace (Account.SetPickPace)
			// has a short, exact wake: wait it out instead of failing or
			// falling to the guest token.
			wait = rem
			timed = wait <= time.Until(deadline)
		}
		// With no timed wake to honour, an account blocked only by its
		// in-flight cap can still be served the moment a slot frees.
		capWait := c.anyCapWaitable(capWaitable)
		if !timed {
			if !capWait {
				// The failed pick can be microseconds stale: a hold or limit
				// that lifted between it and now leaves no future wake to
				// honour, so before giving up take one fresh pick — nearly
				// free on the give-up path and current by definition. Keep the
				// original error when it finds nothing either.
				if fresh, ferr := c.pool.Next(filter); ferr == nil {
					return fresh, nil
				}
				return nil, err
			}
			wait = flightCapPoll
		} else if capWait && wait > flightCapPoll {
			// A cap-blocked account may also be waiting: never sleep a long
			// timed wake past the poll bound, so a missed release is re-checked.
			wait = flightCapPoll
		}
		// Jitter the wake: N waiters parked on one freed account must not
		// re-select at the same instant. The jitter is added before the budget
		// clamp, so the sleep never extends past deadline (poolWaitCap); the
		// ctx select bounds it by the caller's context separately.
		wait += poolWakeJitter()
		if budget := time.Until(deadline); wait > budget {
			wait = budget
		}
		if wait <= 0 {
			return nil, err
		}
		slog.Debug("all active accounts limited, waiting",
			slog.String("endpoint", endpoint), slog.Duration("wait", wait), slog.Bool("cap_wait", capWait && !timed))
		start := time.Now()
		t := time.NewTimer(wait)
		// A freed in-flight slot also makes an account eligible — the
		// broadcast wakes the wait early instead of sleeping the full span.
		// Nil-channel receive blocks forever when no capped account exists.
		if !capWait {
			flight = nil
		}
		select {
		case <-t.C:
			waited += time.Since(start)
		case <-flight:
			t.Stop()
			waited += time.Since(start)
		case <-ctx.Done():
			t.Stop()
			waited += time.Since(start)
			return nil, ctx.Err()
		}
	}
}

// nextPaceWait returns the shortest remaining pick-pace gap among the ACTIVE
// accounts that paceWaitable accepts (blocked only by the pace). ok is false
// when no such account has a gap left.
func (c *Client) nextPaceWait(paceWaitable func(*Account) bool) (wait time.Duration, ok bool) {
	if paceWaitable == nil {
		return 0, false
	}
	for _, a := range c.pool.Items() {
		if !a.IsActive() || !paceWaitable(a) {
			continue
		}
		if rem := a.intervalRemaining(); rem > 0 && (!ok || rem < wait) {
			wait, ok = rem, true
		}
	}
	return wait, ok
}

// anyCapWaitable reports whether some ACTIVE account is blocked only by its
// in-flight cap for this call — it could serve the moment a slot frees, so a
// timed wake on rate limits/cooldowns is not the only way forward.
func (c *Client) anyCapWaitable(capWaitable func(*Account) bool) bool {
	if capWaitable == nil {
		return false
	}
	for _, a := range c.pool.Items() {
		if a.IsActive() && capWaitable(a) {
			return true
		}
	}
	return false
}

// nextUsableAt returns the earliest future moment at which some account may be
// usable for endpoint: an active account's endpoint limit or proxy backoff
// lifting, or a soft-deactivated account's cooldown ending. ok is false when no
// account is active (nothing to wait for beyond what NextWithWait already did)
// or no such moment lies in the future.
func (c *Client) nextUsableAt(endpoint string) (at time.Time, ok bool) {
	now := time.Now()
	anyActive := false
	var earliest time.Time
	consider := func(t time.Time) {
		if t.After(now) && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
		}
	}
	for _, acc := range c.pool.Items() {
		if !acc.IsActive() {
			consider(acc.ReactivateAt())
			continue
		}
		anyActive = true
		ready := acc.EndpointAvailableAt(endpoint)
		acc.mu.Lock()
		if acc.proxyBackoff.After(ready) {
			ready = acc.proxyBackoff
		}
		acc.mu.Unlock()
		if rem := acc.intervalRemaining(); rem > 0 {
			// Pick-pace wake: the waiter sleeps to exactly the end of the drawn
			// gap. rem is measured on the account clock, then placed on this
			// loop's clock.
			if iv := now.Add(rem); iv.After(ready) {
				ready = iv
			}
		}
		consider(ready)
	}
	return earliest, anyActive && !earliest.IsZero()
}

// doPoolRequest executes a pool-rotated request (GET or POST) with retry, ct0 rotation,
// relogin, and guest-token fallback.
func (c *Client) doPoolRequest(ctx context.Context, method, endpoint, url string, payload []byte) ([]byte, map[string]string, error) {
	ct := beginCall(ctx, endpoint) // nil unless the caller attached a RequestTrace
	// Pre-request jitter: off unless the client opted in with WithRequestJitter.
	if err := c.preRequestJitter(ctx); err != nil {
		return nil, nil, err
	}
	ct.jitterDone()

	// One wait budget for the whole request: nextAccountWithWait runs once per
	// attempt, so a per-call cap would allow maxRetries x poolWaitCap.
	waitDeadline := time.Now().Add(poolWaitCap)

	var lastErr error

	// Accounts that already produced an endpoint-fault class response this
	// call. An endpoint fault is a property of the committed request shape, not
	// the serving account: re-picking the same account is a guaranteed
	// duplicate request (on a one-account pool, an endless same-account retry
	// loop), so faulted accounts are filtered out — the next attempt serves a
	// DIFFERENT account or fails fast.
	endpointFaulted := make(map[string]bool)
	// prevEndpointFault records the endpoint-fault class the previous attempt
	// ended on (errNone = it was not an endpoint fault). An errEndpoint (404 /
	// GraphQL-shaped 400) is deterministic — the stealth backoff cannot change
	// the committed request shape, so it is skipped; errOverloaded keeps the
	// bounded backoff because X shedding load may recover.
	prevEndpointFault := errNone
	// prevAcc is the account the previous attempt was served by; prevProxyFault
	// records that it failed at its own per-account proxy (see skipBackoff).
	var prevAcc *Account
	prevProxyFault := false

	// poolFilter is the eligibility check every pick this call shares:
	// faulted accounts excluded, account usable for this endpoint NOW. It is
	// consuming — tryAcquireFlight takes the in-flight slot and AllowRequest
	// increments the per-endpoint counter — so it must only run on a real pick.
	flightLimit := c.flightCap()
	// paceHint is the upper bound of one pacer gap. It is 0 (feature off) when
	// the client has no account pacer; otherwise accounts the pick reserved
	// within the last paceHint are "pace-busy": the pacer may still hold them,
	// so a request prefers an account that is certainly past its gap.
	var paceHint time.Duration
	if c.accountPacer != nil {
		paceHint = c.cfg.AccountPaceMin + c.cfg.AccountPaceRandom
	}
	// newPoolFilter builds the eligibility check every pick this call shares:
	// faulted accounts excluded, account usable for this endpoint NOW. It is
	// consuming — tryAcquireFlight takes the in-flight slot and AllowRequest
	// increments the per-endpoint counter — so it must only run on a real pick.
	// requireReady additionally refuses a pace-busy account; that check is
	// read-only and runs before anything is consumed, so a refusal has nothing
	// to unwind. The preferred pass uses it; the pass that follows does not, so
	// a pool whose every account is inside its gap still picks and the
	// post-pick pacer waits (ctx-aware) instead of the call failing.
	newPoolFilter := func(requireReady bool) func(*Account) bool {
		return func(a *Account) bool {
			if endpointFaulted[a.ID()] || a.proxyBackedOff() || (requireReady && a.paceBusy()) || !a.tryAcquireFlight(flightLimit) {
				return false
			}
			// The minimum-interval check+stamp (Account.SetMinInterval) sits
			// BEFORE the consuming AllowRequest, so an account refused for being
			// too soon never spends rate-limit budget (#129); the in-flight slot
			// is returned on that refusal.
			rsv, ok := a.tryReserveInterval()
			if !ok {
				if flightLimit > 0 {
					a.releaseFlight()
				}
				return false
			}
			// The slot is taken BEFORE the consuming AllowRequest, so a refused
			// slot never burns a rate-limit slot; a refused AllowRequest returns
			// the slot it just took and the interval stamp (a pick that was not
			// made must not delay the next one).
			if !a.AllowRequest(endpoint) {
				a.unreserveInterval(rsv)
				if flightLimit > 0 {
					a.releaseFlight()
				}
				return false
			}
			// Every consuming step passed: the pick is made. Reserve the pace
			// window now (not after the pacer wait) so concurrent picks already
			// see this account as busy.
			if paceHint > 0 {
				a.reservePace(paceHint)
			}
			return true
		}
	}
	poolFilter := newPoolFilter(false)
	readyFilter := newPoolFilter(true)
	// preferReady takes an account that is certainly past its pace gap, or nil
	// when there is none (or pacing is off) and the regular pick must decide.
	preferReady := func() *Account {
		if paceHint <= 0 {
			return nil
		}
		if a, err := c.pool.Next(readyFilter); err == nil {
			return a
		}
		return nil
	}
	// canServe is the read-only twin of poolFilter for the backoff gate below:
	// IsEndpointRateLimited reads the same limiter without consuming a slot.
	canServe := func(a *Account) bool {
		return !endpointFaulted[a.ID()] &&
			!a.flightFull(flightLimit) &&
			!a.IsEndpointRateLimited(endpoint) &&
			!a.proxyBackedOff()
	}
	// capWaitable is the read-only twin for the wait decision: an account
	// blocked ONLY by its in-flight cap — it can serve the moment a slot
	// frees, so a cap-full pool waits on the release broadcast instead of
	// failing or waiting out a timed wake that cannot free a slot.
	capWaitable := func(a *Account) bool {
		return a.flightFull(flightLimit) &&
			!endpointFaulted[a.ID()] &&
			!a.IsEndpointRateLimited(endpoint) &&
			!a.proxyBackedOff()
	}

	// paceWaitable is the read-only twin for the cap-only paths: an account
	// whose ONLY blocker is its pick pace (not faulted, not endpoint-limited or
	// held, not proxy-backed-off). Those paths wait for it; they still never
	// wait for a limit or a hold.
	paceWaitable := func(a *Account) bool {
		return a.intervalRemaining() > 0 &&
			!endpointFaulted[a.ID()] &&
			!a.IsEndpointRateLimited(endpoint) &&
			!a.proxyBackedOff()
	}

	// heldAcc owns the in-flight slot consumed by the current pick. It is
	// released at the top of the next iteration (the previous attempt is done)
	// and by the deferred call on every return path, so a slot can never leak
	// past the attempt that took it.
	var heldAcc *Account
	release := func() {
		if heldAcc != nil {
			c.flightReleased(heldAcc)
			heldAcc = nil
		}
	}
	defer release()

	for attempt := range maxRetries {
		// The previous attempt ended before this iteration — return its slot
		// before the pick, so a capped pool sees the freed capacity.
		release()
		if ae := terminalVerdict(lastErr); ae != nil {
			return nil, nil, ae // a recovery retry answered terminally: do not rotate
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		// The backoff paces the next pick; with no account able to serve this
		// endpoint there is nothing to pace — an endpoint-fault pick fails
		// fast on pool.Next below and a normal pick enters nextAccountWithWait's
		// own bounded wait, so sleeping ~1 s first only delays a decided
		// outcome (e.g. an errOverloaded on a single-account pool).
		//
		// The sleep is also skipped when the previous failure was a verdict
		// about the serving account alone (accountScoped) and a DIFFERENT
		// account can serve now: that account is unaffected by the verdict, and
		// the per-account pacer still spaces it. X-side or shared failures keep
		// the backoff — pacing the next attempt is what protects everyone then.
		skipBackoff := false
		if attempt > 0 && prevAcc != nil && (prevProxyFault || accountScopedFailure(lastErr)) {
			skipBackoff = c.pool.Healthy(func(a *Account) bool { return a != prevAcc && canServe(a) }) > 0
		}
		if skipBackoff {
			slog.Debug("retry backoff skipped: previous failure was account-scoped and another account can serve",
				slog.String("endpoint", endpoint), slog.Int("attempt", attempt),
				slog.Int("class", int(failureClass(lastErr))), slog.Bool("proxy_fault", prevProxyFault))
		}
		if attempt > 0 && !skipBackoff && prevEndpointFault != errEndpoint && c.pool.Healthy(canServe) > 0 {
			delay := stealth.DefaultBackoff.Duration(attempt)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		// A retry after an endpoint fault takes only an account usable NOW:
		// waiting for a busy one just delays the same dead request shape.
		rotateNow := prevEndpointFault != errNone
		prevEndpointFault = errNone

		var acc *Account
		var accErr error

		if acc = preferReady(); acc != nil {
			// A pace-ready account was taken: no wait of any kind.
		} else if requiresAuth(endpoint) && !rotateNow {
			acc, accErr = c.nextAccountWithWait(ctx, endpoint, poolFilter, capWaitable, paceWaitable, false, waitDeadline)
		} else {
			acc, accErr = c.pool.Next(poolFilter)
			// Cap saturation is transient: an endpoint-fault retry and a
			// guest-eligible endpoint wait (cap-only, ctx- and budget-bounded)
			// for a slot instead of failing with "pool exhausted" or silently
			// downgrading to a guest token. Faulted accounts stay excluded.
			if accErr != nil && ctx.Err() == nil && (c.anyCapWaitable(capWaitable) || c.anyCapWaitable(paceWaitable)) {
				acc, accErr = c.nextAccountWithWait(ctx, endpoint, poolFilter, capWaitable, paceWaitable, true, waitDeadline)
			}
		}
		if accErr != nil {
			if ctx.Err() != nil {
				return nil, nil, accErr
			}
			// Keep the previous attempt's typed error reachable (IsBanned,
			// IsRateLimited, ...): the pool error alone says nothing about why
			// the pool ran dry.
			if lastErr != nil {
				accErr = fmt.Errorf("%w (previous attempt: %w)", accErr, lastErr)
			}
			lastErr = accErr
			break
		}
		prevAcc, prevProxyFault = acc, false
		heldAcc = acc // the pick consumed this account's in-flight slot inside the filter
		ct.picked(acc.Proxy != "")

		// Per-account human-pace spacing, applied AFTER the pool selects the
		// account and keyed by that account. Keying by account (not domain) means
		// a low-frequency caller is never starved by a high-frequency one — each
		// account self-paces its own rhythm. Independent of the opt-in pre-request
		// jitter above; well under the per-account rate ceiling so it never caps
		// throughput. nil pacer = pacing disabled.
		if c.accountPacer != nil {
			if err := c.accountPacer.Wait(ctx, acc.ID()); err != nil {
				return nil, nil, err
			}
			if paceHint > 0 {
				acc.reservePace(paceHint) // the grant may have come after a wait: restart the window from it
			}
		}
		ct.paced()

		// NO proactive ct0 rotation: an established session's ct0 is validated
		// server-side, so replacing it with a client-random value on a timer
		// guarantees CSRF-353 and the doomed relogin cascade (the live-reproduced
		// killer). ct0 is kept current by adopting the server's set-cookie ct0 on
		// each successful response below.
		bc, err := c.clientForAccount(acc)
		if err != nil {
			// The pool never selects such an account (IsActive); reaching here
			// means a racing reactivation. Never send it direct.
			lastErr = err
			continue
		}

		authTok, ct0, ua := acc.Credentials()
		body, respHdrs, status, err := c.observedPoolReq(ctx, acc, endpoint, bc, method, url, payload, twitterHeaders(authTok, ct0, ua))
		if err != nil {
			// A caller-side cancel/deadline is not evidence about the account
			// or its proxy: the caller revoked the work mid-flight, so nothing
			// is charged. Only the caller ctx's own Err() counts — an internal
			// transport timeout (go-stealth's per-request deadline) is a real
			// network failure and falls through to the normal attribution.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, nil, ctxErr
			}
			c.attributeTransportError(acc, err)
			prevProxyFault = acc.Proxy != "" && isProxyError(err)
			lastErr = err
			continue
		}

		// Reset proxy consecutive failures on any HTTP response — except a 5xx
		// delivered through the account's own proxy, which is itself proxy-layer
		// evidence (a dead residential proxy answers 502/503/504 for the tunnel).
		// That response feeds markProxyDown below instead, so the run must
		// accumulate, not reset.
		if !(status >= 500 && status < 600 && acc.Proxy != "") {
			acc.mu.Lock()
			acc.proxyConsecFails = 0
			acc.mu.Unlock()
		}

		// X's rate-limit headers are the authority: every primary response (200,
		// 429, 4xx) refines the per-endpoint cap, updates the account's window
		// state, feeds the ratelimit metrics, and rotates the endpoint away from
		// this account when the window is spent. Recovery sub-requests (ct0
		// rotation / relogin) are not re-observed; the next primary response is.
		// No-op for absent/malformed headers (the seeded default stands).
		c.observeRateLimit(acc, endpoint, respHdrs, true)

		// Handle HTTP status
		switch {
		case status == 429:
			c.recordAPICall(endpoint, false, true)
			acc.MarkEndpointRateLimited(endpoint, parseRateLimitReset(respHdrs["x-rate-limit-reset"]))
			lastErr = &APIError{Status: status, Class: errRateLimited, Message: "429 rate limited"}
			continue

		case status == 401 || status == 403:
			c.recordAPICall(endpoint, false, false)
			errClass := classifyError(body, respHdrs)
			switch errClass {
			case errCSRF:
				if body2, respHdrs2, ok := c.recoverCSRF(ctx, acc, bc, method, url, payload, endpoint, respHdrs, &lastErr); ok {
					return body2, respHdrs2, nil
				}
				continue
			case errAuthExpired:
				slog.Warn("auth expired (code 32), attempting relogin", slog.String("user", acc.Username))
				if reErr := c.relogin(ctx, acc); reErr != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return nil, nil, ctxErr // caller gone: the relogin verdict is not an account charge
					}
					slog.Warn("relogin failed", slog.String("user", acc.Username), slog.Any("error", reErr))
					c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
					lastErr = &APIError{Class: errAuthExpired, Err: reErr, Message: reErr.Error()}
					continue
				}
				authTok2, ct02, ua2 := acc.Credentials()
				body2, respHdrs2, status2, err2 := c.observedPoolReq(ctx, acc, endpoint, bc, method, url, payload, twitterHeaders(authTok2, ct02, ua2))
				if err2 == nil && status2 == 200 && usableResponse(body2, respHdrs2) {
					c.recordAPICall(endpoint, true, false)
					acc.RecordSuccess()
					return body2, respHdrs2, nil
				}
				if ctxErr := ctx.Err(); ctxErr != nil && err2 != nil {
					return nil, nil, ctxErr // post-relogin transport abort by caller cancel: no charge
				}
				lastErr = c.postReloginFailure(acc, endpoint, status2, body2, respHdrs2, err2, errAuthExpired, c.cfg.AuthCooldown, "post-relogin request failed")
				continue
			case errOverloaded:
				// LoadShed riding a 401/403 body is still X-side overload:
				// rotate to the next account, never charge this one.
				apiErr := newAPIError(status, body, respHdrs)
				apiErr.Message = fmt.Sprintf("%s HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
				endpointFaulted[acc.ID()] = true
				prevEndpointFault = apiErr.Class
				lastErr = apiErr
				continue
			default:
				acc.RecordFailure()
				apiErr := newAPIError(status, body, respHdrs)
				apiErr.Message = fmt.Sprintf("%s HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
				lastErr = apiErr
				continue
			}

		case status != 200:
			c.recordAPICall(endpoint, false, false)
			apiErr := newAPIError(status, body, respHdrs)
			apiErr.Message = fmt.Sprintf("%s HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
			if apiErr.Class == errBadRequest {
				// Deterministic caller error (#96): X rejected the INPUT —
				// another account would answer identically, so no retry, no
				// rotation, no charge, and no endpoint-fault bookkeeping (the
				// request shape was fine; the caller's payload was not).
				return nil, nil, apiErr
			}
			if apiErr.Class == errEndpoint || apiErr.Class == errOverloaded {
				// Endpoint-wide fault (#61): a 404/GraphQL-shaped-400 means the
				// committed request shape is stale for EVERY account (stale
				// queryId, missing xtid, feature mismatch); a 5xx/LoadShed means
				// X itself is shedding load. The serving account is innocent —
				// rotate to a DIFFERENT account inside the attempt budget, never
				// charge it. A 5xx delivered through the account's own proxy is
				// proxy-layer evidence (a dead residential proxy answers
				// 502/503/504 for the tunnel), so it feeds the proxy-failure
				// accounting and the proxy benches itself; a 5xx on a direct
				// connection is X-side and stays uncharged.
				if apiErr.Class == errOverloaded && acc.Proxy != "" {
					c.markProxyDown(acc)
				}
				endpointFaulted[acc.ID()] = true
				prevEndpointFault = apiErr.Class
				if apiErr.Class == errEndpoint {
					c.noteEndpointFault(endpoint, bc, authTok, ct0, ua) // K of these refresh the queryIds (#68)
				}
				slog.Warn("endpoint fault, rotating account",
					slog.String("endpoint", endpoint), slog.Int("status", status),
					slog.String("user", acc.Username))
				lastErr = apiErr
				continue
			}
			slog.Warn("doGET non-200", slog.String("endpoint", endpoint), slog.Int("status", status), slog.String("body", truncateBytes(body, 500)))
			if shouldDeactivate := acc.RecordFailure(); shouldDeactivate {
				total, failed, consec := acc.Stats()
				trip := acc.TripCount()
				slog.Warn("account unhealthy, soft-deactivating with backoff",
					slog.String("user", acc.Username),
					slog.Int("total", total),
					slog.Int("failed", failed),
					slog.Int("consec", consec),
					slog.Int("trip", trip))
				// Transient endpoint failures must NOT latch the account permanently:
				// use a growing-but-capped backoff so the pool self-heals once the
				// upstream recovers. Permanent removal is reserved for errSuspended.
				c.pool.SoftDeactivateBackoff(acc, c.nonResponsiveBackoff, trip)
			}
			return nil, nil, apiErr
		}

		// HTTP 200 — check for error codes in response body
		errClass := classifyError(body, respHdrs)

		// A 200 carrying errors AND usable data is a partial success (#61d):
		// X answered the query and degraded some fields. The bypass is
		// restricted to X-side classes — an account-dead verdict (326 locked,
		// 64 suspended, 88 banned/limited) riding alongside data must take its
		// class arm below, never count as a healthy success. A 200 with errors
		// and NO usable data still fails through the class switch below.
		if partialSuccessClass(errClass) && hasResponseData(body) {
			if newCT0 := extractCT0FromHeaders(respHdrs); newCT0 != "" && newCT0 != ct0 {
				acc.SetCT0(newCT0)
				authTok2, ct02, _ := acc.Credentials()
				c.saveSession(acc, authTok2, ct02)
			}
			c.recordAPICall(endpoint, true, false)
			acc.RecordSuccess()
			slog.Debug("errors with usable data, treating as success",
				slog.String("endpoint", endpoint), slog.Int("class", int(errClass)))
			dumpBody(endpoint, body)
			return body, respHdrs, nil
		}

		// An errors-only 200 — errors[] and no usable data — is a failed call
		// wearing a success status: classifyError leaves codes outside its
		// known list at errNone, so without this probe the body reached the
		// caller as an empty success and nobody ever learned the request
		// failed. The answer is terminal, NOT an endpoint fault: the errors[]
		// envelope proves the committed request shape EXECUTED, so it is not
		// stale and rotating another account can only reproduce the identical
		// body. A deterministic miss (deleted/unknown resource — see
		// errorsOnlyClass) returns ErrNotFound; any unrecognised answer
		// returns ErrUnexpectedResponse. Neither retries, rotates, falls back
		// to guest, relogs in, nor charges the account — and neither is fed
		// to endpointFaulted or noteEndpointFault.
		if errClass == errNone && errorsOnly(body) {
			c.recordAPICall(endpoint, false, false)
			cls := errorsOnlyClass(body)
			slog.Warn("errors-only 200, terminal answer",
				slog.String("endpoint", endpoint), slog.String("user", acc.Username),
				slog.Int("class", int(cls)))
			return nil, nil, &APIError{
				Status:  200,
				Codes:   extractErrorCodes(body),
				Class:   cls,
				Message: fmt.Sprintf("%s errors-only 200: %s", endpoint, truncateBytes(body, 200)),
			}
		}

		switch errClass {
		case errNone:
			if newCT0 := extractCT0FromHeaders(respHdrs); newCT0 != "" && newCT0 != ct0 {
				acc.SetCT0(newCT0)
				authTok2, ct02, _ := acc.Credentials()
				c.saveSession(acc, authTok2, ct02)
			}
			c.recordAPICall(endpoint, true, false)
			acc.RecordSuccess()
			dumpBody(endpoint, body)
			return body, respHdrs, nil

		case errCSRF:
			if body2, respHdrs2, ok := c.recoverCSRF(ctx, acc, bc, method, url, payload, endpoint, respHdrs, &lastErr); ok {
				return body2, respHdrs2, nil
			}
			continue

		case errAuthExpired:
			slog.Warn("auth expired (code 32), attempting relogin", slog.String("user", acc.Username))
			if reErr := c.relogin(ctx, acc); reErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, nil, ctxErr // caller gone: the relogin verdict is not an account charge
				}
				slog.Warn("relogin failed, soft-deactivating", slog.String("user", acc.Username), slog.Any("error", reErr))
				c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
				lastErr = &APIError{Class: errAuthExpired, Err: reErr, Message: reErr.Error()}
				continue
			}
			authTok2, ct02, ua2 := acc.Credentials()
			body2, respHdrs2, status2, err2 := c.observedPoolReq(ctx, acc, endpoint, bc, method, url, payload, twitterHeaders(authTok2, ct02, ua2))
			if err2 == nil && status2 == 200 && usableResponse(body2, respHdrs2) {
				c.recordAPICall(endpoint, true, false)
				acc.RecordSuccess()
				return body2, respHdrs2, nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil && err2 != nil {
				return nil, nil, ctxErr // post-relogin transport abort by caller cancel: no charge
			}
			lastErr = c.postReloginFailure(acc, endpoint, status2, body2, respHdrs2, err2, errAuthExpired, c.cfg.AuthCooldown, "post-relogin request failed")
			continue

		case errInternal:
			slog.Warn("error 131 without data, retrying", slog.String("user", acc.Username), slog.String("endpoint", endpoint))
			apiErr := newAPIError(200, body, respHdrs)
			apiErr.Message = "Twitter internal error (131)"
			lastErr = apiErr
			continue

		case errOverloaded:
			// LoadShed on a 200: X is shedding load — rotate to the next
			// account without charging this one.
			slog.Warn("X overloaded (LoadShed), rotating account",
				slog.String("user", acc.Username), slog.String("endpoint", endpoint))
			apiErr := newAPIError(200, body, respHdrs)
			apiErr.Message = "X overloaded (LoadShed)"
			endpointFaulted[acc.ID()] = true
			prevEndpointFault = apiErr.Class
			lastErr = apiErr
			continue

		case errRateLimited:
			// A run of consecutive header-less 88s is the signature of a ban
			// that omits the rate-limit headers, not of per-endpoint limiting:
			// escalate past the threshold instead of marking the endpoint again.
			if acc.noteRateLimited88(respHdrs) >= headerless88BanThreshold {
				lastErr = c.escalateHeaderless88(acc, endpoint, body)
				continue
			}
			// Code 88 inside a 200 body with an exhausted window: a plain
			// per-endpoint rate limit. The account is healthy, so block only this
			// endpoint until the reset instead of benching it for BanCooldown.
			c.recordAPICall(endpoint, false, true)
			acc.MarkEndpointRateLimited(endpoint, parseRateLimitReset(respHdrs[xRateLimitReset]))
			lastErr = &APIError{Status: 200, Class: errRateLimited, Codes: extractErrorCodes(body), Message: "rate limited (code 88)"}
			continue

		case errBanned:
			c.recordAPICall(endpoint, false, false)
			slog.Warn("account banned (code 88)", slog.String("user", acc.Username))
			c.pool.SoftDeactivate(acc, c.cfg.BanCooldown)
			lastErr = &APIError{Status: 200, Class: errBanned, Codes: extractErrorCodes(body), Message: "account banned"}
			continue

		case errSuspended:
			c.recordAPICall(endpoint, false, false)
			slog.Warn("account suspended (code 64), permanently deactivating", slog.String("user", acc.Username))
			c.pool.DeactivateItem(acc)
			lastErr = &APIError{Status: 200, Class: errSuspended, Codes: extractErrorCodes(body), Message: "account suspended"}
			continue

		case errLocked:
			c.recordAPICall(endpoint, false, false)
			slog.Warn("account locked (code 326, captcha needed)", slog.String("user", acc.Username))
			if c.cfg.CaptchaSolver != nil {
				slog.Info("attempting CAPTCHA unlock via relogin", slog.String("user", acc.Username))
				if reErr := c.relogin(ctx, acc); reErr != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return nil, nil, ctxErr // caller gone: the relogin verdict is not an account charge
					}
					slog.Warn("CAPTCHA unlock failed", slog.String("user", acc.Username), slog.Any("error", reErr))
				} else {
					authTok2, ct02, ua2 := acc.Credentials()
					body2, respHdrs2, status2, err2 := c.observedPoolReq(ctx, acc, endpoint, bc, method, url, payload, twitterHeaders(authTok2, ct02, ua2))
					if err2 == nil && status2 == 200 && usableResponse(body2, respHdrs2) {
						c.recordAPICall(endpoint, true, false)
						acc.RecordSuccess()
						slog.Info("CAPTCHA unlock succeeded", slog.String("user", acc.Username))
						return body2, respHdrs2, nil
					}
					if ctxErr := ctx.Err(); ctxErr != nil && err2 != nil {
						return nil, nil, ctxErr // caller cancel aborted the retry: no charge
					}
					slog.Warn("post-CAPTCHA request failed", slog.String("user", acc.Username))
					// The retry's own verdict decides the bench: a
					// still-locked account keeps errLocked/BanCooldown, a
					// suspended one is benched for good.
					lastErr = c.postReloginFailure(acc, endpoint, status2, body2, respHdrs2, err2, errLocked, c.cfg.BanCooldown, "post-CAPTCHA request failed")
					continue
				}
			}
			c.pool.SoftDeactivate(acc, c.cfg.BanCooldown)
			lastErr = &APIError{Status: 200, Class: errLocked, Codes: extractErrorCodes(body), Message: "account locked"}
			continue

		default: // errBlocked, errNotAuthorized
			c.recordAPICall(endpoint, false, false)
			slog.Warn("account error", slog.String("user", acc.Username), slog.Int("class", int(errClass)))
			c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
			apiErr := newAPIError(200, body, respHdrs)
			apiErr.Message = fmt.Sprintf("account error class %d", errClass)
			lastErr = apiErr
			continue
		}
	}

	// The last attempt is over: return its slot before the guest fallback, which
	// uses no account.
	release()
	if ae := terminalVerdict(lastErr); ae != nil {
		return nil, nil, ae // terminal answer from the last attempt: no guest fallback
	}
	// --- Guest token fallback ---
	if requiresAuth(endpoint) {
		if lastErr != nil {
			return nil, nil, fmt.Errorf("pool exhausted for %s (requires auth): %w", endpoint, lastErr)
		}
		return nil, nil, fmt.Errorf("%s requires authenticated account", endpoint)
	}

	// Global guest fallback kill-switch. In production, guest tokens from
	// datacenter IPs are unreliable (persistent 403 Bad guest token). Enabling
	// this flag forces all endpoints to require an authenticated account.
	if c.cfg.DisableGuestFallback {
		if lastErr != nil {
			return nil, nil, fmt.Errorf("pool exhausted for %s (guest fallback disabled): %w", endpoint, lastErr)
		}
		return nil, nil, fmt.Errorf("%s: no authenticated account and guest fallback disabled", endpoint)
	}

	gt, ok := c.getGuestTokenCached()
	if !ok {
		token, err := c.acquireGuestToken(ctx, c.client)
		if err != nil {
			if lastErr != nil {
				return nil, nil, fmt.Errorf("pool exhausted for %s: %w", endpoint, lastErr)
			}
			return nil, nil, fmt.Errorf("guest token unavailable for %s: %w", endpoint, err)
		}
		c.setGuestToken(token)
		gt = token
		slog.Info("guest token acquired as fallback", slog.String("endpoint", endpoint))
	}

	body, respHdrs, status, err := c.observedRequest(ctx, nil, endpoint, c.client, "GET", url, guestHeaders(gt))
	if err != nil {
		return nil, nil, err
	}
	if status == 429 {
		c.recordAPICall(endpoint, false, true)
		c.markGuestTokenRateLimited(parseRateLimitReset(respHdrs["x-rate-limit-reset"]))
		return nil, nil, &APIError{Status: status, Class: errRateLimited, Message: fmt.Sprintf("guest token rate-limited for %s", endpoint)}
	}
	if status == 401 || status == 403 {
		slog.Warn("guest token expired, reacquiring", slog.String("endpoint", endpoint), slog.Int("status", status))
		c.setGuestToken("")
		newGT, gtErr := c.acquireGuestToken(ctx, c.client)
		if gtErr != nil {
			c.recordAPICall(endpoint, false, false)
			return nil, nil, fmt.Errorf("guest token reacquisition failed for %s: %w", endpoint, gtErr)
		}
		c.setGuestToken(newGT)
		body, respHdrs, status, err = c.observedRequest(ctx, nil, endpoint, c.client, "GET", url, guestHeaders(newGT))
		if err != nil {
			return nil, nil, err
		}
		if status != 200 {
			c.recordAPICall(endpoint, false, false)
			apiErr := newAPIError(status, body, respHdrs)
			apiErr.Message = withAuthFailure(lastErr, fmt.Sprintf("%s (guest retry) HTTP %d: %s", endpoint, status, truncateBytes(body, 200)))
			return nil, nil, apiErr
		}
		if errorsOnly(body) {
			c.recordAPICall(endpoint, false, false)
			return nil, nil, &APIError{Status: 200, Codes: extractErrorCodes(body), Class: errorsOnlyClass(body),
				Message: withAuthFailure(lastErr, fmt.Sprintf("%s (guest retry) errors-only 200: %s", endpoint, truncateBytes(body, 200)))}
		}
		c.recordAPICall(endpoint, true, false)
		return body, respHdrs, nil
	}
	if status != 200 {
		c.recordAPICall(endpoint, false, false)
		apiErr := newAPIError(status, body, respHdrs)
		apiErr.Message = withAuthFailure(lastErr, fmt.Sprintf("%s (guest) HTTP %d: %s", endpoint, status, truncateBytes(body, 200)))
		return nil, nil, apiErr
	}
	if errorsOnly(body) {
		c.recordAPICall(endpoint, false, false)
		return nil, nil, &APIError{Status: 200, Codes: extractErrorCodes(body), Class: errorsOnlyClass(body),
			Message: withAuthFailure(lastErr, fmt.Sprintf("%s (guest) errors-only 200: %s", endpoint, truncateBytes(body, 200)))}
	}
	c.recordAPICall(endpoint, true, false)
	return body, respHdrs, nil
}

// withAuthFailure prefixes a guest-fallback failure message with the
// authenticated failure that triggered the fallback. Without it the caller only
// sees "(guest) HTTP 404" and the real cause (the logged-in request was
// rejected) is invisible. authErr already names the endpoint and status.
func withAuthFailure(authErr error, guestMsg string) string {
	if authErr == nil {
		return guestMsg
	}
	return fmt.Sprintf("authenticated request failed: %v; then %s", authErr, guestMsg)
}

// recoverCSRF handles a CSRF error 353 for a pool (GET/POST) request without ever
// generating a client-random ct0. It (1) adopts the SERVER ct0 from the 353
// response set-cookie when present and retries; (2) only if x.com offered no ct0
// to adopt, or the adopted ct0 still fails, falls through to relogin and retries
// with fresh credentials. Returns (body, hdrs, true) on a recovered success;
// (nil, nil, false) when recovery failed — the caller sets lastErr (via the
// pointer) and `continue`s to the next pool attempt.
//
// Rotating to a random ct0 on a 353 is proven futile against an established
// session (the live-reproduced killer), so it is never done here.
func (c *Client) recoverCSRF(ctx context.Context, acc *Account, bc *stealth.BrowserClient, method, url string, payload []byte, endpoint string, respHdrs map[string]string, lastErr *error) ([]byte, map[string]string, bool) {
	if serverCT0, ok := csrfRecoveryCT0(respHdrs); ok {
		slog.Warn("CSRF error 353, adopting server ct0", slog.String("user", acc.Username))
		acc.SetCT0(serverCT0)
		authTok2, ct02, ua2 := acc.Credentials()
		c.saveSession(acc, authTok2, ct02)
		body2, respHdrs2, status2, err2 := c.observedPoolReq(ctx, acc, endpoint, bc, method, url, payload, twitterHeaders(authTok2, ct02, ua2))
		if err2 == nil && status2 == 200 && usableResponse(body2, respHdrs2) {
			if newCT0 := extractCT0FromHeaders(respHdrs2); newCT0 != "" {
				acc.SetCT0(newCT0)
				authTok3, ct03, _ := acc.Credentials()
				c.saveSession(acc, authTok3, ct03)
			}
			c.recordAPICall(endpoint, true, false)
			acc.RecordSuccess()
			return body2, respHdrs2, true
		}
		if ctx.Err() != nil && err2 != nil {
			// The retry died to the caller's cancel/deadline, not to a real
			// failure: no RecordFailure, no charge, and no point relogging in.
			return nil, nil, false
		}
		acc.RecordFailure()
		slog.Warn("CSRF retry with server ct0 failed, attempting relogin", slog.String("user", acc.Username))
	} else {
		slog.Warn("CSRF error 353, no server ct0 to adopt, attempting relogin", slog.String("user", acc.Username))
	}

	if ctx.Err() != nil {
		return nil, nil, false // caller gone: no relogin, no charge on its way out
	}
	if reErr := c.relogin(ctx, acc); reErr != nil {
		if ctx.Err() != nil {
			return nil, nil, false // relogin aborted by the caller's own ctx
		}
		slog.Warn("relogin after CSRF failed", slog.String("user", acc.Username), slog.Any("error", reErr))
		c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
		*lastErr = &APIError{Class: errAuthExpired, Err: reErr, Message: reErr.Error()}
		return nil, nil, false
	}
	authTok3, ct03, ua3 := acc.Credentials()
	body3, respHdrs3, status3, err3 := c.observedPoolReq(ctx, acc, endpoint, bc, method, url, payload, twitterHeaders(authTok3, ct03, ua3))
	if err3 == nil && status3 == 200 && usableResponse(body3, respHdrs3) {
		c.recordAPICall(endpoint, true, false)
		acc.RecordSuccess()
		return body3, respHdrs3, true
	}
	if ctx.Err() != nil && err3 != nil {
		return nil, nil, false // post-relogin abort by caller cancel: no charge
	}
	*lastErr = c.postReloginFailure(acc, endpoint, status3, body3, respHdrs3, err3, errAuthExpired, c.cfg.AuthCooldown, "post-relogin CSRF request failed")
	return nil, nil, false
}

// recoverCSRFPost is the single-account (doPOST) twin of recoverCSRF. It adopts
// the SERVER ct0 from the 353 response and retries; only if x.com offered no ct0
// to adopt (or the adopted ct0 still fails) does it relogin and retry. It never
// generates a client-random ct0. Returns (body, true) on a recovered success;
// (nil, false) when recovery failed — the caller sets lastErr and `continue`s.
func (c *Client) recoverCSRFPost(ctx context.Context, acc *Account, bc *stealth.BrowserClient, url string, payload []byte, endpoint string, respHdrs map[string]string, lastErr *error) ([]byte, bool) {
	if serverCT0, ok := csrfRecoveryCT0(respHdrs); ok {
		slog.Warn("doPOST: CSRF error 353, adopting server ct0", slog.String("user", acc.Username))
		acc.SetCT0(serverCT0)
		authTok2, ct02, ua2 := acc.Credentials()
		c.saveSession(acc, authTok2, ct02)
		body2, respHdrs2, status2, err2 := c.observedRequestWithBody(ctx, acc, endpoint, bc, "POST", url, twitterHeaders(authTok2, ct02, ua2), bytes.NewReader(payload))
		if err2 == nil && (status2 == 200 || status2 == 201) && usableResponse(body2, respHdrs2) {
			if newCT0 := extractCT0FromHeaders(respHdrs2); newCT0 != "" {
				acc.SetCT0(newCT0)
				authTok3, ct03, _ := acc.Credentials()
				c.saveSession(acc, authTok3, ct03)
			}
			c.recordAPICall(endpoint, true, false)
			acc.RecordSuccess()
			return body2, true
		}
		if ctx.Err() != nil && err2 != nil {
			return nil, false // retry aborted by the caller's own ctx: no charge
		}
		acc.RecordFailure()
		slog.Warn("doPOST: CSRF retry with server ct0 failed, attempting relogin", slog.String("user", acc.Username))
	} else {
		slog.Warn("doPOST: CSRF error 353, no server ct0 to adopt, attempting relogin", slog.String("user", acc.Username))
	}

	if ctx.Err() != nil {
		return nil, false // caller gone: no relogin, no charge on its way out
	}
	if reErr := c.relogin(ctx, acc); reErr != nil {
		if ctx.Err() != nil {
			return nil, false // relogin aborted by the caller's own ctx
		}
		slog.Warn("doPOST: relogin after CSRF failed", slog.String("user", acc.Username), slog.Any("error", reErr))
		c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
		*lastErr = &APIError{Class: errAuthExpired, Err: reErr, Message: "relogin failed: " + reErr.Error()}
		return nil, false
	}
	authTok3, ct03, ua3 := acc.Credentials()
	body3, respHdrs3, status3, err3 := c.observedRequestWithBody(ctx, acc, endpoint, bc, "POST", url, twitterHeaders(authTok3, ct03, ua3), bytes.NewReader(payload))
	if err3 == nil && (status3 == 200 || status3 == 201) && usableResponse(body3, respHdrs3) {
		c.recordAPICall(endpoint, true, false)
		acc.RecordSuccess()
		return body3, true
	}
	if ctx.Err() != nil && err3 != nil {
		return nil, false // post-relogin abort by caller cancel: no charge
	}
	*lastErr = c.postReloginFailure(acc, endpoint, status3, body3, respHdrs3, err3, errAuthExpired, 0, "post-relogin CSRF request failed")
	return nil, false
}

// doPOST executes a POST mutation with a specific account.
// Unlike doGET, it does not rotate accounts from the pool — the caller provides the account.
// Handles CSRF rotation, auth expiry, and retries on transient errors.
func (c *Client) doPOST(ctx context.Context, acc *Account, endpoint, url string, payload []byte) ([]byte, error) {
	if err := stealth.DefaultJitter.Sleep(ctx); err != nil {
		return nil, err
	}
	// The pinned account owns the slot for the whole call: its retries do not
	// rotate, so there is nothing to re-acquire per attempt. The wait for a
	// free slot is bounded by the caller's ctx.
	if err := c.acquireFlight(ctx, acc); err != nil {
		return nil, err
	}
	defer c.flightReleased(acc)

	var lastErr error
	for attempt := range maxRetries {
		if ae := terminalVerdict(lastErr); ae != nil {
			return nil, ae // a recovery retry answered terminally: do not retry
		}
		if attempt > 0 {
			delay := stealth.DefaultBackoff.Duration(attempt)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// NO proactive ct0 rotation (see doPoolRequest):
		// client-rotating an established session's ct0 forces CSRF-353. ct0 stays
		// current via the server's set-cookie adopted on each success below.
		bc, err := c.clientForAccount(acc)
		if err != nil {
			return nil, err // pinned account: nothing to rotate to, fail closed
		}
		authTok, ct0, ua := acc.Credentials()
		body, respHdrs, status, err := c.observedRequestWithBody(ctx, acc, endpoint, bc, "POST", url, twitterHeaders(authTok, ct0, ua), bytes.NewReader(payload))
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr // caller cancel: not evidence about the account
			}
			c.attributeTransportError(acc, err)
			lastErr = err
			continue
		}

		// Reset proxy consecutive failures on any HTTP response
		acc.mu.Lock()
		acc.proxyConsecFails = 0
		acc.mu.Unlock()

		// Record the window X reports (see doPoolRequest). Writes are pinned to
		// the caller's account, so there is nothing to rotate to: state and
		// metrics only, no hold.
		c.observeRateLimit(acc, endpoint, respHdrs, false)

		switch {
		case status == 429:
			c.recordAPICall(endpoint, false, true)
			acc.MarkEndpointRateLimited(endpoint, parseRateLimitReset(respHdrs["x-rate-limit-reset"]))
			lastErr = &APIError{Status: status, Class: errRateLimited, Message: "429 rate limited"}
			continue

		case status == 401 || status == 403:
			c.recordAPICall(endpoint, false, false)
			errClass := classifyError(body, respHdrs)
			switch errClass {
			case errCSRF:
				if body2, ok := c.recoverCSRFPost(ctx, acc, bc, url, payload, endpoint, respHdrs, &lastErr); ok {
					return body2, nil
				}
				continue
			case errAuthExpired:
				slog.Warn("doPOST: auth expired, attempting relogin", slog.String("user", acc.Username))
				if reErr := c.relogin(ctx, acc); reErr != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return nil, ctxErr // caller gone: the relogin verdict is not an account charge
					}
					slog.Warn("doPOST: relogin failed, soft-deactivating", slog.String("user", acc.Username), slog.Any("error", reErr))
					c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
					lastErr = &APIError{Class: errAuthExpired, Err: reErr, Message: "relogin failed: " + reErr.Error()}
					continue
				}
				authTok2, ct02, ua2 := acc.Credentials()
				body2, respHdrs2, status2, err2 := c.observedRequestWithBody(ctx, acc, endpoint, bc, "POST", url, twitterHeaders(authTok2, ct02, ua2), bytes.NewReader(payload))
				if err2 == nil && (status2 == 200 || status2 == 201) && usableResponse(body2, respHdrs2) {
					c.recordAPICall(endpoint, true, false)
					acc.RecordSuccess()
					return body2, nil
				}
				if ctxErr := ctx.Err(); ctxErr != nil && err2 != nil {
					return nil, ctxErr // post-relogin abort by caller cancel: no charge
				}
				lastErr = c.postReloginFailure(acc, endpoint, status2, body2, respHdrs2, err2, errAuthExpired, 0, "post-relogin request failed")
				continue
			case errOverloaded:
				// LoadShed inside a 401/403 body: X-side, not the account's.
				apiErr := newAPIError(status, body, respHdrs)
				apiErr.Message = fmt.Sprintf("%s HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
				return nil, apiErr
			default:
				acc.RecordFailure()
				apiErr := newAPIError(status, body, respHdrs)
				apiErr.Message = fmt.Sprintf("%s HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
				return nil, apiErr
			}

		case status != 200:
			c.recordAPICall(endpoint, false, false)
			apiErr := newAPIError(status, body, respHdrs)
			apiErr.Message = fmt.Sprintf("%s HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
			// Endpoint faults (#61) and deterministic caller errors (#96) do
			// not charge the account — but a write is never replayed on another
			// account, so this returns immediately.
			if apiErr.Class != errEndpoint && apiErr.Class != errOverloaded && apiErr.Class != errBadRequest {
				acc.RecordFailure()
			}
			if apiErr.Class == errEndpoint {
				c.noteEndpointFault(endpoint, bc, authTok, ct0, ua) // K of these refresh the queryIds (#68)
			}
			return nil, apiErr
		}

		// HTTP 200 — check for error codes in response body
		errClass := classifyError(body, respHdrs)

		// A 200 carrying errors AND usable data is a partial success (#61d) —
		// the same X-side-classes-only rule as doPoolRequest: account-dead
		// verdicts (326, 64, 88) take their class arm below even with data.
		// Errors with no usable data still fail through the class switch.
		if partialSuccessClass(errClass) && hasResponseData(body) {
			if newCT0 := extractCT0FromHeaders(respHdrs); newCT0 != "" && newCT0 != ct0 {
				acc.SetCT0(newCT0)
				authTok2, ct02, _ := acc.Credentials()
				c.saveSession(acc, authTok2, ct02)
			}
			c.recordAPICall(endpoint, true, false)
			acc.RecordSuccess()
			slog.Debug("doPOST: errors with usable data, treating as success",
				slog.String("endpoint", endpoint), slog.Int("class", int(errClass)))
			return body, nil
		}

		// Same errors-only-200 guard as doPoolRequest: an errors[] envelope
		// with no usable data is a failed call, not a success — and must not
		// feed RecordSuccess below. Terminal: a deterministic miss returns
		// ErrNotFound, an unrecognised answer ErrUnexpectedResponse.
		if errClass == errNone && errorsOnly(body) {
			c.recordAPICall(endpoint, false, false)
			return nil, &APIError{Status: 200, Codes: extractErrorCodes(body), Class: errorsOnlyClass(body),
				Message: fmt.Sprintf("%s errors-only 200: %s", endpoint, truncateBytes(body, 200))}
		}

		switch errClass {
		case errNone:
			if newCT0 := extractCT0FromHeaders(respHdrs); newCT0 != "" && newCT0 != ct0 {
				acc.SetCT0(newCT0)
				authTok2, ct02, _ := acc.Credentials()
				c.saveSession(acc, authTok2, ct02)
			}
			c.recordAPICall(endpoint, true, false)
			acc.RecordSuccess()
			return body, nil
		case errCSRF:
			if body2, ok := c.recoverCSRFPost(ctx, acc, bc, url, payload, endpoint, respHdrs, &lastErr); ok {
				return body2, nil
			}
			continue
		case errRateLimited:
			// Same header-less-88 escalation as doPoolRequest.
			if acc.noteRateLimited88(respHdrs) >= headerless88BanThreshold {
				return nil, c.escalateHeaderless88(acc, endpoint, body)
			}
			// Code 88 with an exhausted window: the account is healthy, so do not
			// charge it a failure; block this endpoint until the reset.
			c.recordAPICall(endpoint, false, true)
			acc.MarkEndpointRateLimited(endpoint, parseRateLimitReset(respHdrs[xRateLimitReset]))
			apiErr := newAPIError(200, body, respHdrs)
			apiErr.Message = fmt.Sprintf("%s rate limited (code 88)", endpoint)
			return nil, apiErr
		case errInternal, errOverloaded:
			// X-side fault (131 internal, LoadShed) with no usable data: not
			// the account's — no failure charge, and a non-idempotent write is
			// never replayed on another account.
			c.recordAPICall(endpoint, false, false)
			apiErr := newAPIError(200, body, respHdrs)
			apiErr.Message = fmt.Sprintf("%s X-side error class %d: %s", endpoint, errClass, truncateBytes(body, 200))
			return nil, apiErr
		default:
			c.recordAPICall(endpoint, false, false)
			acc.RecordFailure()
			apiErr := newAPIError(200, body, respHdrs)
			apiErr.Message = fmt.Sprintf("%s error class %d: %s", endpoint, errClass, truncateBytes(body, 200))
			return nil, apiErr
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("%s failed after %d attempts: %w", endpoint, maxRetries, lastErr)
	}
	return nil, fmt.Errorf("%s failed after %d attempts", endpoint, maxRetries)
}

// requiresAuth returns true for endpoints that need a real authenticated account.
// UserByScreenName/UserTweets were added to prevent silent guest-token fallback,
// which is unreliable in production and hides authentication errors.
func requiresAuth(endpoint string) bool {
	switch endpoint {
	case "TweetDetail", "SearchTimeline", "Following", "Followers", "Retweeters",
		"CreateTweet", "UserByScreenName", "UserTweets",
		// T5 read cluster: all require a real account — no guest fallback
		// (the Mar-2026 lesson, plan §T5 / doc §8).
		"Bookmarks", "HomeTimeline", "HomeLatestTimeline",
		"ListLatestTweetsTimeline", "CommunityTweetsTimeline", "BlueVerifiedFollowers",
		// Device-notification feed: the legacy REST read and its GraphQL
		// fallback are both per-account feeds — a real account always.
		"DeviceFollow", "tweetNotificationsQuery",
		// T5.5 engagement mutations: writes always need a real account, never a
		// guest token. (ReplyTweet routes through the CreateTweet op, already above.)
		"FavoriteTweet", "UnfavoriteTweet", "CreateRetweet", "DeleteRetweet",
		// T5.6 DM cluster: inbox is per-account state and sending is a write —
		// both ALWAYS need a real authenticated account, no guest fallback.
		"DMInbox", "SendDM":
		return true
	}
	return false
}

// observeRateLimit applies one response's x-rate-limit-* headers: the adaptive
// per-endpoint cap, the account's window state (recordWindow), the
// ratelimit metrics, and, when hold is set, an endpoint block until the window
// resets if it is spent (holdIfExhausted). hold is false for writes pinned to a
// caller-chosen account.
func (c *Client) observeRateLimit(acc *Account, endpoint string, hdrs map[string]string, hold bool) {
	acc.SyncRateLimit(endpoint, hdrs)
	obs := parseRateLimitHeaders(hdrs)
	acc.recordWindow(endpoint, obs)
	c.metrics.recordRateLimit(endpoint, obs)
	if hold && acc.holdIfExhausted(endpoint) {
		c.metrics.recordHold(endpoint)
	}
}

// escalateHeaderless88 benches an account whose run of header-less code-88
// responses reached headerless88BanThreshold: a ban that omits the rate-limit
// headers. It mirrors the declared-ban (errBanned) arm — BanCooldown soft
// deactivation, non-rate-limited MetricsHook outcome, IsBanned-satisfying
// error — so the doPoolRequest, doPOST and doMediaRequest paths all treat the
// escalation identically. The count restarts at zero, so a reactivated account
// needs a full fresh run of header-less 88s before it is benched again.
func (c *Client) escalateHeaderless88(acc *Account, endpoint string, body []byte) *APIError {
	c.recordAPICall(endpoint, false, false)
	slog.Warn("account banned (repeated header-less code 88)", slog.String("user", acc.Username))
	c.pool.SoftDeactivate(acc, c.cfg.BanCooldown)
	acc.mu.Lock()
	acc.consecHeaderless88 = 0
	acc.mu.Unlock()
	return &APIError{Status: 200, Class: errBanned, Codes: extractErrorCodes(body), Message: "account banned (repeated header-less code 88)"}
}

// terminalVerdict returns the *APIError a recovery path left in lastErr when
// it is a terminal answer (a deterministic miss or an unrecognised errors-only
// body). The recovery paths (relogin / ct0 retries) record their verdict in
// lastErr and `continue`; without this check the loop would rotate to another
// account, or fall back to a guest token, to reproduce the identical answer.
func terminalVerdict(err error) *APIError {
	var ae *APIError
	if errors.As(err, &ae) && (ae.Class == errNotFound || ae.Class == errUnexpected) {
		return ae
	}
	return nil
}

// postReloginFailure classifies a failed post-recovery retry — a relogin or
// ct0-adoption retry that still did not produce a usable response — by the
// retry's own error codes, not by the failure that triggered the recovery
// (#26 / issue #34). A non-2xx retry is classified by its status first, as on
// the primary path (5xx overloaded, 404/410 endpoint), so its errors-only body
// cannot make it terminal. The fallback class applies only when the response
// carries no recognized class: a 2xx errors-only retry body is its own terminal verdict
// (a deterministic miss or an unrecognised answer via errorsOnlyClass —
// never an auth verdict, and never benched: the account just logged in and
// was answered for real), an unrecognized-code retry keeps the verdict the
// sites already reported, but a retry that answered suspended (64) or
// locked (326) must not masquerade as auth-expired and take a soft
// cooldown. The bench follows the classified verdict: suspended is benched
// for good, locked/banned get BanCooldown, a rate-limited retry only marks
// the endpoint window (the account is healthy — the same call the primary
// arm makes), and anything else gets fallbackCooldown — which the
// doPOST/recoverCSRFPost sites pass as 0 because they never charged a
// post-relogin failure at all.
func (c *Client) postReloginFailure(acc *Account, endpoint string, status int, body []byte, hdrs map[string]string, err error, fallback errorClass, fallbackCooldown time.Duration, msg string) *APIError {
	var cls errorClass
	switch {
	case status >= 200 && status < 300:
		cls = classifyError(body, hdrs)
		if cls == errNone && errorsOnly(body) {
			// An errors-only 2xx retry body is its own terminal verdict — a
			// deterministic miss or an answer the library does not recognise
			// — never the auth verdict that triggered the recovery. Neither
			// class benches: the freshly logged-in account proved healthy by
			// being answered for real.
			cls = errorsOnlyClass(body)
		}
	case status > 0:
		// A non-2xx retry keeps the status classes of the primary path (5xx
		// -> overloaded and retryable, 404/410 -> endpoint, 400+214 -> bad
		// request): the errors[] envelope on a non-2xx proves nothing about
		// the request shape having executed, so it must not turn a retryable
		// failure into a terminal verdict.
		cls = newAPIError(status, body, hdrs).Class
	default:
		cls = classifyError(body, hdrs) // transport error: no response to classify
	}
	if cls == errNone {
		cls = fallback
	}
	switch {
	case cls == errSuspended:
		c.pool.DeactivateItem(acc)
	case cls == errLocked || cls == errBanned:
		c.pool.SoftDeactivate(acc, c.cfg.BanCooldown)
	case cls == errRateLimited:
		acc.MarkEndpointRateLimited(endpoint, parseRateLimitReset(hdrs[xRateLimitReset]))
	case cls == errNotFound || cls == errUnexpected:
		// Terminal healthy-account verdicts: no bench, no charge.
	case cls == errEndpoint || cls == errOverloaded || cls == errBadRequest:
		// X-side or caller-side: the account just logged in is innocent.
	case fallbackCooldown > 0:
		c.pool.SoftDeactivate(acc, fallbackCooldown)
	}
	return &APIError{Status: status, Codes: extractErrorCodes(body), Class: cls, Err: err, Message: msg}
}

// quotedURLRe matches quoted HTTP(S) URLs embedded in error messages. Go's
// *url.Error wraps transport errors as `<op> "<url>": <inner>`; tls-client's
// CONNECT failure is a plain errorString whose prose embeds the request URL via
// the same wrapper. Stripping the quoted URL leaves only the transport's own
// message, so user-supplied content in the request URL (SearchTimeline rawQuery,
// UserByScreenName screenName) cannot false-positive the proxy signal.
var quotedURLRe = regexp.MustCompile(`"https?://[^"]*"`)

// isProxyError returns true if the error looks like a proxy connectivity failure.
//
// Matching is case-insensitive. The pre-fix lowercase-only predicate missed the
// single most common way an HTTP proxy dies: tls-client's CONNECT failure
// (connect.go:229/269) returns `errors.New("Proxy responded with non 200 code: "
// + resp.Status)` — "Proxy" capitalised twice, no lowercase "proxy" anywhere —
// so a 407 Proxy Authentication Required was charged to the account, not the
// proxy (5881 consecutive mis-attributed failures in production, see go-twitter
// #43).
//
// A structural classification (status-code field) is NOT reachable: tls-client
// returns a plain *errors.errorString with no typed wrapper, no Unwrap, and no
// extractable status code — the 407 lives only inside the prose. String matching
// is the only available signal. The case-insensitive widening is bounded by the
// call sites: isProxyError is only invoked on the transport-error branch (err !=
// nil) of doPoolReq/doRequestWithBody, where X-side HTTP errors (401/403/429)
// never arrive — those return err == nil and are handled by the status switch.
// The X-side counter-test (TestIsProxyError_XSideAuthErrorNotProxy) guards the
// false-positive direction regardless.
//
// The quoted request URL is stripped before matching so user-supplied content
// in the URL (rawQuery, screenName) cannot false-positive the proxy signal. A
// SearchTimeline for the query "proxy" or a UserByScreenName for handle
// "proxyhandle" embeds that word in the request URL; without stripping, a
// non-proxy transport error on such a request would be mis-attributed to the
// proxy — the same class of mis-attribution this branch exists to remove, in
// the other direction.
func isProxyError(err error) bool {
	if err == nil {
		return false
	}
	msg := quotedURLRe.ReplaceAllString(err.Error(), "")
	return containsFold(msg, "proxy") ||
		containsFold(msg, "SOCKS") ||
		containsFold(msg, "tunnel") ||
		containsFold(msg, "connection refused") ||
		containsFold(msg, "no such host")
}

// containsFold reports whether substr appears in s, case-insensitively. Used by
// isProxyError so a capitalised "Proxy" (tls-client's CONNECT-failure wording)
// matches the same as a lowercase "proxy" (net/http's proxyconnect wording).
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// attributeTransportError charges a transport-level error to the right layer:
// if the account uses a proxy and the error looks like a proxy connectivity
// failure, the proxy is marked down (exponential backoff, observable alert);
// otherwise the account is charged a failure. This is the single attribution
// seam for the triplicated transport-error branches in doPoolRequest, doPOST,
// and doMediaRequest — extracting it keeps the 407 fix in one place and makes
// the seam falsifiable.
func (c *Client) attributeTransportError(acc *Account, err error) {
	if acc.Proxy != "" && isProxyError(err) {
		c.markProxyDown(acc)
	} else {
		acc.RecordFailure()
	}
}

// accountScopedFailure reports whether err is a verdict about the serving
// account alone, so another account is unaffected by it: rate limit (429 /
// code 88), a banned, suspended or locked account, CSRF or expired auth, and
// the blocked / not-authorized classes (the loop benches the account for
// those). Everything else — overload (5xx, LoadShed), internal 131, endpoint
// faults, plain 403, unrecognised or non-API errors — says something about X
// or the shared request, not about the account. A per-account proxy failure is
// account-scoped too, but it is a transport error and tracked by the caller.
func accountScopedFailure(err error) bool {
	switch failureClass(err) {
	case errBanned, errRateLimited, errSuspended, errLocked, errCSRF, errAuthExpired, errBlocked, errNotAuthorized:
		return true
	}
	return false
}

// failureClass returns the error class of an *APIError in err's chain, errNone
// for anything else.
func failureClass(err error) errorClass {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class
	}
	return errNone
}

// markProxyDown applies exponential backoff for proxy failures and emits a
// "proxy.down" alert through the PoolAlertHook seam so a consumer can observe a
// dead proxy pool without grepping logs. Before this alert, a dead proxy could
// produce thousands of failures with no counter anyone could alert on (the
// go-twitter #43 field incident: 5881 consecutive failures, zero observable
// proxy-layer signal).
func (c *Client) markProxyDown(acc *Account) {
	// One critical section: the failure count and the deadline derived from it
	// change together, so no observer sees one without the other.
	acc.mu.Lock()
	acc.proxyConsecFails++
	fails := acc.proxyConsecFails
	duration := stealth.BackoffConfig{
		InitialWait: c.cfg.ProxyBackoffInitial,
		MaxWait:     c.cfg.ProxyBackoffMax,
		Multiplier:  2.0,
		JitterPct:   0.3,
	}.Duration(fails - 1)
	acc.proxyBackoff = time.Now().Add(duration)
	acc.mu.Unlock()

	slog.Warn("proxy down, backing off",
		slog.String("user", acc.Username),
		slog.String("proxy", stealth.MaskProxy(acc.Proxy)),
		slog.Int("consec_fails", fails),
		slog.Duration("backoff", duration))

	c.metrics.recordProxyFailure()

	if c.alertHook != nil {
		c.alertHook("proxy.down", map[string]any{
			"user":         acc.Username,
			"proxy":        stealth.MaskProxy(acc.Proxy),
			"consec_fails": fails,
			"backoff":      duration,
		})
	}
}

func truncateBytes(b []byte, n int) string { return parse.TruncateBytes(b, n) }

// hasResponseData returns true if the JSON body carries a "data" field with
// real content: not absent, not null, not an empty object/array, and not a
// container whose leaves are all null or empty — `{"data":{"user":null}}` is
// the "resource unavailable" shape, not usable data.
func hasResponseData(body []byte) bool {
	var probe struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &probe) != nil || len(probe.Data) == 0 {
		return false
	}
	var v any
	if json.Unmarshal(probe.Data, &v) != nil {
		return false
	}
	return jsonHasContent(v)
}

// jsonHasContent reports whether a decoded JSON value contains at least one
// non-null, non-empty leaf. Empty objects and arrays count as absent; any
// scalar (string, number, bool) is a leaf.
func jsonHasContent(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case map[string]any:
		for _, mv := range t {
			if jsonHasContent(mv) {
				return true
			}
		}
		return false
	case []any:
		for _, ev := range t {
			if jsonHasContent(ev) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// errorsOnly reports whether a body is an error envelope with no usable
// payload: a non-empty errors[] array and no data the caller could act on
// (hasResponseData semantics — absent, null, or all-empty leaves). It is the
// "failed call wearing a 200" shape: classifyError leaves codes outside its
// known list at errNone, so without this probe an errors-only body reaches
// the caller as an empty success.
func errorsOnly(body []byte) bool {
	return hasGraphQLErrors(body) && !hasResponseData(body)
}

// usableResponse reports whether a response body counts as a success at the
// request layer: a clean body (no error codes), or the partial-success shape
// — errors[] under an X-side-only class (partialSuccessClass) that still
// carries usable data. Every post-recovery retry gate uses it instead of a
// bare status check: a 200 whose errors[] carry an account verdict
// (suspended / locked / banned) or no usable data must not be returned as a
// success — the former marks a dead account healthy, the latter is the
// errors-only silent failure.
func usableResponse(body []byte, hdrs map[string]string) bool {
	cls := classifyError(body, hdrs)
	if cls == errNone {
		return !errorsOnly(body)
	}
	return partialSuccessClass(cls) && hasResponseData(body)
}

// addGraphQLParams builds the full URL with variables, features, and optional fieldToggles.
func addGraphQLParams(url string, variables, features map[string]any, fieldToggles ...map[string]any) string {
	v, _ := json.Marshal(variables)
	f, _ := json.Marshal(features)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	result := url + sep + "variables=" + jsonEscape(v) + "&features=" + jsonEscape(f)
	if len(fieldToggles) > 0 && fieldToggles[0] != nil {
		ft, _ := json.Marshal(fieldToggles[0])
		result += "&fieldToggles=" + jsonEscape(ft)
	}
	return result
}

// jsonEscape percent-encodes a JSON blob for use as a URL query value: every
// byte outside the RFC 3986 unreserved set [A-Za-z0-9-._~] is emitted as %XX.
// Encoding per byte (not per rune) encodes UTF-8 correctly, and covering the
// full unreserved set — not a hand-picked list — keeps reserved characters
// from corrupting the query string: a raw '#' would truncate the URL into a
// fragment, '+' would decode as a space, '%' would start a bogus escape, and
// a raw '"' would early-terminate the quoted-URL strip isProxyError relies
// on (#47). Space stays %20 (never '+'), and the hot-path inputs callers
// send today (numeric ids, feature-flag names) produce output byte-identical
// to the previous hand-rolled version.
func jsonEscape(b []byte) string {
	const hex = "0123456789ABCDEF"
	var result strings.Builder
	result.Grow(len(b) + len(b)/4)
	for _, c := range b {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			result.WriteByte(c)
			continue
		}
		result.WriteByte('%')
		result.WriteByte(hex[c>>4])
		result.WriteByte(hex[c&0x0F])
	}
	return result.String()
}
