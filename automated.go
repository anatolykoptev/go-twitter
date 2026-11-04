package twitter

import (
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

const (
	// automatedJitterFrac is the upper bound of the random extension added to
	// the automation cooldown, as a fraction of the base. Jitter only ever
	// lengthens the bench, so a fleet flagged together does not come back in
	// lock-step.
	automatedJitterFrac = 0.25

	// automatedRepeatWindow is how long after a 226 bench ENDS a further 226 on
	// the same account still counts as a repeat.
	automatedRepeatWindow = 24 * time.Hour

	// sharedSuspectWindow and sharedSuspectMin define the shared-cause signal:
	// this many DISTINCT accounts answered 226 within the window.
	sharedSuspectWindow = 10 * time.Minute
	sharedSuspectMin    = 2
)

// automatedHit is one account's most recent 226 inside the shared-cause window.
type automatedHit struct {
	at       time.Time
	endpoint string
}

// automatedTracker holds the sliding window of accounts that answered 226. The
// zero value is ready to use.
type automatedTracker struct {
	mu      sync.Mutex
	hits    map[string]automatedHit
	flagged bool // the window is currently at or above the threshold
}

// automatedCooldown extends base by up to automatedJitterFrac of random
// jitter. A zero or negative base stays zero.
func automatedCooldown(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	return base + time.Duration(rand.Float64()*automatedJitterFrac*float64(base))
}

// automatedBenchFor picks and records the bench for a 226 on acc. The graduated
// policy: the FIRST 226 on an account benches it for AuthCooldown (plus
// jitter) — X has judged one burst of traffic, which a pause usually settles,
// and a wrong first call costs an hour, not a shift. A SECOND 226 within
// automatedRepeatWindow of the previous bench ending means the pause did not
// change the verdict, so it escalates to BanCooldown (plus jitter), the same
// base as a lock (326). Both reads and writes feed one per-account state.
func (c *Client) automatedBenchFor(acc *Account) time.Duration {
	now := time.Now()
	acc.mu.Lock()
	repeat := !acc.automatedUntil.IsZero() && now.Before(acc.automatedUntil.Add(automatedRepeatWindow))
	base := c.cfg.AuthCooldown
	if repeat {
		base = c.cfg.BanCooldown
	}
	d := automatedCooldown(base)
	acc.automatedUntil = now.Add(d)
	acc.mu.Unlock()
	return d
}

// benchAutomated applies the account-scoped verdict for code 226: the account
// is soft-deactivated for the graduated cooldown (automatedBenchFor), one
// warning is logged with the account, the endpoint and the code — never the
// response body — and the shared-cause window is fed. The returned error
// carries errAutomated for IsAutomated. The twitter.outcome=automated counter
// is recorded per attempt by the observed* request wrappers, and the caller
// owns the MetricsHook record for the attempt (so each attempt is recorded
// exactly once).
//
// It is the single arm every request path (pool GET, pinned POST, media) uses,
// for a 200 and a 401/403 alike, with or without data riding alongside: a 226
// next to usable data is still the verdict.
func (c *Client) benchAutomated(acc *Account, endpoint string, status int, body []byte, hdrs map[string]string) *APIError {
	cooldown := c.automatedBenchFor(acc)
	slog.Warn("account flagged as automated, benching",
		slog.String("user", acc.Username),
		slog.String("endpoint", endpoint),
		slog.Int("code", automatedCode),
		slog.Duration("cooldown", cooldown))
	c.pool.SoftDeactivate(acc, cooldown)
	c.noteAutomatedHit(acc, endpoint)
	apiErr := newAPIError(status, body, hdrs)
	apiErr.Class = errAutomated
	apiErr.Message = "account flagged as automated (code 226)"
	return apiErr
}

// noteAutomatedHit feeds the shared-cause window. Detection only: when
// sharedSuspectMin or more distinct accounts have answered 226 within
// sharedSuspectWindow, it logs ONE error line per crossing (count and
// endpoints, no bodies), bumps the shared_suspect counter and fires the pool
// alert. 226 on several accounts at once points at something they share (a
// proxy, a request signature) rather than at each account; nothing here
// changes how requests are served.
func (c *Client) noteAutomatedHit(acc *Account, endpoint string) {
	now := time.Now()
	t := &c.automated
	t.mu.Lock()
	if t.hits == nil {
		t.hits = make(map[string]automatedHit)
	}
	for id, h := range t.hits {
		if now.Sub(h.at) > sharedSuspectWindow {
			delete(t.hits, id)
		}
	}
	t.hits[acc.ID()] = automatedHit{at: now, endpoint: endpoint}
	n := len(t.hits)
	crossed := n >= sharedSuspectMin && !t.flagged
	t.flagged = n >= sharedSuspectMin
	var endpoints []string
	if crossed {
		seen := map[string]bool{}
		for _, h := range t.hits {
			if !seen[h.endpoint] {
				seen[h.endpoint] = true
				endpoints = append(endpoints, h.endpoint)
			}
		}
		sort.Strings(endpoints)
	}
	t.mu.Unlock()
	if !crossed {
		return
	}
	slog.Error("automated verdict on several accounts, shared cause suspected",
		slog.Int("accounts", n),
		slog.Any("endpoints", endpoints),
		slog.Duration("window", sharedSuspectWindow))
	c.metrics.recordAutomatedSharedSuspect()
	if c.alertHook != nil {
		c.alertHook("automated.shared_suspect", map[string]any{
			"accounts":  n,
			"endpoints": endpoints,
			"window":    sharedSuspectWindow.String(),
		})
	}
}
