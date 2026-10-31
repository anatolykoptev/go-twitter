package xtid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"golang.org/x/sync/singleflight"
)

const (
	// warmPageURL is the public x.com landing used by the unauthenticated attempt.
	// Logged-out, x.com now serves a Vite shell without the bundle (see
	// bundle.ErrLoggedOutShell), so this is only the fallback attempt.
	warmPageURL = "https://x.com"
	// authedWarmPageURL is the page fetched with an account's cookies. A profile
	// page is what twscrape (xclid.py) uses; it carries the verification meta, the
	// loading-animation SVGs and the x-web script links.
	authedWarmPageURL = "https://x.com/tesla"
	// AuthedWarmPageURL is authedWarmPageURL for consumers that share the page
	// fetch (see SetWarmPageHook).
	AuthedWarmPageURL = authedWarmPageURL

	// initTimeout bounds one shared init attempt (it outlives any single caller).
	initTimeout = 90 * time.Second
	// Negative-cache backoff after an init failure: base, doubling, capped at max.
	defaultBackoffBase = 60 * time.Second
	defaultBackoffMax  = 15 * time.Minute
)

// ErrUnavailable is returned by GenerateID when no ClientTransaction could be
// built and the negative cache forbids another attempt yet.
var ErrUnavailable = errors.New("xtid: transaction id unavailable")

// Manager fetches x.com page/JS and caches the ClientTransaction, auto-refreshing
// every 30 min. Thread-safe. Falls back to old keys on refresh failure. Init is
// lazy (first GenerateIDWith), single-flighted, and negatively cached.
type Manager struct {
	mu              sync.RWMutex
	ct              *ClientTransaction
	lastRefresh     time.Time
	refreshInterval time.Duration
	fetcher         bundle.Fetcher
	// cacheDir overrides the bundle chunk-map disk cache location; empty uses
	// the bundle package default. Set in tests for isolation.
	cacheDir string

	// Init-failure state, guarded by mu.
	sf singleflight.Group
	// fails is the negative cache, one entry per attempt kind: a failed guest
	// (unauthenticated) init must never lock out an authenticated one, which
	// fetches a different page and can succeed where the guest shell cannot.
	fails        [kindCount]failState
	failing      bool // alert dedupe: true between an ok->failed transition and the next success
	onInitFailed func(error)
	onWarmPage   func(pageURL string, page []byte)

	now         func() time.Time
	backoffBase time.Duration
	backoffMax  time.Duration
}

// attemptKind separates guest and authenticated init attempts.
type attemptKind int

const (
	kindGuest attemptKind = iota
	kindAuth
	kindCount
)

func kindOf(auth bundle.Fetcher) attemptKind {
	if auth != nil {
		return kindAuth
	}
	return kindGuest
}

// failState is one kind's negative-cache entry.
type failState struct {
	until  time.Time // no attempt of this kind before this instant
	streak int
	err    error
}

// NewManager creates a new transaction ID manager backed by the given Fetcher.
// The runtime passes a stealth-backed Fetcher; tests pass a stub.
func NewManager(f bundle.Fetcher) *Manager {
	return &Manager{
		refreshInterval: 30 * time.Minute,
		fetcher:         f,
		now:             time.Now,
		backoffBase:     defaultBackoffBase,
		backoffMax:      defaultBackoffMax,
	}
}

// SetRefreshInterval overrides how long a built ClientTransaction is reused
// before the next call re-initialises it (default 30 minutes). A non-positive
// d is ignored.
func (m *Manager) SetRefreshInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.Lock()
	m.refreshInterval = d
	m.mu.Unlock()
}

// SetInitFailedHook registers fn to be called once per ok->failed transition
// (never per request). It runs outside the manager's locks.
func (m *Manager) SetInitFailedHook(fn func(error)) {
	m.mu.Lock()
	m.onInitFailed = fn
	m.mu.Unlock()
}

// SetWarmPageHook registers fn to receive every successfully fetched
// AUTHENTICATED warm page (pageURL == AuthedWarmPageURL) right after the fetch,
// before the indices file is located — so a consumer that needs the same page
// (the runtime queryId refresh) shares this fetch instead of repeating it. fn
// runs synchronously outside the manager's locks and must not retain or mutate
// page.
func (m *Manager) SetWarmPageHook(fn func(pageURL string, page []byte)) {
	m.mu.Lock()
	m.onWarmPage = fn
	m.mu.Unlock()
}

// Initialize runs one unauthenticated init attempt synchronously, bypassing the
// negative cache but recording its outcome. GenerateID/GenerateIDWith call init
// lazily; this is for callers and tests that want it eagerly.
func (m *Manager) Initialize() error {
	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	err := m.initialize(ctx, nil)
	m.recordResult(kindGuest, err)
	return err
}

// initialize builds the ClientTransaction. With a non-nil auth fetcher (carrying
// an account's cookies) it is tried first against the authenticated warm page;
// the unauthenticated fetcher is the fallback attempt.
func (m *Manager) initialize(ctx context.Context, auth bundle.Fetcher) error {
	if auth == nil {
		return m.initFrom(ctx, retryFetcher{m.fetcher}, warmPageURL)
	}
	authErr := m.initFrom(ctx, retryFetcher{auth}, authedWarmPageURL)
	if authErr == nil {
		return nil
	}
	// Only a logged-out shell (the account's cookies were not honoured) is worth
	// a guest retry; any other failure would hit the same wall unauthenticated
	// and just doubles the requests toward x.com.
	if !errors.Is(authErr, bundle.ErrLoggedOutShell) {
		return authErr
	}
	if err := m.initFrom(ctx, retryFetcher{m.fetcher}, warmPageURL); err != nil {
		return fmt.Errorf("%w (unauthenticated fallback: %v)", authErr, err)
	}
	return nil
}

// initFrom fetches the warm page through f, locates the indices file, fetches it
// and builds the ClientTransaction.
func (m *Manager) initFrom(ctx context.Context, f bundle.Fetcher, pageURL string) error {
	page, err := bundle.FetchPage(ctx, f, pageURL)
	if err != nil {
		return fmt.Errorf("fetch warm page: %w", err)
	}
	if pageURL == authedWarmPageURL {
		m.mu.RLock()
		hook := m.onWarmPage
		m.mu.RUnlock()
		if hook != nil {
			hook(pageURL, page)
		}
	}
	homeHTML := string(page)

	indicesURL, err := m.locateIndices(ctx, f, homeHTML, pageURL)
	if err != nil {
		return err
	}

	indicesJS, err := f.Fetch(ctx, indicesURL)
	if err != nil {
		return fmt.Errorf("fetch indices file: %w", err)
	}

	ct, err := newClientTransaction(homeHTML, string(indicesJS))
	if err != nil {
		return fmt.Errorf("build client transaction: %w", err)
	}

	m.mu.Lock()
	m.ct = ct
	m.lastRefresh = m.now()
	m.mu.Unlock()

	logInitialized(ct)
	return nil
}

// locateIndices resolves the URL of the file holding the key-byte indices
// (ondemand.s in the legacy webpack build, sign.o-* in the Vite build).
//
// Order: the legacy direct-embed fast-path (`"ondemand.s":"<hash>"`); then, for a
// Vite page (x-web script links), a bounded scan of those chunks; otherwise the
// bundle core's webpack chunk-map lookup. A Vite page never falls through to the
// chunk-map path — it has none, and that path refetches more warm pages.
func (m *Manager) locateIndices(ctx context.Context, f bundle.Fetcher, homeHTML, pageURL string) (string, error) {
	if legacy := onDemandLegacyURL(homeHTML); legacy != "" {
		return legacy, nil
	}
	if scripts := bundle.ViteScripts(homeHTML, pageURL); len(scripts) > 0 {
		if bundle.IsLoggedOutShell(scripts) {
			return "", bundle.ErrLoggedOutShell
		}
		return bundle.FindIndicesURL(ctx, f, scripts, bundle.DefaultScanConcurrency, bundle.DefaultScanMax)
	}
	return m.locateOnDemand(ctx, f)
}

// locateOnDemand resolves the ondemand.s bundle URL via the webpack chunk map
// (chunkID by name -> hash -> URL).
func (m *Manager) locateOnDemand(ctx context.Context, f bundle.Fetcher) (string, error) {
	cm, err := bundle.Build(ctx, f, bundle.Options{CacheDir: m.cacheDir})
	if err != nil {
		return "", fmt.Errorf("build chunk map: %w", err)
	}
	id, ok := cm.ChunkIDByName("ondemand.s")
	if !ok {
		return "", fmt.Errorf("ondemand.s chunk not found in chunk map")
	}
	bundleURL, ok := cm.BundleURL("ondemand.s")
	if !ok {
		return "", fmt.Errorf("ondemand.s hash missing for chunk %s", id)
	}
	return bundleURL, nil
}

func logInitialized(ct *ClientTransaction) {
	prefix := ct.animationKey
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	slog.Info("xtid: initialized",
		slog.String("anim_key", prefix+"..."),
		slog.String("sample_key", "xtid_init"))
}

// fetchMaxAttempts is the number of attempts for transient network failures.
const fetchMaxAttempts = 3

// fetchBackoffBase is the initial backoff between retry attempts.
const fetchBackoffBase = 500 * time.Millisecond

// retryFetcher wraps a Fetcher with retry/backoff on transient failures; 4xx
// responses are permanent and not retried. It forwards PostForm without retry.
type retryFetcher struct{ inner bundle.Fetcher }

func (r retryFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= fetchMaxAttempts; attempt++ {
		body, err := r.inner.Fetch(ctx, rawURL)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if isPermanentFetchErr(err) || attempt == fetchMaxAttempts || ctx.Err() != nil {
			break
		}
		slog.Warn("xtid: fetch retry",
			slog.String("url", rawURL),
			slog.Int("attempt", attempt),
			slog.Any("error", err))
		// Exponential backoff with ±30% jitter via go-kit/pacing (through
		// stealth.BackoffConfig, which delegates to pacing.ExponentialBackoff).
		delay := stealth.BackoffConfig{
			InitialWait: fetchBackoffBase,
			MaxWait:     10 * time.Second,
			Multiplier:  2.0,
			JitterPct:   0.3,
		}.Duration(attempt - 1)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// PostForm forwards to the inner fetcher when it can POST.
func (r retryFetcher) PostForm(ctx context.Context, rawURL string, form url.Values) ([]byte, error) {
	p, ok := r.inner.(bundle.Poster)
	if !ok {
		return nil, errors.New("xtid: fetcher cannot POST")
	}
	return p.PostForm(ctx, rawURL, form)
}

// isPermanentFetchErr returns true for errors that should not be retried (HTTP 4xx).
// Network errors, timeouts, and 5xx are retried. Matches anywhere in the message
// because Fetcher impls format the status as a suffix ("... HTTP 403").
func isPermanentFetchErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, code := range []string{"HTTP 400", "HTTP 401", "HTTP 403", "HTTP 404", "HTTP 410", "HTTP 451"} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

// GenerateID returns a new x-client-transaction-id for the given HTTP method and
// URL path, initialising unauthenticated if needed. See GenerateIDWith.
func (m *Manager) GenerateID(method, path string) (string, error) {
	return m.GenerateIDWith(context.Background(), method, path, nil)
}

// GenerateIDWith is GenerateID with an optional auth fetcher (an account's
// cookies through that account's client) used when an init/refresh is needed.
// Concurrent callers share one init attempt; after a failure no new attempt is
// made until the backoff expires, and callers get ErrUnavailable (or the stale
// keys, when a previous init succeeded) immediately. Only the very first init
// blocks callers; a periodic refresh runs in the background while the previous
// keys keep serving.
func (m *Manager) GenerateIDWith(ctx context.Context, method, path string, auth bundle.Fetcher) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	kind := kindOf(auth)
	if m.needsInit() {
		if m.hasKeys() {
			// Stale but usable keys: answer now, refresh in the background so the
			// request path never waits on a page fetch + chunk scan.
			m.startInit(kind, auth)
		} else if err := m.awaitInit(ctx, kind, auth); err != nil {
			return "", err
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.ct == nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, m.lastErrLocked(kind))
	}
	return m.ct.GenerateID(method, path), nil
}

// lastErrLocked returns the most relevant recorded init error; m.mu must be held.
func (m *Manager) lastErrLocked(kind attemptKind) error {
	if err := m.fails[kind].err; err != nil {
		return err
	}
	for _, f := range m.fails {
		if f.err != nil {
			return f.err
		}
	}
	return nil
}

func (m *Manager) needsInit() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ct == nil || m.now().Sub(m.lastRefresh) > m.refreshInterval
}

// hasKeys reports whether a ClientTransaction was ever built; it is never reset.
func (m *Manager) hasKeys() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ct != nil
}

func (m *Manager) backoffActive(kind attemptKind) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.now().Before(m.fails[kind].until)
}

// startInit starts (or joins) one init attempt of the given kind unless that
// kind's negative cache forbids it, and returns the channel that delivers its
// result (nil when no attempt was started). The flight runs on its own
// background context, so no caller's cancellation can abort it.
func (m *Manager) startInit(kind attemptKind, auth bundle.Fetcher) <-chan singleflight.Result {
	if m.backoffActive(kind) {
		return nil
	}
	return m.sf.DoChan(fmt.Sprintf("init-%d", kind), func() (any, error) {
		// Re-check inside the flight: a caller that passed the check above just as
		// the previous flight finished must not start a second attempt.
		if !m.needsInit() || m.backoffActive(kind) {
			return nil, nil
		}
		ictx, cancel := context.WithTimeout(context.Background(), initTimeout)
		defer cancel()
		err := m.initialize(ictx, auth)
		m.recordResult(kind, err)
		return nil, err
	})
}

// awaitInit runs (or joins) one init attempt and waits for it. A waiter's ctx
// only bounds how long it waits.
func (m *Manager) awaitInit(ctx context.Context, kind attemptKind, auth bundle.Fetcher) error {
	ch := m.startInit(kind, auth)
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recordResult updates the negative cache and fires the failure alert once per
// ok->failed transition.
func (m *Manager) recordResult(kind attemptKind, err error) {
	m.mu.Lock()
	if err == nil {
		wasFailing := m.failing
		m.failing, m.fails[kind] = false, failState{}
		m.mu.Unlock()
		if wasFailing {
			slog.Info("xtid: recovered")
		}
		return
	}
	f := &m.fails[kind]
	f.streak++
	wait := m.backoffBase
	for i := 1; i < f.streak && wait < m.backoffMax; i++ {
		wait *= 2
	}
	if wait > m.backoffMax {
		wait = m.backoffMax
	}
	f.until = m.now().Add(wait)
	f.err = err
	transition := !m.failing
	m.failing = true
	hasKeys := m.ct != nil
	hook := m.onInitFailed
	m.mu.Unlock()

	if !transition {
		return
	}
	slog.Warn("xtid: init failed, x-client-transaction-id will be missing or stale",
		slog.Any("error", err), slog.Bool("stale_keys", hasKeys), slog.Duration("retry_in", wait),
		slog.Bool("authenticated", kind == kindAuth))
	if hook != nil {
		hook(err)
	}
}
