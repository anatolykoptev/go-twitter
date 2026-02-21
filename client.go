package twitter

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/xtid"
)

// Client is the top-level Twitter scraping client.
type Client struct {
	client  *stealth.BrowserClient
	pool    *pool.Pool[*Account]
	xtidMgr *xtid.Manager
	cfg     ClientConfig

	mu                sync.Mutex
	guestToken        string
	guestLimitedUntil time.Time
}

// NewClient creates a fully-wired Twitter client.
func NewClient(cfg ClientConfig) (*Client, error) {
	cfg.defaults()

	for _, acc := range cfg.Accounts {
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

	mgr := xtid.NewManager()
	if err := mgr.Initialize(); err != nil {
		slog.Warn("xtid: init failed, x-client-transaction-id will be missing", slog.Any("error", err))
	}

	poolCfg := pool.Config{
		AlertHook: func(topic string, payload any) {
			slog.Warn("pool alert", slog.String("topic", topic), slog.Any("payload", payload))
		},
		ProxyBackoff: pool.BackoffConfig{
			InitialWait: cfg.ProxyBackoffInitial,
			MaxWait:     cfg.ProxyBackoffMax,
			Multiplier:  2.0,
			JitterPct:   0.3,
		},
	}
	p := pool.New(cfg.Accounts, poolCfg)

	c := &Client{
		client:  bc,
		pool:    p,
		xtidMgr: mgr,
		cfg:     cfg,
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

// doRequest executes a request with xtid header injection.
func (c *Client) doRequest(bc *stealth.BrowserClient, method, urlStr string, headers map[string]string) ([]byte, map[string]string, int, error) {
	urlPath := urlStr
	if u, parseErr := url.Parse(urlStr); parseErr == nil {
		urlPath = u.Path
	}
	if txID, txErr := c.xtidMgr.GenerateID(method, urlPath); txErr == nil {
		headers["x-client-transaction-id"] = txID
	} else {
		slog.Debug("xtid: failed to generate transaction id", slog.Any("error", txErr))
	}

	return bc.DoWithHeaderOrder(method, urlStr, headers, nil, twitterHeaderOrder)
}

// Pool returns the underlying account pool.
func (c *Client) Pool() *pool.Pool[*Account] {
	return c.pool
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
func (c *Client) getGuestTokenCached() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guestToken == "" || time.Now().Before(c.guestLimitedUntil) {
		return "", false
	}
	return c.guestToken, true
}
