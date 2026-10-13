package xtid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"golang.org/x/sync/singleflight"
)

const (
	// warmPageURL is the public x.com landing fetched for the twitter-site-verification
	// meta tag and the loading-animation SVGs.
	warmPageURL = "https://x.com"

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
	sf           singleflight.Group
	failUntil    time.Time // no init attempt before this instant
	failStreak   int
	failing      bool // true between an ok->failed transition and the next success
	lastErr      error
	onInitFailed func(error)

	now         func() time.Time
	backoffBase time.Duration
	backoffMax  time.Duration
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

// SetInitFailedHook registers fn to be called once per ok->failed transition
// (never per request). It runs outside the manager's locks.
func (m *Manager) SetInitFailedHook(fn func(error)) {
	m.mu.Lock()
	m.onInitFailed = fn
	m.mu.Unlock()
}

// Initialize runs one unauthenticated init attempt synchronously, bypassing the
// negative cache but recording its outcome. GenerateID/GenerateIDWith call init
// lazily; this is for callers and tests that want it eagerly.
func (m *Manager) Initialize() error {
	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	err := m.initialize(ctx)
	m.recordResult(err)
	return err
}

// initialize builds the ClientTransaction from the unauthenticated warm page.
func (m *Manager) initialize(ctx context.Context) error {
	return m.initFrom(ctx, retryFetcher{m.fetcher}, warmPageURL)
}

// initFrom fetches the warm page through f, locates the indices file, fetches it
// and builds the ClientTransaction.
func (m *Manager) initFrom(ctx context.Context, f bundle.Fetcher, pageURL string) error {
	page, err := f.Fetch(ctx, pageURL)
	if err != nil {
		return fmt.Errorf("fetch warm page: %w", err)
	}
	homeHTML := string(page)

	indicesURL, err := m.locateOnDemand(ctx, f, homeHTML)
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

// locateOnDemand resolves the ondemand.s bundle URL. It prefers the legacy
// direct-embed fast-path (`"ondemand.s":"<hash>"`), otherwise the bundle core's
// webpack chunk-map lookup (chunkID by name -> hash -> URL).
func (m *Manager) locateOnDemand(ctx context.Context, f bundle.Fetcher, homeHTML string) (string, error) {
	if legacy := onDemandLegacyURL(homeHTML); legacy != "" {
		return legacy, nil
	}
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
// responses are permanent and not retried.
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
// URL path, initialising lazily. Concurrent callers share one init attempt; after
// a failure no new attempt is made until the backoff expires, and callers get
// ErrUnavailable (or the stale keys, when a previous init succeeded) immediately.
func (m *Manager) GenerateID(method, path string) (string, error) {
	if m.needsInit() {
		m.ensureInit()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.ct == nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, m.lastErr)
	}
	return m.ct.GenerateID(method, path), nil
}

func (m *Manager) needsInit() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ct == nil || m.now().Sub(m.lastRefresh) > m.refreshInterval
}

func (m *Manager) backoffActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.now().Before(m.failUntil)
}

// ensureInit runs (or joins) one init attempt unless the negative cache forbids it.
func (m *Manager) ensureInit() {
	if m.backoffActive() {
		return
	}
	_, _, _ = m.sf.Do("init", func() (any, error) {
		// Re-check inside the flight: a caller that passed the check above just as
		// the previous flight finished must not start a second attempt.
		if !m.needsInit() || m.backoffActive() {
			return nil, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
		defer cancel()
		err := m.initialize(ctx)
		m.recordResult(err)
		return nil, err
	})
}

// recordResult updates the negative cache and fires the failure alert once per
// ok->failed transition.
func (m *Manager) recordResult(err error) {
	m.mu.Lock()
	if err == nil {
		wasFailing := m.failing
		m.failing, m.failStreak, m.failUntil, m.lastErr = false, 0, time.Time{}, nil
		m.mu.Unlock()
		if wasFailing {
			slog.Info("xtid: recovered")
		}
		return
	}
	m.failStreak++
	wait := m.backoffBase
	for i := 1; i < m.failStreak && wait < m.backoffMax; i++ {
		wait *= 2
	}
	if wait > m.backoffMax {
		wait = m.backoffMax
	}
	m.failUntil = m.now().Add(wait)
	m.lastErr = err
	transition := !m.failing
	m.failing = true
	hasKeys := m.ct != nil
	hook := m.onInitFailed
	m.mu.Unlock()

	if !transition {
		return
	}
	slog.Warn("xtid: init failed, x-client-transaction-id will be missing or stale",
		slog.Any("error", err), slog.Bool("stale_keys", hasKeys), slog.Duration("retry_in", wait))
	if hook != nil {
		hook(err)
	}
}
