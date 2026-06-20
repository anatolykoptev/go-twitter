package twitter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/xpff"
	"github.com/anatolykoptev/go-twitter/xtid"
)

// Client is the top-level Twitter scraping client.
type Client struct {
	client               *stealth.BrowserClient
	pool                 *pool.Pool[*Account]
	xtidMgr              *xtid.Manager
	xpffGen              *xpff.Generator
	cfg                  ClientConfig
	reloginGate          AutoReloginGate          // nil = always allow
	nonResponsiveBackoff pool.BackoffConfig       // transient-failure backoff (base from cfg, x2, cap 30m)
	domainPacer          *ratelimit.DomainLimiter // human-pace spacing per Twitter domain (nil = disabled)

	mu                sync.Mutex
	guestToken        string
	guestLimitedUntil time.Time
	guestConsecFails  int
	guestBlockedUntil time.Time
}

// NewClient creates a fully-wired Twitter client.
func NewClient(cfg ClientConfig) (*Client, error) {
	cfg.defaults()

	for _, acc := range cfg.Accounts {
		acc.active = true
		acc.rateLimiter = ratelimit.NewLimiter(cfg.RateLimit)
		acc.HealthTracker = pool.DefaultHealthTracker()
	}

	opts := []stealth.ClientOption{
		stealth.WithHeaderOrder(twitterHeaderOrder),
	}
	if cfg.DefaultProxy != "" {
		opts = append(opts, stealth.WithProxy(cfg.DefaultProxy))
	}
	bc, err := stealth.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("stealth client: %w", err)
	}

	alertHook := cfg.PoolAlertHook
	if alertHook == nil {
		alertHook = func(topic string, payload any) {
			slog.Warn("pool alert", slog.String("topic", topic), slog.Any("payload", payload))
		}
	}

	// xtid locates ondemand.s via the bundle core, fetching warm pages through the
	// stealth client (shared TLS fingerprint + proxy). Unauthenticated/guest is
	// fine. On failure we degrade — x-client-transaction-id is simply omitted —
	// and route the alert through the pool hook; NewClient never hard-fails here.
	fetcher := &bundle.StealthFetcher{Client: bc, UserAgent: defaultUserAgent}
	mgr := xtid.NewManager(fetcher)
	if err := mgr.Initialize(); err != nil {
		slog.Warn("xtid: init failed, x-client-transaction-id will be missing", slog.Any("error", err))
		alertHook("xtid.init_failed", map[string]any{"error": err.Error()})
	}

	nonResponsiveBackoff := pool.BackoffConfig{
		InitialWait: cfg.NonResponsiveCooldown,
		MaxWait:     30 * time.Minute,
		Multiplier:  2.0,
		JitterPct:   0.3,
	}
	poolCfg := pool.Config{
		AlertHook: alertHook,
		ProxyBackoff: pool.BackoffConfig{
			InitialWait: cfg.ProxyBackoffInitial,
			MaxWait:     cfg.ProxyBackoffMax,
			Multiplier:  2.0,
			JitterPct:   0.3,
		},
		NonResponsiveBackoff: nonResponsiveBackoff,
	}
	p := pool.New(cfg.Accounts, poolCfg)

	// The guest_id cookie is set by the warm-page fetches above; read it from the
	// stealth client's jar (xtid no longer surfaces it). Fall back to a generated
	// id when the fetch was bot-walled. Log which branch fired so a silent
	// downgrade to a synthetic id (the warm-page fetch was walled and never set a
	// real cookie) is observable rather than masked behind a well-formed header.
	xpffGuestID := bc.GetCookieValue("https://x.com", "guest_id")
	if xpffGuestID == "" {
		xpffGuestID = xpff.GenerateGuestID()
		slog.Warn("xpff: guest_id cookie absent, using synthetic GenerateGuestID fallback (warm-page fetch likely bot-walled)")
	} else {
		slog.Info("xpff: using server-set guest_id cookie")
	}
	xpffGen := xpff.New(xpffGuestID, defaultUserAgent)

	// Human-pace per-domain spacing for x.com / twitter.com. ADDITIVE to the
	// anti-fingerprint jitter: it spaces the whole scrape workload under the
	// per-account rate-limit ceiling so the pool self-paces over time instead of
	// bursting through it and tripping "all accounts unavailable". The per-account
	// rate limiter remains the authoritative ceiling.
	domainPacer := buildDomainPacer(cfg)

	c := &Client{
		client:               bc,
		pool:                 p,
		xtidMgr:              mgr,
		xpffGen:              xpffGen,
		cfg:                  cfg,
		nonResponsiveBackoff: nonResponsiveBackoff,
		domainPacer:          domainPacer,
	}

	for _, acc := range cfg.Accounts {
		if acc.Proxy != "" {
			accClient, err := stealth.NewClient(
				stealth.WithProxy(acc.Proxy),
				stealth.WithProfile(acc.Profile.TLSProfile),
				stealth.WithHeaderOrder(twitterHeaderOrder),
			)
			if err != nil {
				slog.Warn("per-account client failed", slog.String("user", acc.Username), slog.Any("error", err))
			} else {
				acc.client = accClient
			}
		}

		if err := c.loadOrLogin(acc, c.clientForAccount(acc)); err != nil {
			slog.Warn("account login failed", slog.String("user", acc.Username), slog.Any("error", err))
			acc.SetActive(false)
		} else {
			acc.SetActive(true)
		}
	}

	if cfg.OpenAccountCount > 0 {
		ctx := context.Background()
		for i := 0; i < cfg.OpenAccountCount; i++ {
			acc, err := c.loginOpenAccount(ctx)
			if err != nil {
				slog.Warn("open account failed", slog.Int("attempt", i+1), slog.Any("error", err))
				continue
			}
			acc.rateLimiter = ratelimit.NewLimiter(cfg.RateLimit)
			acc.HealthTracker = pool.DefaultHealthTracker()
			p.Add(acc)
		}
	}

	return c, nil
}

// clientForAccount returns the per-account client if available, otherwise the shared client.
func (c *Client) clientForAccount(acc *Account) *stealth.BrowserClient {
	if acc.client != nil {
		return acc.client
	}
	return c.client
}

// doPoolReq is a helper for doPoolRequest: executes method+payload via doRequestWithBody.
func (c *Client) doPoolReq(bc *stealth.BrowserClient, method, urlStr string, payload []byte, headers map[string]string) ([]byte, map[string]string, int, error) {
	var body io.Reader
	if len(payload) > 0 {
		body = bytes.NewReader(payload)
	}
	return c.doRequestWithBody(bc, method, urlStr, headers, body)
}

// doRequest executes a request with xtid header injection (no body).
func (c *Client) doRequest(bc *stealth.BrowserClient, method, urlStr string, headers map[string]string) ([]byte, map[string]string, int, error) {
	return c.doRequestWithBody(bc, method, urlStr, headers, nil)
}

// doRequestWithBody executes a request with xtid header injection and an optional body.
func (c *Client) doRequestWithBody(bc *stealth.BrowserClient, method, urlStr string, headers map[string]string, body io.Reader) ([]byte, map[string]string, int, error) {
	urlPath := urlStr
	if u, parseErr := url.Parse(urlStr); parseErr == nil {
		urlPath = u.Path
	}
	if txID, txErr := c.xtidMgr.GenerateID(method, urlPath); txErr == nil {
		headers["x-client-transaction-id"] = txID
	} else {
		slog.Debug("xtid: failed to generate transaction id", slog.Any("error", txErr))
	}

	if xpffVal, xpffErr := c.xpffGen.Generate(); xpffErr == nil {
		headers["x-xp-forwarded-for"] = xpffVal
	} else {
		slog.Debug("xpff: failed to generate header", slog.Any("error", xpffErr))
	}

	return bc.DoWithHeaderOrder(method, urlStr, headers, body, twitterHeaderOrder)
}

// Pool returns the underlying account pool.
func (c *Client) Pool() *pool.Pool[*Account] {
	return c.pool
}

// AccountByUsername returns the pool account matching the given username (case-insensitive).
// Returns nil if not found.
func (c *Client) AccountByUsername(username string) *Account {
	for _, acc := range c.pool.Items() {
		if strings.EqualFold(acc.Username, username) {
			return acc
		}
	}
	return nil
}

// AccountHealth describes the health state of a single pool account.
type AccountHealth struct {
	Username    string
	Active      bool
	Total       int
	Failed      int
	ConsecFails int
}

// HealthReport returns health stats for all accounts in the pool.
func (c *Client) HealthReport() []AccountHealth {
	items := c.pool.Items()
	report := make([]AccountHealth, 0, len(items))
	for _, acc := range items {
		total, failed, consecFails := acc.Stats()
		report = append(report, AccountHealth{
			Username:    acc.Username,
			Active:      acc.IsActive(),
			Total:       total,
			Failed:      failed,
			ConsecFails: consecFails,
		})
	}
	return report
}

// recordAPICall calls the metrics hook if configured.
func (c *Client) recordAPICall(endpoint string, success, rateLimited bool) {
	if c.cfg.MetricsHook != nil {
		c.cfg.MetricsHook(endpoint, success, rateLimited)
	}
}

// setGuestToken stores a fresh guest token.
func (c *Client) setGuestToken(token string) {
	c.mu.Lock()
	c.guestToken = token
	c.guestLimitedUntil = time.Time{}
	c.mu.Unlock()
}

// markGuestTokenRateLimited marks the guest token as rate-limited.
func (c *Client) markGuestTokenRateLimited(until time.Time) {
	c.mu.Lock()
	c.guestLimitedUntil = until
	c.mu.Unlock()
}

// getGuestTokenCached returns the current guest token and whether it is usable.
// Returns (_, false) if the guest-token circuit breaker is currently open.
func (c *Client) getGuestTokenCached() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.guestBlockedUntil) {
		return "", false
	}
	if c.guestToken == "" || time.Now().Before(c.guestLimitedUntil) {
		return "", false
	}
	return c.guestToken, true
}

// domainPaceWindowCap and domainPaceWindow keep the DomainLimiter embedded
// window limiter from ever gating: a very high cap over a short window means the
// MinDelay floor (>= 4.5s) is always the binding constraint, never the window.
// This makes the DomainLimiter a pure pacer; the per-account limiter remains the
// authoritative request ceiling. See buildDomainPacer for the full rationale.
const (
	domainPaceWindowCap = 1_000_000
	domainPaceWindow    = time.Minute
)

// buildDomainPacer constructs the per-domain human-pace limiter for the Twitter
// hosts (x.com and twitter.com, including subdomains like api.twitter.com).
// Returns nil when pacing is disabled. MinDelay is the hard floor between
// consecutive same-domain requests; +RandomDelay makes the realized spacing
// variable (human-like). This is the single authority for pacer construction —
// NewClient and the unit tests both go through here so the test exercises the
// exact wiring production uses, not a copy.
//
// NOTE: go-stealth DomainLimiter.Wait polls every 50ms, so realized spacing
// quantizes to the 50ms poll period. At our production values (>=4.5s) this is
// negligible (~90 distinct buckets across the 4.5s jitter span) and the rhythm
// stays human-variable; it only matters for sub-second delays.
func buildDomainPacer(cfg ClientConfig) *ratelimit.DomainLimiter {
	if cfg.DomainPaceDisabled {
		return nil
	}
	// IMPORTANT: DomainConfig embeds a sliding-window rate limiter whose
	// RequestsPerWindow defaults to 0 — and ratelimit.Limiter.Allow treats
	// count<=0 as DENY, so a delay-only DomainConfig (zero window fields) would
	// block forever. We want the DomainLimiter to be a PURE pacer here
	// (MinDelay+RandomDelay spacing only); the per-account ratelimit.Config
	// (50/15m x N accounts) is already the authoritative hard ceiling and we do
	// not want a second redundant window gate. So set the embedded window
	// permissively (effectively unbounded) and let MinDelay/RandomDelay do the
	// spacing. domainPaceWindowCap / domainPaceWindow are large/short enough that
	// the spacing floor is always reached long before the window cap matters.
	return ratelimit.NewDomainLimiter(
		ratelimit.DomainConfig{
			Domain:            "*.x.com",
			RequestsPerWindow: domainPaceWindowCap,
			WindowDuration:    domainPaceWindow,
			MinDelay:          cfg.DomainPaceMin,
			RandomDelay:       cfg.DomainPaceRandom,
		},
		ratelimit.DomainConfig{
			Domain:            "*.twitter.com",
			RequestsPerWindow: domainPaceWindowCap,
			WindowDuration:    domainPaceWindow,
			MinDelay:          cfg.DomainPaceMin,
			RandomDelay:       cfg.DomainPaceRandom,
		},
	)
}
