package twitter

import (
	"time"

	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/captcha"
)

// ClientConfig holds all configuration for the Twitter client.
type ClientConfig struct {
	// Accounts is the list of Twitter accounts to use.
	Accounts []*Account

	// DefaultProxy is the proxy URL for accounts without per-account proxies.
	DefaultProxy string

	// SessionTTL controls how long saved sessions are considered valid.
	SessionTTL time.Duration

	// AuthCooldown is the soft-deactivation duration for auth errors.
	AuthCooldown time.Duration

	// BanCooldown is the soft-deactivation duration for banned/locked accounts.
	BanCooldown time.Duration

	// NonResponsiveCooldown is the BASE backoff for accounts that trip the
	// consecutive-failure threshold against a transiently-broken endpoint. The
	// effective cooldown grows per trip (x2, capped at 30m, +/-30% jitter) so a
	// flapping upstream self-heals once it recovers instead of latching the pool
	// permanently. Default: 5m.
	NonResponsiveCooldown time.Duration

	// CaptchaSolver is the optional CAPTCHA solver for locked accounts.
	CaptchaSolver captcha.Solver

	// RateLimit configures per-account per-endpoint rate limiting.
	RateLimit ratelimit.Config

	// OpenAccountCount is the number of anonymous guest accounts to create at startup.
	OpenAccountCount int

	// MetricsHook is called on each API request for external metrics collection.
	// endpoint is the operation name, success and rateLimited indicate the outcome.
	MetricsHook func(endpoint string, success, rateLimited bool)

	// SessionDir overrides the default session persistence directory.
	// Default: ~/.go-twitter/sessions
	SessionDir string

	// ProxyBackoffInitial is the initial backoff for proxy failures.
	ProxyBackoffInitial time.Duration

	// ProxyBackoffMax is the maximum backoff for proxy failures.
	ProxyBackoffMax time.Duration

	// PoolAlertHook is called when the pool emits alerts (account deactivation, proxy failures, etc.).
	// topic is the alert type (e.g. "pool.deactivated"), payload contains details.
	PoolAlertHook func(topic string, payload any)

	// DisableGuestFallback disables the guest-token fallback path entirely.
	// When true, endpoints that would normally fall back to guest mode after
	// pool exhaustion will return an error instead. Recommended in production
	// where guest tokens from datacenter IPs return persistent 403 errors.
	// Default: false (guest fallback enabled for backward compatibility).
	DisableGuestFallback bool

	// DomainPaceMin is the minimum human-pace delay between consecutive requests
	// to the same Twitter domain (x.com / twitter.com), applied at the
	// pool-request site IN ADDITION TO the anti-fingerprint jitter. It spaces the
	// whole scrape workload (Retweeters/Followers/KOL/seed) under the per-account
	// rate-limit ceiling so the pool self-paces instead of bursting through and
	// tripping "all accounts unavailable". The per-account rate limiter remains
	// the hard ceiling; this is the soft, variable spacing.
	//
	// Together with DomainPaceRandom the realized inter-request spacing is
	// [DomainPaceMin, DomainPaceMin+DomainPaceRandom) (variable, never below
	// DomainPaceMin). Default: 4.5s. Set DomainPaceDisabled to turn pacing off.
	DomainPaceMin time.Duration

	// DomainPaceRandom is the random jitter added on top of DomainPaceMin for the
	// human-pace spacing (see DomainPaceMin). Default: 4.5s.
	DomainPaceRandom time.Duration

	// DomainPaceDisabled turns off the per-domain human pace entirely. When true,
	// no DomainLimiter is wired and only the anti-fingerprint jitter applies
	// (legacy behaviour). Default: false (pacing enabled with safe defaults).
	DomainPaceDisabled bool
}

// defaults fills in zero-value config fields with sensible defaults.
func (cfg *ClientConfig) defaults() {
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 24 * time.Hour
	}
	if cfg.AuthCooldown == 0 {
		cfg.AuthCooldown = 1 * time.Hour
	}
	if cfg.BanCooldown == 0 {
		cfg.BanCooldown = 6 * time.Hour
	}
	if cfg.NonResponsiveCooldown == 0 {
		cfg.NonResponsiveCooldown = 5 * time.Minute
	}
	if cfg.DomainPaceMin == 0 {
		cfg.DomainPaceMin = 4500 * time.Millisecond
	}
	if cfg.DomainPaceRandom == 0 {
		cfg.DomainPaceRandom = 4500 * time.Millisecond
	}
	if cfg.RateLimit.RequestsPerWindow == 0 {
		cfg.RateLimit = ratelimit.DefaultConfig
	}
	if cfg.ProxyBackoffInitial == 0 {
		cfg.ProxyBackoffInitial = 30 * time.Second
	}
	if cfg.ProxyBackoffMax == 0 {
		cfg.ProxyBackoffMax = 30 * time.Minute
	}
}
