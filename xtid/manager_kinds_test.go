package xtid

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
)

// A guest request that fails against the logged-out shell must not lock out an
// authenticated init a few seconds later (the backoff is per attempt kind).
func TestGuestFailureDoesNotGateAuthenticatedInit(t *testing.T) {
	base := newExactFetcher(map[string][]byte{warmPageURL: viteShellHTML()})
	auth := newExactFetcher(viteRoutes(fixtureKey))
	mgr := newTestManager(base)
	clock := time.Now()
	mgr.now = func() time.Time { return clock }

	if _, err := mgr.GenerateID("GET", "/p"); err == nil {
		t.Fatal("guest GenerateID succeeded against the shell")
	}
	clock = clock.Add(5 * time.Second)

	id, err := mgr.GenerateIDWith(context.Background(), "GET", "/p", auth)
	if err != nil || id == "" {
		t.Fatalf("authenticated init after a guest failure: %q, %v", id, err)
	}
	if auth.count(authedWarmPageURL) != 1 {
		t.Fatalf("authenticated warm page fetched %d times, want 1", auth.count(authedWarmPageURL))
	}
}

// An authenticated attempt that fails with something other than the logged-out
// shell does not also hit the unauthenticated page.
func TestAuthenticatedNonShellFailureSkipsGuestFallback(t *testing.T) {
	base := newExactFetcher(map[string][]byte{warmPageURL: viteShellHTML()})
	auth := newExactFetcher(map[string][]byte{}) // 404 on the authed page
	mgr := newTestManager(base)

	if _, err := mgr.GenerateIDWith(context.Background(), "GET", "/p", auth); err == nil {
		t.Fatal("expected failure")
	}
	if got := base.count(warmPageURL); got != 0 {
		t.Fatalf("guest fallback fetched %d times after a non-shell auth failure, want 0", got)
	}
}

// A logged-out shell on the authenticated page (cookies not honoured) does fall back.
func TestAuthenticatedShellFallsBackToGuest(t *testing.T) {
	base := newExactFetcher(map[string][]byte{warmPageURL: viteShellHTML()})
	auth := newExactFetcher(map[string][]byte{authedWarmPageURL: viteShellHTML()})
	mgr := newTestManager(base)

	_, err := mgr.GenerateIDWith(context.Background(), "GET", "/p", auth)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if got := base.count(warmPageURL); got != 1 {
		t.Fatalf("guest fallback fetched %d times after a shell on the auth page, want 1", got)
	}
}

type blockingFetcher struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingFetcher) Fetch(ctx context.Context, _ string) ([]byte, error) {
	b.calls.Add(1)
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil, errors.New("blocked: HTTP 404")
}

// A waiter's ctx bounds its wait; the leader's init keeps running.
func TestGenerateIDWith_WaiterContextBoundsWait(t *testing.T) {
	bf := &blockingFetcher{started: make(chan struct{}, 1), release: make(chan struct{})}
	mgr := NewManager(bundle.Fetcher(bf))
	go func() { _, _ = mgr.GenerateID("GET", "/p") }() // leader
	<-bf.started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := mgr.GenerateIDWith(ctx, "GET", "/p", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("waiter was not bounded by its ctx")
	}
	close(bf.release)
}
