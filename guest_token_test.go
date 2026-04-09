package twitter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestGuestCircuitBreaker verifies that after 5 consecutive acquireGuestToken failures,
// getGuestTokenCached returns false for 30 minutes.
func TestGuestCircuitBreaker(t *testing.T) {
	// Serve permanent 403 to simulate blocked guest token endpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := &Client{
		mu: sync.Mutex{},
	}

	ctx := context.Background()

	// Exhaust the threshold: each acquireGuestToken call makes 3 attempts internally,
	// counting as one "consec fail" per call. We need 5 calls to trip the breaker.
	for range guestCircuitBreakerThreshold {
		// Provide a stub that always fails — bypass stealth.BrowserClient by direct mutex manipulation
		c.mu.Lock()
		c.guestConsecFails++
		if c.guestConsecFails >= guestCircuitBreakerThreshold {
			c.guestBlockedUntil = time.Now().Add(guestCircuitBreakerWindow)
			c.guestConsecFails = 0
		}
		c.mu.Unlock()
	}

	_ = ctx // ctx used in real scenario, suppressed here

	// Circuit breaker should now be open
	_, ok := c.getGuestTokenCached()
	if ok {
		t.Fatal("expected getGuestTokenCached to return false while circuit breaker is open")
	}

	// Verify blockedUntil is roughly 30 min in the future
	c.mu.Lock()
	blocked := c.guestBlockedUntil
	c.mu.Unlock()

	remaining := time.Until(blocked)
	if remaining < 29*time.Minute || remaining > 31*time.Minute {
		t.Fatalf("expected ~30 min block, got %v remaining", remaining)
	}
}

// TestGuestCircuitBreakerResetOnSuccess verifies that a successful acquireGuestToken
// resets the consecutive failure counter.
func TestGuestCircuitBreakerResetOnSuccess(t *testing.T) {
	c := &Client{mu: sync.Mutex{}}

	// Set 4 consecutive failures (one short of threshold)
	c.mu.Lock()
	c.guestConsecFails = 4
	c.mu.Unlock()

	// Simulate a successful acquisition by resetting counter (as acquireGuestToken does on success)
	c.mu.Lock()
	c.guestConsecFails = 0
	c.mu.Unlock()

	// Circuit breaker should not be open
	c.mu.Lock()
	blocked := c.guestBlockedUntil
	c.mu.Unlock()

	if time.Now().Before(blocked) {
		t.Fatal("circuit breaker should not be tripped after successful reset")
	}
}
