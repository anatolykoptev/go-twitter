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
