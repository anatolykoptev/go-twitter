package xtid

import (
	"context"
	"errors"
	"testing"
	"time"
)

// holdFetcher holds every fetch until the gate opens, then serves from inner.
type holdFetcher struct {
	inner *exactFetcher
	gate  chan struct{}
}

func (g *holdFetcher) Fetch(ctx context.Context, u string) ([]byte, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.inner.Fetch(ctx, u)
}

// X2 — the cold-start key fetch is shared work: a caller that leaves (its ctx
// ends) must not cancel it, so a later caller still gets keys once the source
// answers.
func TestGenerateIDWith_AbandonedCallerDoesNotCancelSharedFetch(t *testing.T) {
	g := &holdFetcher{inner: newExactFetcher(viteRoutes(fixtureKey)), gate: make(chan struct{})}
	mgr := NewManager(g)
	mgr.cacheDir = ""

	ctx1, cancel1 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel1()
	if _, err := mgr.GenerateIDWith(ctx1, "GET", "/p", g); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first caller: err = %v, want DeadlineExceeded", err)
	}

	close(g.gate) // the source answers after the first caller left
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	id, err := mgr.GenerateIDWith(ctx2, "GET", "/p", g)
	if err != nil || id == "" {
		t.Fatalf("second caller: id=%q err=%v, want keys from the surviving fetch", id, err)
	}
}
