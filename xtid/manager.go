package xtid

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
)

// warmPageURL is the public x.com landing fetched for the twitter-site-verification
// meta tag and the loading-animation SVGs. Unauthenticated/guest is fine — the
// warm page is public HTML.
const warmPageURL = "https://x.com"

// Manager fetches x.com page/JS and caches the ClientTransaction, auto-refreshing every 30 min.
// Thread-safe. Falls back to old keys on refresh failure.
type Manager struct {
	mu              sync.RWMutex
	ct              *ClientTransaction
	lastRefresh     time.Time
	refreshInterval time.Duration
	fetcher         bundle.Fetcher
	// cacheDir overrides the bundle chunk-map disk cache location; empty uses
	// the bundle package default. Set in tests for isolation.
	cacheDir string
}

// NewManager creates a new transaction ID manager backed by the given Fetcher.
// The runtime passes a stealth-backed Fetcher; tests pass a stub.
func NewManager(f bundle.Fetcher) *Manager {
	return &Manager{
		refreshInterval: 30 * time.Minute,
		fetcher:         f,
	}
}

// Initialize fetches the warm page, locates ondemand.s, fetches it, and builds the
// ClientTransaction. Must be called at least once before GenerateID.
func (m *Manager) Initialize() error {
	ctx := context.Background()

	homeHTML, err := m.fetchURL(warmPageURL)
	if err != nil {
		return fmt.Errorf("fetch warm page: %w", err)
	}

	ondemandURL, err := m.locateOnDemand(ctx, homeHTML)
	if err != nil {
		return err
	}

	ondemandJS, err := m.fetchURL(ondemandURL)
	if err != nil {
		return fmt.Errorf("fetch ondemand.s: %w", err)
	}

	ct, err := newClientTransaction(homeHTML, ondemandJS)
	if err != nil {
		return fmt.Errorf("build client transaction: %w", err)
	}

	m.mu.Lock()
	m.ct = ct
	m.lastRefresh = time.Now()
	m.mu.Unlock()

	logInitialized(ct)
	return nil
}

// locateOnDemand resolves the ondemand.s bundle URL.
//
// It prefers the legacy direct-embed fast-path (`"ondemand.s":"<hash>"`) that
// still appears on some x.com snapshots — those carry no webpack chunk map to
// walk, so the bundle-core two-step would miss them. Otherwise it falls back to
// the bundle core's chunk-map lookup (chunkID by name -> hash -> URL).
//
// The webpack-location logic that used to live in xtid/parser.go now belongs to
// internal/bundle; xtid only chooses between the legacy fast-path and the core.
func (m *Manager) locateOnDemand(ctx context.Context, homeHTML string) (string, error) {
	if url := onDemandLegacyURL(homeHTML); url != "" {
		return url, nil
	}

	cm, err := bundle.Build(ctx, m.fetcher, bundle.Options{CacheDir: m.cacheDir})
	if err != nil {
		return "", fmt.Errorf("build chunk map: %w", err)
	}
	id, ok := cm.ChunkIDByName("ondemand.s")
	if !ok {
		return "", fmt.Errorf("ondemand.s chunk not found in chunk map")
	}
	url, ok := cm.BundleURL("ondemand.s")
	if !ok {
		return "", fmt.Errorf("ondemand.s hash missing for chunk %s", id)
	}
	return url, nil
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

// fetchURL fetches a URL through the injected Fetcher with retry/backoff. 4xx
// responses are treated as permanent and not retried.
func (m *Manager) fetchURL(url string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= fetchMaxAttempts; attempt++ {
		body, err := m.fetcher.Fetch(context.Background(), url)
		if err == nil {
			return string(body), nil
		}
		lastErr = err
		if isPermanentFetchErr(err) || attempt == fetchMaxAttempts {
			break
		}
		slog.Warn("xtid: fetch retry",
			slog.String("url", url),
			slog.Int("attempt", attempt),
			slog.Any("error", err))
		time.Sleep(fetchBackoffBase * time.Duration(1<<(attempt-1)))
	}
	return "", lastErr
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

// GenerateID returns a new x-client-transaction-id for the given HTTP method and URL path.
// Auto-refreshes keys if they are older than refreshInterval.
func (m *Manager) GenerateID(method, path string) (string, error) {
	m.mu.RLock()
	needRefresh := m.ct == nil || time.Since(m.lastRefresh) > m.refreshInterval
	m.mu.RUnlock()

	if needRefresh {
		if err := m.Initialize(); err != nil {
			m.mu.RLock()
			hasOld := m.ct != nil
			m.mu.RUnlock()
			if !hasOld {
				return "", fmt.Errorf("xtid init failed: %w", err)
			}
			slog.Warn("xtid: refresh failed, using stale keys", slog.Any("error", err))
		}
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.ct == nil {
		return "", fmt.Errorf("xtid not initialized")
	}
	return m.ct.GenerateID(method, path), nil
}
