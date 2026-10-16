package twitter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/xpff"
	"github.com/anatolykoptev/go-twitter/xtid"
	"go.opentelemetry.io/otel/metric"
)

// Client is the top-level Twitter scraping client.
type Client struct {
	client               *stealth.BrowserClient
	pool                 *pool.Pool[*Account]
	xtidMgr              *xtid.Manager
	xpffGen              *xpff.Generator
	cfg                  ClientConfig
	reloginGate          AutoReloginGate                 // nil = always allow
	nonResponsiveBackoff pool.BackoffConfig              // transient-failure backoff (base from cfg, x2, cap 30m)
	accountPacer         *ratelimit.KeyedPacer           // per-account human-pace spacing (keyed by account, nil = disabled)
	alertHook            func(topic string, payload any) // proxy/pool event sink (PoolAlertHook or slog fallback)
	metrics              *clientMetrics                  // OpenTelemetry instruments (nil-safe)
	poolGaugeReg         metric.Registration             // pool.accounts callback; unregistered by Close (guarded by mu)
	warmDoerOverride     warmDoer                        // test seam: replaces the account client for xtid warm-page fetches

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
		seedRateLimits(acc) // prime per-endpoint caps with X's measured real limits
		acc.HealthTracker = pool.DefaultHealthTracker()
	}

	alertHook := cfg.PoolAlertHook
	if alertHook == nil {
		alertHook = func(topic string, payload any) {
			slog.Warn("pool alert", slog.String("topic", topic), slog.Any("payload", payload))
		}
	}
	m := newClientMetrics(cfg.MeterProvider)

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

	// Per-account transports for accounts pinned to their own proxy. A build
	// failure disables only that account (a pooled consumer's shared client
	// must survive one malformed proxy row) and NewClient fails only when no
	// account is usable. Fail-closed is kept: the failed account is marked
	// permanently unselectable (IsActive) and clientForAccount refuses to lend
	// it the shared client, which may be direct (the *WithSocial path sets no
	// DefaultProxy) and would send its auth_token/ct0 from this host's IP.
	//
	// go-stealth's BrowserClient exposes no Close/CloseIdleConnections, so the
	// clients built before an error return below cannot be released explicitly;
	// they hold no goroutines until first use and are reclaimed by the GC.
	var buildErrs []error
	usable := 0
	for _, acc := range cfg.Accounts {
		if acc.Proxy == "" {
			usable++
			continue
		}
		accClient, err := stealth.NewClient(
			stealth.WithProxy(acc.Proxy),
			stealth.WithProfile(acc.Profile.TLSProfile),
			stealth.WithHeaderOrder(twitterHeaderOrder),
		)
		if err != nil {
			acc.markProxyClientFailed()
			label, reason := proxyBuildFailure(acc.Proxy)
			buildErrs = append(buildErrs, fmt.Errorf("per-account client for %s: %s", acc.Username, reason))
			slog.Warn("per-account proxy client build failed, account disabled",
				slog.String("user", acc.Username), slog.String("proxy", label), slog.String("error", reason))
			m.recordProxyClientFailure()
			alertHook("account.proxy_client_failed", map[string]any{
				"user":  acc.Username,
				"proxy": label,
				"error": reason,
			})
			continue
		}
		acc.client = accClient
		usable++
	}
	if len(cfg.Accounts) > 0 && usable == 0 {
		return nil, fmt.Errorf("no usable account: %w", errors.Join(buildErrs...))
	}

	// xtid is initialised lazily, on the first request that carries an account's
	// cookies (see warmFetcher): logged-out x.com serves a Vite shell without the
	// bundle. The shared-client fetcher below is only the unauthenticated fallback.
	// On failure we degrade — x-client-transaction-id is simply omitted — and the
	// alert fires once per ok->failed transition, never per request.
	mgr := cfg.sharedXtid
	if mgr == nil {
		fetcher := &bundle.StealthFetcher{Client: bc, UserAgent: defaultUserAgent}
		mgr = xtid.NewManager(fetcher)
		mgr.SetInitFailedHook(func(err error) {
			alertHook("xtid.init_failed", map[string]any{"error": err.Error()})
		})
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

	// Read the guest_id cookie from the stealth client's jar. xtid's warm-page
	// fetch is lazy now (first authenticated request), so it has normally not run
	// yet and the jar is empty here: fall back to a generated id. Log which
	// branch fired so the synthetic id is observable.
	xpffGuestID := bc.GetCookieValue("https://x.com", "guest_id")
	if xpffGuestID == "" {
		xpffGuestID = xpff.GenerateGuestID()
		slog.Info("xpff: guest_id cookie absent at construction, using synthetic GenerateGuestID")
	} else {
		slog.Info("xpff: using server-set guest_id cookie")
	}
	xpffGen := xpff.New(xpffGuestID, defaultUserAgent)

	// Per-account human-pace spacing. ADDITIVE to the anti-fingerprint jitter: it
	// spaces a single account's consecutive requests for stealth, keyed by
	// account (not domain), so a low-frequency caller is never starved by a
	// global gate. The per-account-per-endpoint rate limiter remains the
	// authoritative throughput ceiling; this pace stays well under it.
	accountPacer := buildAccountPacer(cfg)

	c := &Client{
		client:               bc,
		pool:                 p,
		xtidMgr:              mgr,
		xpffGen:              xpffGen,
		cfg:                  cfg,
		nonResponsiveBackoff: nonResponsiveBackoff,
		accountPacer:         accountPacer,
		alertHook:            alertHook,
		metrics:              m,
	}
	if !cfg.skipPoolGauge {
		c.registerPoolGauge(cfg.MeterProvider)
	}

	// Default per-account relogin circuit breaker: a transient login outage must
	// not repeatedly destroy still-valid sessions or storm the WAF-blocked
	// guest-token/login endpoint. A consumer's SetAutoReloginGate (go-social)
	// overrides this — wiring is non-clobbering.
	wireDefaultReloginGate(c)

	for _, acc := range cfg.Accounts {
		if acc.proxyClientFailed {
			continue // disabled above; no transport to log in through
		}
		bc, err := c.clientForAccount(acc)
		if err == nil {
			err = c.loadOrLogin(acc, bc)
		}
		if err != nil {
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
			seedRateLimits(acc) // prime per-endpoint caps with X's measured real limits
			acc.HealthTracker = pool.DefaultHealthTracker()
			p.Add(acc)
		}
	}

	return c, nil
}

// proxyBuildFailure returns a log-safe proxy label and failure reason. The
// proxy string is operator input that may be malformed in credential-bearing
// ways ("user:pw@host:8080" parses as scheme "user" with u.User nil, so
// stealth.MaskProxy returns it unchanged), and go-stealth's build error echoes
// the URL, so neither the string nor err.Error() may ever be emitted. The label
// is scheme://host[:port] from a successful parse with a non-empty host and
// userinfo dropped; anything else is a fixed placeholder. The reason is fixed.
func proxyBuildFailure(proxy string) (label, reason string) {
	const placeholder = "<unparseable proxy URL>"
	reason = "per-account proxy client build failed"
	u, err := url.Parse(proxy)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return placeholder, reason
	}
	return u.Scheme + "://" + u.Host, reason
}

// clientForAccount returns the per-account client if available, otherwise the
// shared client. An account pinned to its own proxy with no per-account client
// never gets the shared one (it may be direct): ErrProxyClientUnavailable.
func (c *Client) clientForAccount(acc *Account) (*stealth.BrowserClient, error) {
	if acc.client != nil {
		return acc.client, nil
	}
	if acc.Proxy != "" {
		return nil, fmt.Errorf("%w: %s", ErrProxyClientUnavailable, acc.Username)
	}
	return c.client, nil
}

// doPoolReq is a helper for doPoolRequest: executes method+payload via doRequestWithBody.
func (c *Client) doPoolReq(bc *stealth.BrowserClient, method, urlStr string, payload []byte, headers map[string]string) ([]byte, map[string]string, int, error) {
	var body io.Reader
	if len(payload) > 0 {
		body = bytes.NewReader(payload)
	}
	return c.doRequestWithBody(bc, method, urlStr, headers, body)
}

// observedPoolReq is doPoolReq plus one twitter.client.request.duration record
// for the attempt. Every HTTP attempt in the pool/POST/guest request paths goes
// through an observed* wrapper, including ct0-recovery and relogin retries.
func (c *Client) observedPoolReq(endpoint string, bc *stealth.BrowserClient, method, urlStr string, payload []byte, headers map[string]string) ([]byte, map[string]string, int, error) {
	start := time.Now()
	body, hdrs, status, err := c.doPoolReq(bc, method, urlStr, payload, headers)
	c.metrics.recordRequest(endpoint, time.Since(start), attemptOutcome(status, body, hdrs, err, bc != c.client))
	return body, hdrs, status, err
}

// observedRequestWithBody is doRequestWithBody plus a duration record.
func (c *Client) observedRequestWithBody(endpoint string, bc *stealth.BrowserClient, method, urlStr string, headers map[string]string, body io.Reader) ([]byte, map[string]string, int, error) {
	start := time.Now()
	respBody, hdrs, status, err := c.doRequestWithBody(bc, method, urlStr, headers, body)
	c.metrics.recordRequest(endpoint, time.Since(start), attemptOutcome(status, respBody, hdrs, err, bc != c.client))
	return respBody, hdrs, status, err
}

// observedRequest is doRequest plus a duration record.
func (c *Client) observedRequest(endpoint string, bc *stealth.BrowserClient, method, urlStr string, headers map[string]string) ([]byte, map[string]string, int, error) {
	return c.observedRequestWithBody(endpoint, bc, method, urlStr, headers, nil)
}

// authTokenCookieRe matches a non-empty auth_token cookie pair.
var authTokenCookieRe = regexp.MustCompile(`(?:^|;\s*)auth_token=[^;\s]+`)

// warmFetcher builds the authenticated warm-page fetcher for xtid from a
// request's own headers: the account's cookie, sent through that account's
// client (bc). Returns nil (unauthenticated fallback) for requests without an
// auth_token cookie. The cookie is never logged.
func warmFetcher(d warmDoer, headers map[string]string) bundle.Fetcher {
	cookie := headers["cookie"]
	if d == nil || !authTokenCookieRe.MatchString(cookie) {
		return nil
	}
	ua := headers["user-agent"]
	if ua == "" {
		ua = defaultUserAgent
	}
	return &bundle.StealthFetcher{Client: d, UserAgent: ua, Cookie: cookie}
}

// warmDoer is the request primitive the warm-page fetcher needs; the account's
// *stealth.BrowserClient in production.
type warmDoer interface {
	DoCtx(ctx context.Context, method, urlStr string, headers map[string]string, body io.Reader) ([]byte, map[string]string, int, error)
}

// warmClient picks the client the warm-page fetch goes through: the request
// account's own (bc), or the test override.
func (c *Client) warmClient(bc *stealth.BrowserClient) warmDoer {
	if c.warmDoerOverride != nil {
		return c.warmDoerOverride
	}
	if bc == nil {
		return nil
	}
	return bc
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
	if txID, txErr := c.xtidMgr.GenerateIDWith(context.Background(), method, urlPath, warmFetcher(c.warmClient(bc), headers)); txErr == nil {
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

	// ProxyConsecFails is the count of consecutive proxy-layer failures for this
	// account. Non-zero while a dead proxy is being backed off; reset to 0 on
	// the next successful HTTP response. Distinguishes a dead proxy from a dead
	// account — the go-twitter #43 incident mis-attributed 5881 proxy failures
	// to the account layer because this distinction was not observable.
	ProxyConsecFails int
	// ProxyBackoffUntil is when the account's proxy becomes eligible again after
	// exponential backoff. Zero when no proxy backoff is in effect.
	ProxyBackoffUntil time.Time
}

// HealthReport returns health stats for all accounts in the pool.
func (c *Client) HealthReport() []AccountHealth {
	items := c.pool.Items()
	report := make([]AccountHealth, 0, len(items))
	for _, acc := range items {
		total, failed, consecFails := acc.Stats()
		acc.mu.Lock()
		proxyConsec := acc.proxyConsecFails
		proxyBackoff := acc.proxyBackoff
		acc.mu.Unlock()
		report = append(report, AccountHealth{
			Username:          acc.Username,
			Active:            acc.IsActive(),
			Total:             total,
			Failed:            failed,
			ConsecFails:       consecFails,
			ProxyConsecFails:  proxyConsec,
			ProxyBackoffUntil: proxyBackoff,
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

// buildAccountPacer constructs the per-account human-pace pacer. Returns nil
// when pacing is disabled. The pacer is keyed by account ID at the call site
// (doPoolRequest, after pool.Next): MinDelay is the hard floor between an
// account's consecutive requests, +RandomDelay makes the realized spacing
// human-variable. This is the single authority for pacer construction —
// NewClient and the unit tests both go through here so the test exercises the
// exact wiring production uses, not a copy.
//
// Unlike the rejected per-DOMAIN gate, per-ACCOUNT keying gives N independent
// pacers: a low-frequency caller (seed) selecting account B is never blocked by
// a high-frequency caller (KOL/VC) that just used account A. The pace stays well
// under the per-account-per-endpoint rate ceiling so it never caps throughput;
// it only smooths a single account's burst rhythm for stealth.
//
// SCOPE: the pacer is wired ONLY at the doPoolRequest pool-rotation site, which
// is the high-volume scrape burst path (Retweeters/Followers/KOL/VC/seed). The
// low-frequency media-upload (doMediaRequest) and login/guest-activate (auth.go
// via DoWithHeaderOrder) sites are INTENTIONALLY not paced -- they are not the
// burst source. They do still count against the same per-account rate-limit
// ceiling, so if a future burst path is added through one of those, route it
// through doPoolRequest (or call c.accountPacer.Wait keyed by account) so it
// inherits the pace.
func buildAccountPacer(cfg ClientConfig) *ratelimit.KeyedPacer {
	if cfg.AccountPaceDisabled {
		return nil
	}
	return ratelimit.NewKeyedPacer(cfg.AccountPaceMin, cfg.AccountPaceRandom)
}
