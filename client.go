package twitter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/internal/queryid"
	"github.com/anatolykoptev/go-twitter/xpff"
	"github.com/anatolykoptev/go-twitter/xtid"
	"go.opentelemetry.io/otel/metric"
)

// Client is the top-level Twitter scraping client.
type Client struct {
	client               *stealth.BrowserClient
	pool                 *pool.Pool[*Account]
	xtidMgr              *xtid.Manager
	xtidFail             xtidFailureStreak // failure-streak state for the missing-header warning
	xpffGen              *xpff.Generator
	cfg                  ClientConfig
	reloginGate          AutoReloginGate                                  // nil = always allow
	nonResponsiveBackoff pool.BackoffConfig                               // transient-failure backoff (base from cfg, x2, cap 30m)
	accountPacer         *ratelimit.KeyedPacer                            // per-account human-pace spacing (keyed by account, nil = disabled)
	alertHook            func(topic string, payload any)                  // proxy/pool event sink (PoolAlertHook or slog fallback)
	metrics              *clientMetrics                                   // OpenTelemetry instruments (nil-safe)
	poolGaugeReg         metric.Registration                              // pool.accounts callback; unregistered by Close (guarded by mu)
	owned                *clientSet                                       // every BrowserClient this Client built; Close releases their idle connections (nil-safe)
	warmDoerOverride     warmDoer                                         // test seam: replaces the account client for xtid warm-page fetches
	qid                  *queryid.Refresher                               // nil = the process-wide queryId refresher; only tests set it
	requestJitter        *stealth.Jitter                                  // pre-request jitter for pool requests (opt-in); nil = none
	deviceWarn           warnLimiter                                      // rate-limits the both-sources-failed warning of GetDeviceNotifications
	jitterSleep          func(ctx context.Context, d time.Duration) error // test seam: replaces the real sleep in preRequestJitter

	mu                sync.Mutex
	guestToken        string
	guestLimitedUntil time.Time
	guestConsecFails  int
	guestBlockedUntil time.Time
	// flightRelease is the cap-wait broadcast channel: every slot release
	// closes it and installs a fresh one, so a waiter parked on one
	// generation wakes on the NEXT release. Re-fetch before each wait; a
	// release before that re-fetch is caught by the next pick attempt.
	flightRelease chan struct{}

	reloginMu      sync.Mutex
	reloginFlights map[*Account]*reloginFlight
}

// NewClient creates a fully-wired Twitter client.
//
// Optional ClientOptions (WithRequestJitter, WithoutRequestJitter,
// WithAccountPace) adjust the config after it is passed in. Without WithRequestJitter pooled
// requests are sent without a pre-request delay.
func NewClient(cfg ClientConfig, clientOpts ...ClientOption) (*Client, error) {
	for _, o := range clientOpts {
		o(&cfg)
	}
	cfg.defaults()

	for _, acc := range cfg.Accounts {
		acc.active = true
		acc.installRateLimiter(cfg.RateLimit)
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
	bc, err := stealth.NewClient(append(opts, cfg.stealthOpts...)...)
	if err != nil {
		return nil, fmt.Errorf("stealth client: %w", err)
	}
	owned := &clientSet{}
	owned.add(bc)

	// One transport per account. Every account owns its own stealth client, and
	// with it its own cookie jar: a set-cookie on a response to account A can
	// never ride on account B's next request (fhttp appends jar cookies to the
	// explicit cookie header). An account without its own Proxy gets
	// cfg.DefaultProxy, so its egress is unchanged. The shared client above
	// serves only the account-less paths (guest token, warm fetches without an
	// account).
	//
	// A build failure disables only that account (a pooled consumer's shared
	// client must survive one malformed proxy row) and NewClient fails only when
	// no account is usable. Fail-closed is kept: the failed account is marked
	// permanently unselectable (IsActive) and clientForAccount refuses to lend
	// it the shared client, which may be direct (the *WithSocial path sets no
	// DefaultProxy) and would send its auth_token/ct0 from this host's IP.
	//
	// The clients built before an error return below are not released
	// explicitly; they have made no request yet, so they hold no connections
	// and are reclaimed by the GC.
	var buildErrs []error
	usable := 0
	for _, acc := range cfg.Accounts {
		acc.ensureIdentity()
		accClient, accProxy, err := buildAccountClient(cfg, acc)
		if err != nil {
			acc.markProxyClientFailed()
			reason := reportClientBuildFailure(acc, accProxy, m, alertHook)
			buildErrs = append(buildErrs, fmt.Errorf("per-account client for %s: %s", acc.Username, reason))
			continue
		}
		owned.add(accClient)
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
		fetcher := &bundle.StealthFetcher{Client: bc, UserAgent: clientUA(bc)}
		mgr = xtid.NewManager(fetcher)
		if cfg.XtidKeySource != xtid.KeySourceAccountPage {
			mgr.SetKeySource(cfg.XtidKeySource)
		}
		mgr.SetInitFailedHook(func(err error) {
			alertHook("xtid.init_failed", map[string]any{"error": err.Error()})
		})
		mgr.SetWarmPageHook(observeWarmPage)
	}
	if cfg.PoolAlertHook != nil {
		// The queryId refresh is process-wide; only an explicitly supplied hook
		// may own its failure alert (the *WithSocial path supplies none).
		processQID().SetAlertHook(cfg.PoolAlertHook)
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
	xpffGen := xpff.New(xpffGuestID, clientUA(bc))

	// Per-account human-pace spacing. Independent of the opt-in pre-request jitter: it
	// spaces a single account's consecutive requests for stealth, keyed by
	// account (not domain), so a low-frequency caller is never starved by a
	// global gate. The per-account-per-endpoint rate limiter remains the
	// authoritative throughput ceiling; this pace stays well under it.
	accountPacer := cfg.sharedPacer
	if accountPacer == nil {
		accountPacer = buildAccountPacer(cfg)
	}

	c := &Client{
		client:               bc,
		pool:                 p,
		xtidMgr:              mgr,
		xpffGen:              xpffGen,
		cfg:                  cfg,
		nonResponsiveBackoff: nonResponsiveBackoff,
		accountPacer:         accountPacer,
		requestJitter:        cfg.RequestJitter,
		alertHook:            alertHook,
		metrics:              m,
		owned:                owned,
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
			acc.installRateLimiter(cfg.RateLimit)
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

// buildAccountClient builds the account's own stealth client: its Proxy, else
// cfg.DefaultProxy, else direct, with the account's TLS profile. NewClient and
// the lazy path in clientForAccount both build through it, so they cannot
// drift. It returns the proxy URL the client was built for ("" = direct).
func buildAccountClient(cfg ClientConfig, acc *Account) (*stealth.BrowserClient, string, error) {
	proxy := acc.Proxy
	if proxy == "" {
		proxy = cfg.DefaultProxy
	}
	// The caller has completed the identity (ensureIdentity); the account's
	// mutex may be held, so it is read directly.
	opts := []stealth.ClientOption{stealth.WithHeaderOrder(twitterHeaderOrder)}
	if acc.Profile.TLSProfile != "" {
		// Profile and UA go in together, so the client's reported identity (used
		// by the login flow) is exactly what every later request sends.
		id := stealth.BrowserIdentity{BrowserProfile: acc.Profile}
		id.UserAgent = acc.UserAgent
		opts = append(opts, stealth.WithIdentity(id))
	} else {
		// A caller-set UA that no built-in profile presents: keep it as the
		// client's identity over go-stealth's default TLS profile, so the login
		// flow sends the same UA as every later request. An empty profile is
		// never passed to go-stealth.
		opts = append(opts, stealth.WithIdentity(stealth.BrowserIdentity{
			BrowserProfile: stealth.BrowserProfile{UserAgent: acc.UserAgent, TLSProfile: defaultTLSProfile},
		}))
	}
	// An account whose UA no built-in profile presents keeps the client default;
	// an empty profile is never passed to WithProfile.
	if proxy != "" {
		opts = append(opts, stealth.WithProxy(proxy))
	}
	bc, err := stealth.NewClient(append(opts, cfg.stealthOpts...)...)
	return bc, proxy, err
}

// reportClientBuildFailure logs, counts and alerts one failed per-account
// client build and returns the fixed, log-safe reason. The alert topic is API
// and keeps its name; the payload says "direct" when the client had no proxy.
// The raw proxy string and the build error never leave: both may carry
// credentials.
func reportClientBuildFailure(acc *Account, proxy string, m *clientMetrics, alertHook func(string, any)) string {
	label, reason := "direct", "per-account client build failed"
	if proxy != "" {
		label, reason = proxyBuildFailure(proxy)
	}
	slog.Warn("per-account client build failed, account disabled",
		slog.String("user", acc.Username), slog.String("proxy", label), slog.String("error", reason))
	m.recordProxyClientFailure()
	if alertHook != nil {
		alertHook("account.proxy_client_failed", map[string]any{
			"user":  acc.Username,
			"proxy": label,
			"error": reason,
		})
	}
	return reason
}

// clientForAccount returns the account's own client, building it on first use
// for an account NewClient never saw (loginOpenAccount accounts, anything added
// through Pool().Add). The build is serialised on the account, so concurrent
// first uses build one client. It never lends the shared client to an account:
// that client's cookie jar also serves the guest paths, and for a proxy-pinned
// account it may be direct. A failed build marks only this account failed and
// returns ErrProxyClientUnavailable. Only the account-less paths use c.client.
func (c *Client) clientForAccount(acc *Account) (*stealth.BrowserClient, error) {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if acc.client != nil {
		return acc.client, nil
	}
	if acc.proxyClientFailed {
		return nil, fmt.Errorf("%w: %s", ErrProxyClientUnavailable, acc.Username)
	}
	acc.ensureIdentityLocked()
	bc, proxy, err := buildAccountClient(c.cfg, acc)
	if err != nil {
		acc.proxyClientFailed = true
		reportClientBuildFailure(acc, proxy, c.metrics, c.alertHook)
		return nil, fmt.Errorf("%w: %s", ErrProxyClientUnavailable, acc.Username)
	}
	c.owned.add(bc)
	acc.client = bc
	return bc, nil
}

// clientSet is the BrowserClients one Client built. Close walks it instead of
// reading Account.client: an Account handed to two Clients carries whichever
// client was stored last, and a caller-owned client must never be released
// here. A nil *clientSet is valid and empty, so a bare &Client{} never panics.
type clientSet struct {
	mu  sync.Mutex
	set []*stealth.BrowserClient
}

func (s *clientSet) add(bc *stealth.BrowserClient) {
	if s == nil || bc == nil {
		return
	}
	s.mu.Lock()
	s.set = append(s.set, bc)
	s.mu.Unlock()
}

// closeIdle closes the idle connections of every listed client. It never closes
// a client: a background xtid or queryId refresh may still be using one, and
// CloseIdleConnections is safe beside a running request, repeatedly, and after
// Close.
func (s *clientSet) closeIdle() {
	if s == nil {
		return
	}
	s.mu.Lock()
	snap := append([]*stealth.BrowserClient(nil), s.set...)
	s.mu.Unlock()
	for _, bc := range snap {
		bc.CloseIdleConnections()
	}
}

// usesOwnProxy reports whether an attempt served by acc went through the
// account's own pinned proxy. Every account owns a client, so client identity
// no longer says anything about the proxy; the account's Proxy field does, the
// same gate attributeTransportError and the proxy-backoff paths use. nil (a
// guest-token attempt) and an account on the shared DefaultProxy are not
// proxied in this sense: their transport failures are not proxy-attributable.
func usesOwnProxy(acc *Account) bool {
	return acc != nil && acc.Proxy != ""
}

// flightCap returns the per-account in-flight cap (ClientConfig
// .MaxInFlightPerAccount); <= 0 disables it. NewClient turns the zero value
// into the default, so <= 0 here means either explicitly disabled or a
// hand-built client (tests), which then runs uncapped.
func (c *Client) flightCap() int {
	if c == nil {
		return 0
	}
	return c.cfg.MaxInFlightPerAccount
}

// flightChan returns the current release-broadcast generation. A cap waiter
// grabs it BEFORE re-checking eligibility, so a release landing between the
// check and the select still closes exactly the generation it waits on.
func (c *Client) flightChan() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.flightRelease == nil {
		c.flightRelease = make(chan struct{})
	}
	return c.flightRelease
}

// flightReleased drops one in-flight slot on acc and wakes every cap waiter
// parked on the broadcast channel. A no-op when the cap is disabled (no slot
// was ever taken, so there is nothing to return).
func (c *Client) flightReleased(acc *Account) {
	if c.flightCap() <= 0 {
		return
	}
	acc.releaseFlight()
	c.mu.Lock()
	if c.flightRelease != nil {
		close(c.flightRelease)
		c.flightRelease = make(chan struct{})
	}
	c.mu.Unlock()
}

// acquireFlight blocks until the pinned account drops below its in-flight
// cap, or the caller's ctx ends. The pinned paths (doPOST, media) have no
// account to rotate to, so waiting — not failing — is the only option; the
// wait is ctx-bounded like the pool's own.
func (c *Client) acquireFlight(ctx context.Context, acc *Account) error {
	limit := c.flightCap()
	if limit <= 0 {
		return nil
	}
	for {
		ch := c.flightChan()
		if acc.tryAcquireFlight(limit) {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// doPoolReq is a helper for doPoolRequest: executes method+payload via doRequestWithBody.
func (c *Client) doPoolReq(ctx context.Context, acc *Account, bc *stealth.BrowserClient, method, urlStr string, payload []byte, headers map[string]string) ([]byte, map[string]string, int, error) {
	var body io.Reader
	if len(payload) > 0 {
		body = bytes.NewReader(payload)
	}
	return c.doRequestWithBody(ctx, acc, bc, method, urlStr, headers, body)
}

// observedPoolReq is doPoolReq plus one twitter.client.request.duration record
// for the attempt. Every HTTP attempt in the pool/POST/guest request paths goes
// through an observed* wrapper, including ct0-recovery and relogin retries.
// acc is the pooled account that served the attempt; it feeds the OnRequest
// hook (nil = the request carried no account).
func (c *Client) observedPoolReq(ctx context.Context, acc *Account, endpoint string, bc *stealth.BrowserClient, method, urlStr string, payload []byte, headers map[string]string) ([]byte, map[string]string, int, error) {
	start := time.Now()
	body, hdrs, status, err := c.doPoolReq(ctx, acc, bc, method, urlStr, payload, headers)
	currentCall(ctx).done(status, len(body))
	outcome := attemptOutcome(status, body, hdrs, err, usesOwnProxy(acc))
	c.metrics.recordRequest(endpoint, time.Since(start), outcome)
	c.emitRequestEvent(acc, endpoint, status, outcome, hdrs)
	return body, hdrs, status, err
}

// observedRequestWithBody is doRequestWithBody plus a duration record.
// acc semantics match observedPoolReq.
func (c *Client) observedRequestWithBody(ctx context.Context, acc *Account, endpoint string, bc *stealth.BrowserClient, method, urlStr string, headers map[string]string, body io.Reader) ([]byte, map[string]string, int, error) {
	start := time.Now()
	respBody, hdrs, status, err := c.doRequestWithBody(ctx, acc, bc, method, urlStr, headers, body)
	outcome := attemptOutcome(status, respBody, hdrs, err, usesOwnProxy(acc))
	c.metrics.recordRequest(endpoint, time.Since(start), outcome)
	c.emitRequestEvent(acc, endpoint, status, outcome, hdrs)
	return respBody, hdrs, status, err
}

// observedRequest is doRequest plus a duration record. acc semantics match
// observedPoolReq.
func (c *Client) observedRequest(ctx context.Context, acc *Account, endpoint string, bc *stealth.BrowserClient, method, urlStr string, headers map[string]string) ([]byte, map[string]string, int, error) {
	return c.observedRequestWithBody(ctx, acc, endpoint, bc, method, urlStr, headers, nil)
}

// observedMediaReq is doPoolReq plus the request-duration record and the
// OnRequest event — the media-upload twin of observedPoolReq under the fixed
// epUploadMedia endpoint label. The call site stays doPoolReq-shaped so the
// payload handling is byte-for-byte what it was unobserved.
func (c *Client) observedMediaReq(ctx context.Context, acc *Account, bc *stealth.BrowserClient, method, urlStr string, payload []byte, headers map[string]string) ([]byte, map[string]string, int, error) {
	start := time.Now()
	body, hdrs, status, err := c.doPoolReq(ctx, acc, bc, method, urlStr, payload, headers)
	outcome := attemptOutcome(status, body, hdrs, err, usesOwnProxy(acc))
	c.metrics.recordRequest(epUploadMedia, time.Since(start), outcome)
	c.emitRequestEvent(acc, epUploadMedia, status, outcome, hdrs)
	return body, hdrs, status, err
}

// emitRequestEvent fires ClientConfig.OnRequest for one attempt a pooled
// account served. acc nil means the attempt carried no account (guest-token
// fallback) and emits nothing — the hook only ever observes pooled accounts.
// The outcome is the same classification recordRequest just wrote, and the
// rate-limit fields mirror the response headers (unknowns stay at their
// zero/-1 sentinels). Synchronous by design; the hook must not block.
func (c *Client) emitRequestEvent(acc *Account, endpoint string, status int, outcome string, hdrs map[string]string) {
	hook := c.cfg.OnRequest
	if hook == nil || acc == nil {
		return
	}
	obs := parseRateLimitHeaders(hdrs)
	ev := RequestEvent{Username: acc.Username, Endpoint: endpoint, Status: status, Outcome: outcome}
	ev.RateLimit.Limit = obs.limit
	ev.RateLimit.Remaining = obs.remaining
	ev.RateLimit.Reset = obs.resetAt
	hook(ev)
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
func (c *Client) doRequest(ctx context.Context, acc *Account, bc *stealth.BrowserClient, method, urlStr string, headers map[string]string) ([]byte, map[string]string, int, error) {
	return c.doRequestWithBody(ctx, acc, bc, method, urlStr, headers, nil)
}

// doRequestWithBody executes a request with xtid header injection and an optional body.
// The caller's ctx reaches the transport (DoWithHeaderOrderCtx): a cancel or
// deadline aborts the in-flight request instead of parking it until
// go-stealth's internal request timeout fires.
func (c *Client) doRequestWithBody(ctx context.Context, acc *Account, bc *stealth.BrowserClient, method, urlStr string, headers map[string]string, body io.Reader) ([]byte, map[string]string, int, error) {
	currentCall(ctx).headersStart()
	urlPath := urlStr
	if u, parseErr := url.Parse(urlStr); parseErr == nil {
		urlPath = u.Path
	}
	// Content-type describes a body. X's web client sends it on every GraphQL
	// operation (GET included) and on bodies, and on no other bodyless GET.
	if body == nil && method == http.MethodGet && !strings.Contains(urlPath, "/graphql/") {
		delete(headers, "content-type")
	}
	qid := c.qidRefresh()
	wf := qid.Shared(warmFetcher(c.warmClient(bc), headers))
	if txID, txErr := c.xtidMgr.GenerateIDWith(ctx, method, urlPath, wf); txErr == nil {
		headers["x-client-transaction-id"] = txID
		c.xtidFail.onSuccess()
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		// The caller's deadline/cancel ended the cold-start wait (the shared key
		// fetch itself keeps running for later callers). That is the caller's
		// verdict, not a generator failure: do not count it, and do not send a
		// request on a dead ctx.
		return nil, nil, 0, ctxErr
	} else {
		// The request still goes out, without the header. The failure is counted
		// and logged once per streak so a broken generator is visible.
		c.noteXtidFailure(txErr)
	}
	// First/TTL queryId refresh rides the same authenticated fetcher (and, after
	// an xtid init, the page it just fetched); it runs in the background.
	qid.OnAuthedRequest(wf, c.metrics.instruments())

	if xpffVal, xpffErr := c.xpffFor(acc, bc, headers["user-agent"]).Generate(); xpffErr == nil {
		headers["x-xp-forwarded-for"] = xpffVal
	} else {
		slog.Debug("xpff: failed to generate header", slog.Any("error", xpffErr))
	}

	currentCall(ctx).ready()
	return bc.DoWithHeaderOrderCtx(ctx, method, urlStr, headers, body, twitterHeaderOrder)
}

// xpffFor returns the generator for a request: the account's own when the
// request carries one, else the client-wide one (guest paths). The guest id is
// the one in the account's cookie jar once the server has set it, so the value
// agrees with the cookie X sees; before that it derives from the account key,
// stable across restarts and Client instances. ua is the User-Agent the
// request sends, so the value embeds the same identity.
func (c *Client) xpffFor(acc *Account, bc *stealth.BrowserClient, ua string) *xpff.Generator {
	if acc == nil {
		return c.xpffGen
	}
	if ua == "" {
		ua = defaultUserAgent
	}
	guestID := ""
	if bc != nil {
		guestID = bc.GetCookieValue("https://x.com", "guest_id")
	}
	if guestID == "" {
		guestID = xpff.GuestIDFor(accountKey(acc.Username))
	}
	return acc.xpffGenerator(guestID, ua)
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

// HoldAccount benches a pooled account until the given time: the pool pick
// skips it for every endpoint while the account clock is before until, and it
// becomes eligible again afterwards. Returns false when no pool account
// matches username (the match is case-insensitive, like AccountByUsername).
// Re-holding is max-wins: a later call with an earlier until never shortens
// an existing hold — a deliberate early lift goes through ReleaseAccount. The
// hold gates only the pool pick: account-pinned write paths that take the
// account directly (doPOST-backed writes, FollowUser, UploadMedia) do not
// consult it. A hold is not a failure — it applies no health penalty, no
// failure counters and no relogin, so it deliberately bypasses the
// SetActive/reactivateAt cooldown machinery. When every account is held the
// caller gets the same error a fully-spent pool returns today:
// "pool exhausted for <endpoint> (requires auth): all N items are unavailable".
func (c *Client) HoldAccount(username string, until time.Time) bool {
	acc := c.AccountByUsername(username)
	if acc == nil {
		return false
	}
	acc.setHeldUntil(until)
	return true
}

// ReleaseAccount lifts a HoldAccount hold early: the account becomes eligible
// for the next pool pick at once. Returns false when no pool account matches
// username. Releasing an account that was not held is a no-op.
func (c *Client) ReleaseAccount(username string) bool {
	acc := c.AccountByUsername(username)
	if acc == nil {
		return false
	}
	acc.releaseHold()
	return true
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
