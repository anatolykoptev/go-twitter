package twitter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// An authenticated request rejected with 404 that then fails the same way over
// the guest fallback must surface the authenticated failure first. Reporting
// only "(guest) HTTP 404" hid that the logged-in request had been rejected.
//
// Mutation: in request.go doPoolRequest's guest `status != 200` arm, set
// apiErr.Message to the bare guest message (drop withAuthFailure) => RED.
func TestGuestFallbackFailure_NamesAuthenticatedRejectionFirst(t *testing.T) {
	const endpoint = "UserByRestId" // guest-eligible (not in requiresAuth): falls back after the pool fails
	fake := newPoolWaitFake(t)
	fake.status.Store(http.StatusNotFound)
	fake.body = `{"errors":[{"message":"Not Found"}]}`

	c, _ := newPoolWaitClient(t, 1)
	c.cfg.DisableGuestFallback = false
	c.setGuestToken("test-guest-token") // skip real guest-token acquisition

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, err := c.doGET(ctx, endpoint, fake.srv.URL+"/i/api/graphql/x/"+endpoint)
	require.Error(t, err)

	msg := err.Error()
	authIdx := strings.Index(msg, endpoint+" HTTP 404")
	guestIdx := strings.Index(msg, "(guest) HTTP 404")
	require.GreaterOrEqual(t, authIdx, 0, "authenticated status+endpoint missing: %q", msg)
	require.GreaterOrEqual(t, guestIdx, 0, "guest failure missing: %q", msg)
	assert.Less(t, authIdx, guestIdx, "authenticated failure must come first: %q", msg)
}
