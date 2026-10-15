package twitter

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
)

// Account represents a Twitter account with credentials for the pool.
type Account struct {
	Username   string
	Password   string
	AuthToken  string
	CT0        string
	TOTPSecret string
	Proxy      string
	UserAgent  string
	Profile    stealth.BrowserProfile

	// credsAuthoritative marks AuthToken/CT0 as freshly leased by a credential
	// service: loadOrLogin then ignores the on-disk session entirely. Set only
	// by tryWithAccount; unexported on purpose (no public API).
	credsAuthoritative bool

	active       bool
	reactivateAt time.Time
	client       *stealth.BrowserClient

	mu                 sync.Mutex
	ct0RefreshedAt     time.Time
	proxyBackoff       time.Time
	proxyConsecFails   int
	consecHeaderless88 int // consecutive code-88s with no parseable x-rate-limit-remaining
	rateLimiter        *ratelimit.Limiter

	// Header-derived rate-limit state (see ratelimit_state.go), per endpoint.
	// rlState is the last known X window; blockedUntil is every explicit block
	// (429, code 88, an exhausted window). Both are consulted instead of the
	// local request counter once an endpoint has header state.
	rlState      map[string]rateLimitState
	blockedUntil map[string]time.Time
	clock        func() time.Time // nil = time.Now; tests inject a fake

	pool.HealthTracker
}

// ID implements pool.Identity.
func (a *Account) ID() string { return a.Username }

// IsActive implements pool.Identity.
//
// Guarded by a.mu: the pool flips these fields (SetActive/SetReactivateAt) under
// its own lock while readers such as the metrics gauge callback run unlocked
// relative to the pool, so the Account must synchronise them itself.
func (a *Account) IsActive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active
}

// SetActive implements pool.Identity.
func (a *Account) SetActive(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active = v
}

// ReactivateAt implements pool.Identity.
func (a *Account) ReactivateAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reactivateAt
}

// SetReactivateAt implements pool.Identity.
func (a *Account) SetReactivateAt(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reactivateAt = t
}

// CT0Age returns the time since the ct0 token was last refreshed.
func (a *Account) CT0Age() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ct0RefreshedAt.IsZero() {
		return 24 * time.Hour
	}
	return time.Since(a.ct0RefreshedAt)
}

// RotateCT0 generates a fresh ct0 token and updates the refresh timestamp.
func (a *Account) RotateCT0() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.CT0 = GenerateCT0()
	a.ct0RefreshedAt = time.Now()
}

// SetCT0 updates the ct0 from a server response.
func (a *Account) SetCT0(ct0 string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.CT0 = ct0
	a.ct0RefreshedAt = time.Now()
}

// Credentials returns a snapshot of (authToken, ct0, userAgent) under lock.
func (a *Account) Credentials() (authToken, ct0, userAgent string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AuthToken, a.CT0, a.UserAgent
}

// SetCredentials atomically updates auth_token and ct0.
func (a *Account) SetCredentials(authToken, ct0 string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.AuthToken = authToken
	a.CT0 = ct0
	a.ct0RefreshedAt = time.Now()
}

// RecordSuccess records a successful request and clears the consecutive
// header-less-88 counter: a successful response proves the credentials work,
// so a later run of header-less 88s starts from zero. It shadows the embedded
// HealthTracker method on purpose — every success path (200, post-relogin,
// CSRF recovery, accepted 131) funnels through it, so the reset cannot be
// missed at a new call site.
func (a *Account) RecordSuccess() {
	a.HealthTracker.RecordSuccess()
	a.mu.Lock()
	a.consecHeaderless88 = 0
	a.mu.Unlock()
}

// noteRateLimited88 examines a code-88 response classified errRateLimited and
// returns the account's run length of consecutive header-less 88s. The run
// advances only when the response carried NO parseable x-rate-limit-remaining
// header: an 88 WITH the header — 0 or more requests remaining — is explained
// by its window and returns 0 without touching the counter. A banned account
// that answers header-less 88s on every endpoint crosses
// headerless88BanThreshold and is escalated to a ban by the caller.
func (a *Account) noteRateLimited88(respHdrs map[string]string) int {
	if _, ok := rateLimitRemaining(respHdrs); ok {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consecHeaderless88++
	return a.consecHeaderless88
}

// AllowRequest checks if this account can make a request to the given endpoint.
// While the endpoint's header window is live (see endpointGate) the account is
// allowed unless explicitly blocked: the headers are the authority and the
// local counter is skipped. Otherwise (cold start, headers absent, or the
// window expired) the local counter decides.
//
// Under header authority each allowed pick spends one slot of the window's
// remaining estimate, so N concurrent callers cannot all pass on the strength of
// one stale response: when the estimate reaches the headroom the endpoint is held
// until the reset. The next response folds in by the recordWindow merge rule
// (minimum within a window), so the estimate never rises inside a window.
// A pick that never produces a response (transport error) keeps its slot spent
// until the window resets: conservative, bounded by the window.
func (a *Account) AllowRequest(endpoint string) bool {
	authority, blocked, now, lim := a.endpointGate(endpoint)
	if !authority {
		return lim == nil || lim.Allow(endpoint)
	}
	if now.Before(blocked) {
		return false
	}
	// Spend a slot and, if that exhausts the estimate, hold the endpoint, all
	// under one lock: a check-then-act split would let a burst of callers pass
	// before the first of them blocks the endpoint.
	a.mu.Lock()
	if now.Before(a.blockedUntil[endpoint]) { // re-check: a concurrent pick may have just held it
		a.mu.Unlock()
		return false
	}
	if st, ok := a.rlState[endpoint]; ok && st.remaining > 0 {
		st.remaining--
		a.rlState[endpoint] = st
	}
	until, held := a.holdLocked(endpoint, now)
	rl := a.rateLimiter
	a.mu.Unlock()
	if held && rl != nil {
		rl.MarkRateLimited(endpoint, until)
	}
	return true
}

// endpointGate snapshots, under one lock, everything the eligibility methods
// need: whether the header window is authoritative for the endpoint, its
// explicit block, the account clock, and the local limiter.
//
// The window is authoritative only until its resetAt. After that the state
// proves nothing (X may have stopped sending headers for the operation), so the
// local counter takes over again until a response renews the window.
func (a *Account) endpointGate(endpoint string) (authority bool, blocked, now time.Time, rl *ratelimit.Limiter) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now = a.nowLocked()
	st, ok := a.rlState[endpoint]
	return ok && now.Before(st.resetAt), a.blockedUntil[endpoint], now, a.rateLimiter
}

// UpdateEndpointLimit sets this account's per-window request cap for the given
// endpoint, overriding the static default. Driven by the x-rate-limit-limit
// response header so the limiter self-tunes to X's real per-account-per-endpoint
// budget. Non-positive limits are ignored by the underlying limiter.
func (a *Account) UpdateEndpointLimit(endpoint string, limit int) {
	a.mu.Lock()
	if a.rateLimiter == nil {
		a.mu.Unlock()
		return
	}
	rl := a.rateLimiter
	a.mu.Unlock()
	rl.UpdateLimit(endpoint, limit)
}

// SyncRateLimit reads the x-rate-limit-limit header from a response and updates
// this account's per-endpoint cap when the header carries a valid positive
// value. A no-op when the header is absent or malformed (the prior cap stands).
// Called on every observed response (200 and 429) so the cap tracks X's real,
// possibly-shifting per-account budget.
func (a *Account) SyncRateLimit(endpoint string, respHdrs map[string]string) {
	if limit, ok := parseRateLimitLimit(respHdrs[xRateLimitLimit]); ok {
		a.UpdateEndpointLimit(endpoint, limit)
	}
}

// MarkEndpointRateLimited marks an endpoint as rate-limited for this account.
func (a *Account) MarkEndpointRateLimited(endpoint string, until time.Time) {
	a.mu.Lock()
	if a.blockedUntil == nil {
		a.blockedUntil = make(map[string]time.Time)
	}
	a.blockedUntil[endpoint] = until
	rl := a.rateLimiter
	a.mu.Unlock()
	if rl == nil {
		return
	}
	rl.MarkRateLimited(endpoint, until)
}

// IsEndpointRateLimited returns true if the endpoint is currently blocked.
func (a *Account) IsEndpointRateLimited(endpoint string) bool {
	authority, blocked, now, rl := a.endpointGate(endpoint)
	if authority {
		return now.Before(blocked)
	}
	if rl == nil {
		return false
	}
	return rl.IsRateLimited(endpoint)
}

// EndpointAvailableAt returns when this account will be available for the given endpoint.
func (a *Account) EndpointAvailableAt(endpoint string) time.Time {
	authority, blocked, now, rl := a.endpointGate(endpoint)
	if authority {
		if now.Before(blocked) {
			return blocked
		}
		return time.Time{}
	}
	if rl == nil {
		return time.Time{}
	}
	return rl.AvailableAt(endpoint)
}

// AssignBrowserProfile sets a browser profile based on index.
func AssignBrowserProfile(acc *Account, idx int) {
	p := stealth.BuiltinProfiles[idx%len(stealth.BuiltinProfiles)]
	acc.Profile = p
	acc.UserAgent = p.UserAgent
}

// ParseAccounts parses a comma-separated list of accounts.
// Format: "user1:pass1,user2:pass2" or "user1:pass1:auth_token:ct0,..."
// or "user1:pass1:auth_token:ct0:totp_secret,...".
func ParseAccounts(raw string) []*Account {
	var accounts []*Account
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 5)
		if len(parts) < 2 {
			// Never log entry content: a malformed entry may be (or contain) a
			// password / auth_token / ct0 / TOTP seed. Log only non-secret
			// discriminators. parts[0] is excluded too — when len(parts)==1 it IS
			// the whole secret.
			slog.Warn("invalid account entry, skipping",
				slog.Int("fields", len(parts)), slog.Int("len", len(entry)))
			continue
		}
		acc := &Account{
			Username: parts[0],
			Password: parts[1],
			active:   true,
		}
		if len(parts) >= 4 {
			acc.AuthToken = parts[2]
			acc.CT0 = parts[3]
			acc.ct0RefreshedAt = time.Now()
		}
		if len(parts) >= 5 && parts[4] != "" {
			acc.TOTPSecret = parts[4]
		}
		AssignBrowserProfile(acc, len(accounts))
		accounts = append(accounts, acc)
	}
	return accounts
}
