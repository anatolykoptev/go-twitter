package twitter

import (
	"log/slog"
	"math/rand/v2"
	"time"
)

// automatedJitterFrac is the upper bound of the random extension added to the
// automation cooldown, as a fraction of the base. Jitter only ever lengthens
// the bench: a fleet flagged together must not come back in lock-step, and the
// result must never undercut the 326 path's BanCooldown.
const automatedJitterFrac = 0.25

// automatedCooldown is the bench duration for an account X flagged as
// automated (code 226): BanCooldown plus up to automatedJitterFrac of jitter.
//
// BanCooldown, not AuthCooldown, is the base. The account is not
// unauthenticated (AuthCooldown, 1h) — X is telling us its traffic pattern was
// judged non-human, the same class of account-level verdict as a lock (326) or
// a declared restriction (88 with window left), both of which bench for
// BanCooldown. Coming back sooner than a lock would, at the same rate, repeats
// the behaviour that earned the flag. A zero or negative base stays zero.
func automatedCooldown(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	return base + time.Duration(rand.Float64()*automatedJitterFrac*float64(base))
}

// benchAutomated applies the account-scoped verdict for code 226: the account
// is soft-deactivated for automatedCooldown, the call is counted as a failed
// (non-rate-limited) request, and ONE warning is logged with the account, the
// endpoint and the code — never the response body. The returned error carries
// errAutomated for IsAutomated. The twitter.outcome=automated counter is
// recorded per attempt by the observed* request wrappers.
//
// It is the single arm every request path (pool GET, pinned POST, media) uses,
// for a 200 and a 401/403 alike, with or without data riding alongside: a 226
// next to usable data is still the verdict.
func (c *Client) benchAutomated(acc *Account, endpoint string, status int, body []byte, hdrs map[string]string) *APIError {
	c.recordAPICall(endpoint, false, false)
	cooldown := automatedCooldown(c.cfg.BanCooldown)
	slog.Warn("account flagged as automated, benching",
		slog.String("user", acc.Username),
		slog.String("endpoint", endpoint),
		slog.Int("code", automatedCode),
		slog.Duration("cooldown", cooldown))
	c.pool.SoftDeactivate(acc, cooldown)
	apiErr := newAPIError(status, body, hdrs)
	apiErr.Class = errAutomated
	apiErr.Message = "account flagged as automated (code 226)"
	return apiErr
}
