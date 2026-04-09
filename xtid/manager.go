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
	guestID         string
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
	homeHTML, guestID, err := m.fetchHome()
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
	if guestID != "" {
		m.guestID = guestID
	}
	m.lastRefresh = time.Now()
	m.mu.Unlock()

	prefix := ct.animationKey
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	slog.Info("xtid: initialized",
		slog.String("anim_key", prefix+"..."),
		slog.String("sample_key", "xtid_init"))
	return nil
}

// GuestID returns the guest_id extracted from x.com set-cookie headers.
func (m *Manager) GuestID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.guestID
}

// fetchHome fetches x.com and extracts the guest_id from set-cookie headers.
func (m *Manager) fetchHome() (html, guestID string, err error) {
	req, err := http.NewRequest("GET", "https://x.com", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	for _, c := range resp.Cookies() {
		if c.Name == "guest_id" && c.Value != "" {
			guestID = c.Value
			break
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	return string(body), guestID, nil
}

// fetchMaxAttempts is the number of attempts for transient network failures.
const fetchMaxAttempts = 3

// fetchBackoffBase is the initial backoff between retry attempts.
const fetchBackoffBase = 500 * time.Millisecond

func (m *Manager) fetchURL(url string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= fetchMaxAttempts; attempt++ {
		body, err := m.fetchOnce(url)
		if err == nil {
			return body, nil
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

func (m *Manager) fetchOnce(url string) (string, error) {
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

// isPermanentFetchErr returns true for errors that should not be retried (HTTP 4xx).
// Network errors, timeouts, and 5xx are retried.
func isPermanentFetchErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, code := range []string{"HTTP 400", "HTTP 401", "HTTP 403", "HTTP 404", "HTTP 410", "HTTP 451"} {
		if len(msg) >= len(code) && msg[:len(code)] == code {
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
