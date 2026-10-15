package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
)

const maxRetries = 3

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
func (c *Client) nextAccountWithWait(ctx context.Context, endpoint string, filter func(*Account) bool, deadline time.Time) (acc *Account, err error) {
	// twitter.client.pool.wait.duration: one sample per call that actually
	// waited (slept for a usable account), labelled by how the wait ended.
	var waited time.Duration
	defer func() {
		if waited <= 0 {
			return
		}
		result := waitResultAcquired
		if err != nil {
			if ctx.Err() != nil {
				result = waitResultCtxDone
			} else {
				result = waitResultGaveUp
			}
		}
		c.metrics.recordPoolWait(endpoint, waited, result)
	}()
	for {
		acc, err = c.pool.NextWithWait(ctx, filter, time.Until(deadline))
		if err == nil {
			return acc, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		wake, ok := c.nextUsableAt(endpoint)
		if !ok {
			return nil, err
		}
		wait := time.Until(wake)
		if wait <= 0 || wait > time.Until(deadline) {
			return nil, err
		}
		// Jitter the wake: N waiters parked on one freed account must not
		// re-select at the same instant. The jitter is added before the budget
		// clamp, so the sleep never extends past deadline (poolWaitCap); the
		// ctx select bounds it by the caller's context separately.
		wait += poolWakeJitter()
		if budget := time.Until(deadline); wait > budget {
			wait = budget
		}
		slog.Debug("all active accounts limited, waiting",
			slog.String("endpoint", endpoint), slog.Duration("wait", wait))
		start := time.Now()
		t := time.NewTimer(wait)
		select {
		case <-t.C:
			waited += time.Since(start)
		case <-ctx.Done():
			t.Stop()
			waited += time.Since(start)
			return nil, ctx.Err()
		}
	}
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
		consider(ready)
	}
	return earliest, anyActive && !earliest.IsZero()
}

// doPoolRequest executes a pool-rotated request (GET or POST) with retry, ct0 rotation,
// relogin, and guest-token fallback.
func (c *Client) doPoolRequest(ctx context.Context, method, endpoint, url string, payload []byte) ([]byte, map[string]string, error) {
	// Anti-fingerprint jitter
	if err := stealth.DefaultJitter.Sleep(ctx); err != nil {
		return nil, nil, err
	}

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

	// poolFilter is the eligibility check every pick this call shares:
	// faulted accounts excluded, account usable for this endpoint NOW. It is
	// consuming — AllowRequest increments the per-endpoint counter — so it
	// must only run on a real pick.
	poolFilter := func(a *Account) bool {
		return !endpointFaulted[a.ID()] && a.AllowRequest(endpoint) && time.Now().After(a.proxyBackoff)
	}
	// canServe is the read-only twin of poolFilter for the backoff gate below:
	// IsEndpointRateLimited reads the same limiter without consuming a slot.
	canServe := func(a *Account) bool {
		return !endpointFaulted[a.ID()] && !a.IsEndpointRateLimited(endpoint) && time.Now().After(a.proxyBackoff)
	}

	for attempt := range maxRetries {
		// The backoff paces the next pick; with no account able to serve this
		// endpoint there is nothing to pace — an endpoint-fault pick fails
		// fast on pool.Next below and a normal pick enters nextAccountWithWait's
		// own bounded wait, so sleeping ~1 s first only delays a decided
		// outcome (e.g. an errOverloaded on a single-account pool).
		if attempt > 0 && prevEndpointFault != errEndpoint && c.pool.Healthy(canServe) > 0 {
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

		if requiresAuth(endpoint) && !rotateNow {
			acc, accErr = c.nextAccountWithWait(ctx, endpoint, poolFilter, waitDeadline)
		} else {
			acc, accErr = c.pool.Next(poolFilter)
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

		// Per-account human-pace spacing, applied AFTER the pool selects the
		// account and keyed by that account. Keying by account (not domain) means
		// a low-frequency caller is never starved by a high-frequency one — each
		// account self-paces its own rhythm. Additive to the anti-fingerprint
		// jitter above; well under the per-account rate ceiling so it never caps
		// throughput. nil pacer = pacing disabled.
		if c.accountPacer != nil {
			if err := c.accountPacer.Wait(ctx, acc.ID()); err != nil {
				return nil, nil, err
			}
		}

		// NO proactive ct0 rotation: an established session's ct0 is validated
		// server-side, so replacing it with a client-random value on a timer
		// guarantees CSRF-353 and the doomed relogin cascade (the live-reproduced
		// killer). ct0 is kept current by adopting the server's set-cookie ct0 on
		// each successful response below. See shouldProactivelyRotate (ct0.go).
		bc := c.clientForAccount(acc)

		authTok, ct0, ua := acc.Credentials()
		body, respHdrs, status, err := c.observedPoolReq(endpoint, bc, method, url, payload, twitterHeaders(authTok, ct0, ua))
		if err != nil {
			c.attributeTransportError(acc, err)
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
				if body2, respHdrs2, ok := c.recoverCSRF(acc, bc, method, url, payload, endpoint, respHdrs, &lastErr); ok {
					return body2, respHdrs2, nil
				}
				continue
			case errAuthExpired:
				slog.Warn("auth expired (code 32), attempting relogin", slog.String("user", acc.Username))
				if reErr := c.relogin(acc); reErr != nil {
					slog.Warn("relogin failed", slog.String("user", acc.Username), slog.Any("error", reErr))
					c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
					lastErr = &APIError{Class: errAuthExpired, Err: reErr, Message: reErr.Error()}
					continue
				}
				authTok2, ct02, ua2 := acc.Credentials()
				body2, respHdrs2, status2, err2 := c.observedPoolReq(endpoint, bc, method, url, payload, twitterHeaders(authTok2, ct02, ua2))
				if err2 == nil && status2 == 200 {
					c.recordAPICall(endpoint, true, false)
					acc.RecordSuccess()
					return body2, respHdrs2, nil
				}
				c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
				lastErr = &APIError{Status: status2, Codes: extractErrorCodes(body2), Class: errAuthExpired, Err: err2, Message: "post-relogin request failed"}
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
			return body, respHdrs, nil
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
			return body, respHdrs, nil

		case errCSRF:
			if body2, respHdrs2, ok := c.recoverCSRF(acc, bc, method, url, payload, endpoint, respHdrs, &lastErr); ok {
				return body2, respHdrs2, nil
			}
			continue

		case errAuthExpired:
			slog.Warn("auth expired (code 32), attempting relogin", slog.String("user", acc.Username))
			if reErr := c.relogin(acc); reErr != nil {
				slog.Warn("relogin failed, soft-deactivating", slog.String("user", acc.Username), slog.Any("error", reErr))
				c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
				lastErr = &APIError{Class: errAuthExpired, Err: reErr, Message: reErr.Error()}
				continue
			}
			authTok2, ct02, ua2 := acc.Credentials()
			body2, respHdrs2, status2, err2 := c.observedPoolReq(endpoint, bc, method, url, payload, twitterHeaders(authTok2, ct02, ua2))
			if err2 == nil && status2 == 200 {
				c.recordAPICall(endpoint, true, false)
				acc.RecordSuccess()
				return body2, respHdrs2, nil
			}
			c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
			lastErr = &APIError{Status: status2, Codes: extractErrorCodes(body2), Class: errAuthExpired, Err: err2, Message: "post-relogin request failed"}
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
				if reErr := c.relogin(acc); reErr == nil {
					authTok2, ct02, ua2 := acc.Credentials()
					body2, respHdrs2, status2, err2 := c.observedPoolReq(endpoint, bc, method, url, payload, twitterHeaders(authTok2, ct02, ua2))
					if err2 == nil && status2 == 200 {
						c.recordAPICall(endpoint, true, false)
						acc.RecordSuccess()
						slog.Info("CAPTCHA unlock succeeded", slog.String("user", acc.Username))
						return body2, respHdrs2, nil
					}
					slog.Warn("post-CAPTCHA request failed", slog.String("user", acc.Username))
				} else {
					slog.Warn("CAPTCHA unlock failed", slog.String("user", acc.Username), slog.Any("error", reErr))
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

	body, respHdrs, status, err := c.observedRequest(endpoint, c.client, "GET", url, guestHeaders(gt))
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
		body, respHdrs, status, err = c.observedRequest(endpoint, c.client, "GET", url, guestHeaders(newGT))
		if err != nil {
			return nil, nil, err
		}
		if status != 200 {
			c.recordAPICall(endpoint, false, false)
			apiErr := newAPIError(status, body, respHdrs)
			apiErr.Message = fmt.Sprintf("%s (guest retry) HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
			return nil, nil, apiErr
		}
		c.recordAPICall(endpoint, true, false)
		return body, respHdrs, nil
	}
	if status != 200 {
		c.recordAPICall(endpoint, false, false)
		apiErr := newAPIError(status, body, respHdrs)
		apiErr.Message = fmt.Sprintf("%s (guest) HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
		return nil, nil, apiErr
	}
	c.recordAPICall(endpoint, true, false)
	return body, respHdrs, nil
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
func (c *Client) recoverCSRF(acc *Account, bc *stealth.BrowserClient, method, url string, payload []byte, endpoint string, respHdrs map[string]string, lastErr *error) ([]byte, map[string]string, bool) {
	if serverCT0, ok := csrfRecoveryCT0(respHdrs); ok {
		slog.Warn("CSRF error 353, adopting server ct0", slog.String("user", acc.Username))
		acc.SetCT0(serverCT0)
		authTok2, ct02, ua2 := acc.Credentials()
		c.saveSession(acc, authTok2, ct02)
		body2, respHdrs2, status2, err2 := c.observedPoolReq(endpoint, bc, method, url, payload, twitterHeaders(authTok2, ct02, ua2))
		if err2 == nil && status2 == 200 && classifyError(body2, respHdrs2) == errNone {
			if newCT0 := extractCT0FromHeaders(respHdrs2); newCT0 != "" {
				acc.SetCT0(newCT0)
				authTok3, ct03, _ := acc.Credentials()
				c.saveSession(acc, authTok3, ct03)
			}
			c.recordAPICall(endpoint, true, false)
			acc.RecordSuccess()
			return body2, respHdrs2, true
		}
		acc.RecordFailure()
		slog.Warn("CSRF retry with server ct0 failed, attempting relogin", slog.String("user", acc.Username))
	} else {
		slog.Warn("CSRF error 353, no server ct0 to adopt, attempting relogin", slog.String("user", acc.Username))
	}

	if reErr := c.relogin(acc); reErr != nil {
		slog.Warn("relogin after CSRF failed", slog.String("user", acc.Username), slog.Any("error", reErr))
		c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
		*lastErr = &APIError{Class: errAuthExpired, Err: reErr, Message: reErr.Error()}
		return nil, nil, false
	}
	authTok3, ct03, ua3 := acc.Credentials()
	body3, respHdrs3, status3, err3 := c.observedPoolReq(endpoint, bc, method, url, payload, twitterHeaders(authTok3, ct03, ua3))
	if err3 == nil && status3 == 200 {
		c.recordAPICall(endpoint, true, false)
		acc.RecordSuccess()
		return body3, respHdrs3, true
	}
	c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
	*lastErr = &APIError{Status: status3, Codes: extractErrorCodes(body3), Class: errAuthExpired, Err: err3, Message: "post-relogin CSRF request failed"}
	return nil, nil, false
}

// recoverCSRFPost is the single-account (doPOST) twin of recoverCSRF. It adopts
// the SERVER ct0 from the 353 response and retries; only if x.com offered no ct0
// to adopt (or the adopted ct0 still fails) does it relogin and retry. It never
// generates a client-random ct0. Returns (body, true) on a recovered success;
// (nil, false) when recovery failed — the caller sets lastErr and `continue`s.
func (c *Client) recoverCSRFPost(acc *Account, bc *stealth.BrowserClient, url string, payload []byte, endpoint string, respHdrs map[string]string, lastErr *error) ([]byte, bool) {
	if serverCT0, ok := csrfRecoveryCT0(respHdrs); ok {
		slog.Warn("doPOST: CSRF error 353, adopting server ct0", slog.String("user", acc.Username))
		acc.SetCT0(serverCT0)
		authTok2, ct02, ua2 := acc.Credentials()
		c.saveSession(acc, authTok2, ct02)
		body2, respHdrs2, status2, err2 := c.observedRequestWithBody(endpoint, bc, "POST", url, twitterHeaders(authTok2, ct02, ua2), bytes.NewReader(payload))
		if err2 == nil && (status2 == 200 || status2 == 201) && classifyError(body2, respHdrs2) == errNone {
			if newCT0 := extractCT0FromHeaders(respHdrs2); newCT0 != "" {
				acc.SetCT0(newCT0)
				authTok3, ct03, _ := acc.Credentials()
				c.saveSession(acc, authTok3, ct03)
			}
			c.recordAPICall(endpoint, true, false)
			acc.RecordSuccess()
			return body2, true
		}
		acc.RecordFailure()
		slog.Warn("doPOST: CSRF retry with server ct0 failed, attempting relogin", slog.String("user", acc.Username))
	} else {
		slog.Warn("doPOST: CSRF error 353, no server ct0 to adopt, attempting relogin", slog.String("user", acc.Username))
	}

	if reErr := c.relogin(acc); reErr != nil {
		slog.Warn("doPOST: relogin after CSRF failed", slog.String("user", acc.Username), slog.Any("error", reErr))
		*lastErr = &APIError{Class: errAuthExpired, Err: reErr, Message: "relogin failed: " + reErr.Error()}
		return nil, false
	}
	authTok3, ct03, ua3 := acc.Credentials()
	body3, respHdrs3, status3, err3 := c.observedRequestWithBody(endpoint, bc, "POST", url, twitterHeaders(authTok3, ct03, ua3), bytes.NewReader(payload))
	if err3 == nil && (status3 == 200 || status3 == 201) && classifyError(body3, respHdrs3) == errNone {
		c.recordAPICall(endpoint, true, false)
		acc.RecordSuccess()
		return body3, true
	}
	*lastErr = &APIError{Status: status3, Codes: extractErrorCodes(body3), Class: errAuthExpired, Err: err3, Message: "post-relogin CSRF request failed"}
	return nil, false
}

// doPOST executes a POST mutation with a specific account.
// Unlike doGET, it does not rotate accounts from the pool — the caller provides the account.
// Handles CSRF rotation, auth expiry, and retries on transient errors.
func (c *Client) doPOST(ctx context.Context, acc *Account, endpoint, url string, payload []byte) ([]byte, error) {
	if err := stealth.DefaultJitter.Sleep(ctx); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			delay := stealth.DefaultBackoff.Duration(attempt)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// NO proactive ct0 rotation (see doPoolRequest / shouldProactivelyRotate):
		// client-rotating an established session's ct0 forces CSRF-353. ct0 stays
		// current via the server's set-cookie adopted on each success below.
		bc := c.clientForAccount(acc)
		authTok, ct0, ua := acc.Credentials()
		body, respHdrs, status, err := c.observedRequestWithBody(endpoint, bc, "POST", url, twitterHeaders(authTok, ct0, ua), bytes.NewReader(payload))
		if err != nil {
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
				if body2, ok := c.recoverCSRFPost(acc, bc, url, payload, endpoint, respHdrs, &lastErr); ok {
					return body2, nil
				}
				continue
			case errAuthExpired:
				slog.Warn("doPOST: auth expired, attempting relogin", slog.String("user", acc.Username))
				if reErr := c.relogin(acc); reErr != nil {
					lastErr = &APIError{Class: errAuthExpired, Err: reErr, Message: "relogin failed: " + reErr.Error()}
					continue
				}
				authTok2, ct02, ua2 := acc.Credentials()
				body2, _, status2, err2 := c.observedRequestWithBody(endpoint, bc, "POST", url, twitterHeaders(authTok2, ct02, ua2), bytes.NewReader(payload))
				if err2 == nil && (status2 == 200 || status2 == 201) {
					c.recordAPICall(endpoint, true, false)
					acc.RecordSuccess()
					return body2, nil
				}
				lastErr = &APIError{Status: status2, Codes: extractErrorCodes(body2), Class: errAuthExpired, Err: err2, Message: "post-relogin request failed"}
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
			// Endpoint faults (#61) do not charge the account — but a write is
			// never replayed on another account, so this returns immediately.
			if apiErr.Class != errEndpoint && apiErr.Class != errOverloaded {
				acc.RecordFailure()
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
			if body2, ok := c.recoverCSRFPost(acc, bc, url, payload, endpoint, respHdrs, &lastErr); ok {
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

// markProxyDown applies exponential backoff for proxy failures and emits a
// "proxy.down" alert through the PoolAlertHook seam so a consumer can observe a
// dead proxy pool without grepping logs. Before this alert, a dead proxy could
// produce thousands of failures with no counter anyone could alert on (the
// go-twitter #43 field incident: 5881 consecutive failures, zero observable
// proxy-layer signal).
func (c *Client) markProxyDown(acc *Account) {
	acc.mu.Lock()
	acc.proxyConsecFails++
	fails := acc.proxyConsecFails
	acc.mu.Unlock()

	duration := stealth.BackoffConfig{
		InitialWait: c.cfg.ProxyBackoffInitial,
		MaxWait:     c.cfg.ProxyBackoffMax,
		Multiplier:  2.0,
		JitterPct:   0.3,
	}.Duration(fails - 1)

	acc.mu.Lock()
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

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

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

func jsonEscape(b []byte) string {
	s := string(b)
	var result strings.Builder
	for _, ch := range s {
		switch {
		case ch == ' ':
			result.WriteString("%20")
		case ch == '"':
			result.WriteString("%22")
		case ch == '{':
			result.WriteString("%7B")
		case ch == '}':
			result.WriteString("%7D")
		case ch == '[':
			result.WriteString("%5B")
		case ch == ']':
			result.WriteString("%5D")
		case ch == ':':
			result.WriteString("%3A")
		case ch == ',':
			result.WriteString("%2C")
		case ch == '\'':
			result.WriteString("%27")
		case ch == '|':
			result.WriteString("%7C")
		default:
			result.WriteRune(ch)
		}
	}
	return result.String()
}
