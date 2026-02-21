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

	active       bool
	reactivateAt time.Time
	client       *stealth.BrowserClient

	mu               sync.Mutex
	ct0RefreshedAt   time.Time
	proxyBackoff     time.Time
	proxyConsecFails int
	rateLimiter      *ratelimit.Limiter

	pool.HealthTracker
}

// ID implements pool.Identity.
func (a *Account) ID() string { return a.Username }

// IsActive implements pool.Identity.
func (a *Account) IsActive() bool { return a.active }

// SetActive implements pool.Identity.
func (a *Account) SetActive(v bool) { a.active = v }

// ReactivateAt implements pool.Identity.
func (a *Account) ReactivateAt() time.Time { return a.reactivateAt }

// SetReactivateAt implements pool.Identity.
func (a *Account) SetReactivateAt(t time.Time) { a.reactivateAt = t }

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

// AllowRequest checks if this account can make a request to the given endpoint.
func (a *Account) AllowRequest(endpoint string) bool {
	a.mu.Lock()
	if a.rateLimiter == nil {
		a.mu.Unlock()
		return true
	}
	rl := a.rateLimiter
	a.mu.Unlock()
	return rl.Allow(endpoint)
}

// MarkEndpointRateLimited marks an endpoint as rate-limited for this account.
func (a *Account) MarkEndpointRateLimited(endpoint string, until time.Time) {
	a.mu.Lock()
	if a.rateLimiter == nil {
		a.mu.Unlock()
		return
	}
	rl := a.rateLimiter
	a.mu.Unlock()
	rl.MarkRateLimited(endpoint, until)
}

// IsEndpointRateLimited returns true if the endpoint is currently blocked.
func (a *Account) IsEndpointRateLimited(endpoint string) bool {
	a.mu.Lock()
	if a.rateLimiter == nil {
		a.mu.Unlock()
		return false
	}
	rl := a.rateLimiter
	a.mu.Unlock()
	return rl.IsRateLimited(endpoint)
}

// EndpointAvailableAt returns when this account will be available for the given endpoint.
func (a *Account) EndpointAvailableAt(endpoint string) time.Time {
	a.mu.Lock()
	if a.rateLimiter == nil {
		a.mu.Unlock()
		return time.Time{}
	}
	rl := a.rateLimiter
	a.mu.Unlock()
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
			slog.Warn("invalid account entry, skipping", slog.String("entry", entry))
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
