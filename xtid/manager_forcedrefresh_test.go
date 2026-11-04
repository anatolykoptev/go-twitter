package xtid

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// refreshHarness is a Manager over a counting fetcher with a controllable clock,
// already initialised with oldKey.
type refreshHarness struct {
	mgr   *Manager
	gf    *gatedFetcher
	clock func(d time.Duration)
}

func newRefreshHarness(t *testing.T, oldKey []byte) *refreshHarness {
	t.Helper()
	gf := &gatedFetcher{exactFetcher: newExactFetcher(legacyRoutes(oldKey))}
	mgr := NewManager(gf)
	mgr.cacheDir = t.TempDir()
	var mu sync.Mutex
	now := time.Now()
	mgr.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	if _, err := mgr.GenerateID("GET", "/p"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	return &refreshHarness{mgr: mgr, gf: gf, clock: func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }}
}

func (h *refreshHarness) keyNow(t *testing.T, n int) []byte {
	t.Helper()
	id, err := h.mgr.GenerateID("GET", "/p")
	if err != nil {
		t.Fatalf("GenerateID: %v", err)
	}
	return keyBytesOf(t, id, n)
}

// A caller whose id was rejected asks for a rebuild: keys older than the floor
// are re-derived from freshly fetched pages and the next id uses them.
func TestRefresh_RebuildsKeysOlderThanFloor(t *testing.T) {
	oldKey, newKey := fixtureKey, []byte{0, 21, 31, 41, 51, 0, 71, 81}
	h := newRefreshHarness(t, oldKey)
	h.gf.set(legacyRoutes(newKey))
	h.clock(defaultForcedRefreshMinAge + time.Second) // older than the floor, younger than the TTL

	if !h.mgr.Refresh(context.Background(), nil) {
		t.Fatal("Refresh = false, want a rebuild of keys older than the floor")
	}
	if got := h.gf.count(warmPageURL); got != 2 {
		t.Fatalf("warm page fetched %d times, want 2 (init + refresh)", got)
	}
	if got := h.keyNow(t, len(newKey)); !bytes.Equal(got, newKey) {
		t.Fatalf("id built with key %v after Refresh, want the new key %v", got, newKey)
	}
}

// Keys built moments ago and still rejected are not the cause: no page fetch.
func TestRefresh_YoungKeysAreNotRebuilt(t *testing.T) {
	h := newRefreshHarness(t, fixtureKey)
	h.clock(defaultForcedRefreshMinAge - time.Second)

	if h.mgr.Refresh(context.Background(), nil) {
		t.Fatal("Refresh = true for keys younger than the floor")
	}
	if got := h.gf.count(warmPageURL); got != 1 {
		t.Fatalf("warm page fetched %d times, want 1 (no refetch for young keys)", got)
	}
}

// Many concurrent callers share one rebuild.
func TestRefresh_ConcurrentCallersShareOneRebuild(t *testing.T) {
	oldKey, newKey := fixtureKey, []byte{0, 21, 31, 41, 51, 0, 71, 81}
	h := newRefreshHarness(t, oldKey)
	h.gf.set(legacyRoutes(newKey))
	h.clock(defaultForcedRefreshMinAge + time.Second)
	release := h.gf.arm() // hold the rebuild so every caller piles up behind it

	const callers = 16
	results := make([]bool, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = h.mgr.Refresh(context.Background(), nil)
		}(i)
	}
	time.Sleep(150 * time.Millisecond) // let the callers reach the flight
	close(release)
	wg.Wait()

	if got := h.gf.count(warmPageURL); got != 2 {
		t.Fatalf("warm page fetched %d times for %d concurrent callers, want 2 (init + ONE refresh)", got, callers)
	}
	for i, ok := range results {
		if !ok {
			t.Errorf("caller %d: Refresh = false, want true (joined the rebuild)", i)
		}
	}
}

// A failed rebuild reports false and the previous keys keep serving.
func TestRefresh_FailureKeepsPreviousKeys(t *testing.T) {
	h := newRefreshHarness(t, fixtureKey)
	h.gf.set(map[string][]byte{}) // every fetch now 404s
	h.clock(defaultForcedRefreshMinAge + time.Second)

	if h.mgr.Refresh(context.Background(), nil) {
		t.Fatal("Refresh = true although every fetch failed")
	}
	if got := h.keyNow(t, len(fixtureKey)); !bytes.Equal(got, fixtureKey) {
		t.Fatalf("keys after failed refresh = %v, want the previous %v", got, fixtureKey)
	}
}

// Before the first init there is nothing to refresh: GenerateID owns that.
func TestRefresh_NoKeysYetDoesNothing(t *testing.T) {
	gf := &gatedFetcher{exactFetcher: newExactFetcher(legacyRoutes(fixtureKey))}
	mgr := NewManager(gf)
	mgr.cacheDir = t.TempDir()
	if mgr.Refresh(context.Background(), nil) {
		t.Fatal("Refresh = true on a manager that never initialised")
	}
	if got := gf.count(warmPageURL); got != 0 {
		t.Fatalf("warm page fetched %d times, want 0", got)
	}
}
