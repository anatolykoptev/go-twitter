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

	// proxyClientFailed marks an account whose pinned per-account proxy client
	// could not be built in NewClient. Deliberately NOT the soft-deactivation
	// `active` flag: pool.Next re-activates any account with a past reactivateAt
	// (SetActive(true)), which would resurrect it. This flag is write-once and
	// gates IsActive, so no reactivation path can make the account selectable.
	proxyClientFailed bool

	mu                 sync.Mutex
	ct0RefreshedAt     time.Time
	proxyBackoff       time.Time
	proxyConsecFails   int
	consecHeaderless88 int // consecutive code-88s with no parseable x-rate-limit-remaining
	inFlight           int // requests currently holding this account's slot (ClientConfig.MaxInFlightPerAccount)
	rateLimiter        *ratelimit.Limiter

	// Budget share (see SetBudgetPct): the fraction of each endpoint window
	// this account may spend. budgetPct is the raw setter value (0 = never
	// set ⇒ 100); rlCaps remembers each endpoint's RAW cap (seeded via
	// seedRateLimits, refined by SyncRateLimit, or the configured fallback for
	// unlisted endpoints) so a runtime pct change can re-scale every key;
	// rlFallback is the cap unlisted endpoints use (the limiter keeps its own
	// config private).
	budgetPct  int
	rlCaps     map[string]int
	rlFallback int

	// heldUntil benches the account pool-wide — not a failure, so deliberately
	// separate from active/reactivateAt (a fault cooldown ending early must
	// not lift a hold, and the pool's deactivation alert must not fire). The
	// eligibility methods consult it via endpointGate.
	heldUntil time.Time

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
	return a.active && !a.proxyClientFailed
}

// markProxyClientFailed permanently disables the account for this Client.
func (a *Account) markProxyClientFailed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.proxyClientFailed = true
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

// setHeldUntil sets the pool-wide operator hold: until that time every
// endpoint gate reports the account unavailable. A past or zero time clears
// the hold. Not a fault: nothing here touches health, failure counters or
// reactivation.
func (a *Account) setHeldUntil(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.heldUntil = t
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

// tryAcquireFlight takes one in-flight slot when the account is below limit.
// A limit <= 0 is uncapped and always succeeds. The call must run inside the
// pick that selects the account (the pool filter under pool.mu): selection and
// slot-taking are then atomic, so concurrent callers cannot both pass a
// capacity check before either takes a slot.
func (a *Account) tryAcquireFlight(limit int) bool {
	if limit <= 0 {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inFlight >= limit {
		return false
	}
	a.inFlight++
	return true
}

// flightFull reports whether the account is at its in-flight cap — the
// read-only twin of tryAcquireFlight used where the eligibility check must not
// take a slot (the cap-wait decision). A limit <= 0 is never full.
func (a *Account) flightFull(limit int) bool {
	if limit <= 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inFlight >= limit
}

// releaseFlight returns one in-flight slot. Every request path releases it
// when the attempt ends — success or error — via defer.
func (a *Account) releaseFlight() {
	a.mu.Lock()
	if a.inFlight > 0 {
		a.inFlight--
	}
	a.mu.Unlock()
}

// InFlight reports the account's current in-flight request count. The
// twitter.client.pool.inflight gauge sums it across the pool; tests assert on
// it.
func (a *Account) InFlight() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inFlight
}

// installRateLimiter wires the per-endpoint limiter at construction time and
// records the cap unlisted endpoints fall back to (the limiter keeps its own
// config private). Not mutex-guarded: it runs before the account is shared.
func (a *Account) installRateLimiter(cfg ratelimit.Config, opts ...ratelimit.LimiterOption) {
	a.rateLimiter = ratelimit.NewLimiter(cfg, opts...)
	a.rlFallback = cfg.RequestsPerWindow
}

// SetBudgetPct sets the percentage of each endpoint's per-account X window
// this account may spend (1-100). Values outside that range — including the
// never-set zero value — mean 100 (the full window, today's behaviour). It is
// safe to call at runtime on a live pooled account: endpointGate re-derives
// each endpoint's scaled cap under the account lock on every eligibility
// check, so the next pick sees the new share.
func (a *Account) SetBudgetPct(pct int) {
	if pct < 1 || pct > 100 {
		pct = 100
	}
	a.mu.Lock()
	a.budgetPct = pct
	a.mu.Unlock()
}

// BudgetPct returns the percentage of each endpoint's window this account may
// spend: the SetBudgetPct value, or 100 when never set or set out of range.
func (a *Account) BudgetPct() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return normalizedBudgetPct(a.budgetPct)
}

// normalizedBudgetPct maps a stored budgetPct to its effective value: the
// never-set zero and every out-of-range value mean 100 (the full window).
func normalizedBudgetPct(pct int) int {
	if pct < 1 || pct > 100 {
		return 100
	}
	return pct
}

// scaledByPct applies the budget share to a raw per-window cap: max(1, raw*pct/100)
// so a non-zero budget can never collapse an endpoint to deny-everything.
func scaledByPct(raw, pct int) int {
	if pct < 1 || pct > 100 {
		pct = 100
	}
	if scaled := raw * pct / 100; scaled > 0 {
		return scaled
	}
	return 1
}

// budgetShareLocked returns how much of the endpoint's window the account must
// NOT spend under its budget: limit*(100-pct)/100, integer math. The basis is
// the window's reported limit, else the seeded/registered cap, else the
// measured table, else the configured fallback. a.mu must be held.
func (a *Account) budgetShareLocked(endpoint string, reportedLimit int) int {
	limit := reportedLimit
	if limit <= 0 {
		limit = a.rlCaps[endpoint]
	}
	if limit <= 0 {
		limit = endpointDefaultLimit[endpoint]
	}
	if limit <= 0 {
		limit = a.rlFallback
	}
	if limit <= 0 {
		return 0
	}
	return limit * (100 - normalizedBudgetPct(a.budgetPct)) / 100
}

// scaledCapLocked returns the cold-path cap the local limiter must enforce for
// endpoint: the budget share of its raw cap. ok is false when no raw cap is
// known (a hand-built account whose fallback was never recorded), in which case
// the limiter's own config decides — today's behaviour for such accounts.
// The fallback is registered into rlCaps on first sight so a later
// SetBudgetPct re-scales it too. a.mu must be held.
func (a *Account) scaledCapLocked(endpoint string) (scaled int, ok bool) {
	raw, ok := a.rlCaps[endpoint]
	if !ok {
		if a.rlFallback <= 0 {
			return 0, false
		}
		if a.rlCaps == nil {
			a.rlCaps = make(map[string]int)
		}
		a.rlCaps[endpoint] = a.rlFallback
		raw = a.rlFallback
	}
	return scaledByPct(raw, a.budgetPct), true
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
	authority, blocked, now, lim, heldUntil := a.endpointGate(endpoint)
	if now.Before(heldUntil) {
		return false // operator hold: every endpoint, no slot spent, no fault charged
	}
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
// explicit block, the account clock, the local limiter, and the account hold.
// It also keeps the limiter's per-key cap in step with the budget share
// (scaledCapLocked): every reader of the local counter then sees the same
// scaled cap with no second decision site.
//
// The window is authoritative only until its resetAt. After that the state
// proves nothing (X may have stopped sending headers for the operation), so the
// local counter takes over again until a response renews the window.
func (a *Account) endpointGate(endpoint string) (authority bool, blocked, now time.Time, rl *ratelimit.Limiter, heldUntil time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now = a.nowLocked()
	st, ok := a.rlState[endpoint]
	if rl = a.rateLimiter; rl != nil {
		if cap, ok := a.scaledCapLocked(endpoint); ok {
			rl.UpdateLimit(endpoint, cap)
		}
	}
	return ok && now.Before(st.resetAt), a.blockedUntil[endpoint], now, rl, a.heldUntil
}

// UpdateEndpointLimit sets this account's per-window request cap for the given
// endpoint, overriding the static default. Driven by the x-rate-limit-limit
// response header so the limiter self-tunes to X's real per-account-per-endpoint
// budget. Non-positive limits are ignored (the prior cap stands). The raw cap
// is what is recorded; the limiter enforces its budget-scaled value.
func (a *Account) UpdateEndpointLimit(endpoint string, limit int) {
	if limit <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rateLimiter == nil {
		return
	}
	if a.rlCaps == nil {
		a.rlCaps = make(map[string]int)
	}
	a.rlCaps[endpoint] = limit
	a.rateLimiter.UpdateLimit(endpoint, scaledByPct(limit, a.budgetPct))
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

// IsEndpointRateLimited returns true if the endpoint is currently blocked —
// by an explicit block, an exhausted window, a spent local budget, or an
// account hold.
func (a *Account) IsEndpointRateLimited(endpoint string) bool {
	authority, blocked, now, rl, heldUntil := a.endpointGate(endpoint)
	if now.Before(heldUntil) {
		return true
	}
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
	authority, blocked, now, rl, heldUntil := a.endpointGate(endpoint)
	var at time.Time
	switch {
	case authority:
		if now.Before(blocked) {
			at = blocked
		}
	case rl != nil:
		at = rl.AvailableAt(endpoint)
	}
	// An account hold pushes the earliest availability out to its expiry; it
	// never pulls an earlier endpoint block in.
	if heldUntil.After(now) && heldUntil.After(at) {
		at = heldUntil
	}
	return at
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
