package xtid

import (
	"bytes"
	"context"
	"encoding/base64"
	"sync"
	"testing"
	"time"
)

// gatedFetcher serves exactFetcher routes but, once armed, blocks every fetch
// until release is closed (or the request context ends).
type gatedFetcher struct {
	*exactFetcher
	mu      sync.Mutex
	release chan struct{}
}

func (g *gatedFetcher) arm() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.release = make(chan struct{})
	return g.release
}

func (g *gatedFetcher) Fetch(ctx context.Context, u string) ([]byte, error) {
	g.mu.Lock()
	rel := g.release
	g.mu.Unlock()
	if rel != nil {
		select {
		case <-rel:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.exactFetcher.Fetch(ctx, u)
}

// keyBytesOf recovers the verification key an id was built with: the first byte
// is the XOR mask, the next len(key) bytes are the key XOR the mask.
func keyBytesOf(t *testing.T, id string, n int) []byte {
	t.Helper()
	raw, err := base64.RawStdEncoding.DecodeString(id)
	if err != nil {
		t.Fatalf("decode id %q: %v", id, err)
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = raw[1+i] ^ raw[0]
	}
	return out
}

func legacyRoutes(key []byte) map[string][]byte {
	return map[string][]byte{
		warmPageURL: legacyHTML(key),
		"https://abs.twimg.com/responsive-web/client-web/ondemand.s.deadbe0a.js": []byte(ondemandJS),
	}
}

// With a built but expired ClientTransaction the request path must not wait for
// the refresh: callers get an id from the old keys at once, the refresh runs in
// the background (one flight for any number of callers), and the next call after
// it lands uses the new keys.
func TestGenerateIDWith_ExpiredKeysRefreshInBackground(t *testing.T) {
	oldKey := fixtureKey
	newKey := []byte{0, 21, 31, 41, 51, 0, 71, 81}

	gf := &gatedFetcher{exactFetcher: newExactFetcher(legacyRoutes(oldKey))}
	mgr := NewManager(gf)
	mgr.cacheDir = t.TempDir()
	var cmu sync.Mutex
	clock := time.Now()
	mgr.now = func() time.Time { cmu.Lock(); defer cmu.Unlock(); return clock }

	if _, err := mgr.GenerateID("GET", "/p"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if got := gf.count(warmPageURL); got != 1 {
		t.Fatalf("warm page fetched %d times by the first init, want 1", got)
	}

	// Expire the keys and make the refresh hang on the network.
	release := gf.arm()
	gf.set(legacyRoutes(newKey))
	cmu.Lock()
	clock = clock.Add(mgr.refreshInterval + time.Minute)
	cmu.Unlock()

	const callers = 20
	var wg sync.WaitGroup
	ids := make([]string, callers)
	errs := make([]error, callers)
	took := make([]time.Duration, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// The deadline turns a synchronous refresh into a failure, not a hang.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			ids[i], errs[i] = mgr.GenerateIDWith(ctx, "GET", "/p", nil)
			took[i] = time.Since(start)
		}(i)
	}
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d blocked on / failed with the refresh: %v (after %v)", i, errs[i], took[i])
		}
		if took[i] > time.Second {
			t.Errorf("caller %d took %v during a hung refresh", i, took[i])
		}
		if got := keyBytesOf(t, ids[i], len(oldKey)); !bytes.Equal(got, oldKey) {
			t.Fatalf("caller %d: key %v, want the old keys %v while refreshing", i, got, oldKey)
		}
	}

	close(release)

	// The next calls pick up the new keys once the background refresh lands.
	deadline := time.Now().Add(5 * time.Second)
	for {
		id, err := mgr.GenerateIDWith(context.Background(), "GET", "/p", nil)
		if err != nil {
			t.Fatalf("GenerateIDWith after release: %v", err)
		}
		if bytes.Equal(keyBytesOf(t, id, len(newKey)), newKey) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new keys never took effect after the refresh was released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := gf.count(warmPageURL); got != 2 {
		t.Fatalf("warm page fetched %d times in total, want 2 (first init + one shared refresh)", got)
	}
}
