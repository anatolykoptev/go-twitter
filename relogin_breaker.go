package twitter

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// AutoReloginGateFunc adapts a plain function to the AutoReloginGate interface.
type AutoReloginGateFunc func(ctx context.Context, username string) (allowed bool, reason string)

// Allowed implements AutoReloginGate.
func (f AutoReloginGateFunc) Allowed(ctx context.Context, username string) (bool, string) {
	return f(ctx, username)
}

const (
	// reloginBreakerThreshold is the number of consecutive failed relogins for one
	// account before the breaker opens. Conservative: a couple of transient
	// failures are tolerated, but a persistently failing login (e.g. the
	// WAF-blocked guest-token path) is cut off before it can keep destroying the
	// still-valid session and hammering the self-worsening 399 throttle.
	reloginBreakerThreshold = 3

	// reloginBreakerWindow is how long the breaker stays open for an account once
	// tripped. After it elapses the next attempt is allowed again (not a permanent
	// latch). 30m matches the guest-token circuit-breaker window so the two cool
	// down together.
	reloginBreakerWindow = 30 * time.Minute
)

// reloginBreaker is a per-account circuit breaker for automatic relogin. It
// implements AutoReloginGate: relogin() consults Allowed BEFORE clearing
// credentials, so an OPEN breaker both skips a doomed login AND preserves the
// account's still-valid creds. This converts the production failure (a transient
// guest-token outage destroying a healthy session and then login-storming the
// WAF) into a bounded, self-healing pause.
//
// A nil gate means "always allow" (backward compatible); this default gate is
// wired only when the consumer did not set its own (e.g. go-social's gate).
type reloginBreaker struct {
	threshold int
	window    time.Duration

	mu    sync.Mutex
	state map[string]*breakerState
}

type breakerState struct {
	consecFails int
	openUntil   time.Time
}

// newReloginBreaker builds a per-account relogin breaker with the given
// consecutive-failure threshold and open-window cooldown.
func newReloginBreaker(threshold int, window time.Duration) *reloginBreaker {
	return &reloginBreaker{
		threshold: threshold,
		window:    window,
		state:     make(map[string]*breakerState),
	}
}

// Allowed implements AutoReloginGate: returns false while this account's breaker
// is open.
func (b *reloginBreaker) Allowed(_ context.Context, username string) (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[username]
	if st == nil {
		return true, ""
	}
	if time.Now().Before(st.openUntil) {
		return false, "relogin breaker open (too many consecutive login failures)"
	}
	return true, ""
}

// RecordFailure registers a failed relogin for the account and opens the breaker
// once the consecutive-failure threshold is reached.
func (b *reloginBreaker) RecordFailure(username string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[username]
	if st == nil {
		st = &breakerState{}
		b.state[username] = st
	}
	st.consecFails++
	if st.consecFails >= b.threshold {
		st.openUntil = time.Now().Add(b.window)
		st.consecFails = 0
		slog.Warn("twitter: relogin breaker tripped",
			slog.String("user", username),
			slog.Int("threshold", b.threshold),
			slog.Duration("open_for", b.window))
	}
}

// RecordSuccess clears the account's consecutive-failure count after a
// successful login so it is not penalized for old, recovered failures.
func (b *reloginBreaker) RecordSuccess(username string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if st := b.state[username]; st != nil {
		st.consecFails = 0
	}
}

// wireDefaultReloginGate installs the internal per-account relogin breaker as
// the client's gate when no external gate has been set. Idempotent and
// non-clobbering: an externally-configured gate (go-social) always wins.
func wireDefaultReloginGate(c *Client) {
	if c.reloginGate == nil {
		c.reloginGate = newReloginBreaker(reloginBreakerThreshold, reloginBreakerWindow)
	}
}
