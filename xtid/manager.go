package xtid

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Manager fetches x.com page/JS and caches the ClientTransaction, auto-refreshing every 30 min.
// Thread-safe. Falls back to old keys on refresh failure.
type Manager struct {
	mu              sync.RWMutex
	ct              *ClientTransaction
	lastRefresh     time.Time
	refreshInterval time.Duration
	client          *http.Client
}

// NewManager creates a new transaction ID manager.
func NewManager() *Manager {
	return &Manager{
		refreshInterval: 30 * time.Minute,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Initialize fetches x.com and the ondemand.s JS file, then builds the ClientTransaction.
// Must be called at least once before GenerateID.
func (m *Manager) Initialize() error {
	homeHTML, err := m.fetchURL("https://x.com")
	if err != nil {
		return fmt.Errorf("fetch x.com: %w", err)
	}

	ondemandURL := getOnDemandFileURL(homeHTML)
	if ondemandURL == "" {
		return fmt.Errorf("ondemand.s URL not found in x.com HTML")
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

	prefix := ct.animationKey
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	slog.Info("xtid: initialized", slog.String("anim_key", prefix+"..."))
	return nil
}

func (m *Manager) fetchURL(url string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
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

