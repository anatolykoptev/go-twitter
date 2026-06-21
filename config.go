package twitter

import (
	"time"

	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/captcha"
)

const (
	// defaultAccountPaceMin / defaultAccountPaceRandom give a per-account
	// realized spacing of [0.8s, 2.0s). This spaces a single account's bursts
	// for stealth while staying far under the per-account-per-endpoint rate
	// ceiling (e.g. 187/15m ≈ one Followers req / 4.8s per account) and far
	// above the aggregate scrape demand, so the pace never caps throughput.
	defaultAccountPaceMin    = 800 * time.Millisecond
	defaultAccountPaceRandom = 1200 * time.Millisecond
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

	// AccountPaceMin is the minimum human-pace delay between consecutive requests
	// MADE BY THE SAME ACCOUNT, applied AFTER the pool selects an account and IN
	// ADDITION TO the anti-fingerprint jitter. Pacing is keyed by account, not by
	// domain: each account self-paces its own request rhythm via an independent
	// timestamp, so a low-frequency caller (seed) is never starved by a global
	// gate that a high-frequency caller (KOL/VC) holds. It is anti-burst stealth
	// spacing UNDER the per-account-per-endpoint rate-limit ceiling, never a
	// throughput cap below it.
	//
	// Together with AccountPaceRandom the realized inter-request spacing for one
	// account is [AccountPaceMin, AccountPaceMin+AccountPaceRandom) (variable,
	// never below AccountPaceMin). Default: 800ms. Set AccountPaceDisabled to off.
	AccountPaceMin time.Duration

	// AccountPaceRandom is the random jitter added on top of AccountPaceMin for
	// the per-account spacing (see AccountPaceMin). Default: 1.2s.
	AccountPaceRandom time.Duration

	// AccountPaceDisabled turns off the per-account human pace entirely. When
	// true only the anti-fingerprint jitter applies. Default: false (pacing
	// enabled with safe defaults).
	AccountPaceDisabled bool
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
	if cfg.AccountPaceMin == 0 {
		cfg.AccountPaceMin = defaultAccountPaceMin
	}
	if cfg.AccountPaceRandom == 0 {
		cfg.AccountPaceRandom = defaultAccountPaceRandom
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
