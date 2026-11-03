package xtid

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// shortKeyJS makes the animation read key byte 9, which an 8-byte key lacks.
const shortKeyJS = `(function(){return k(c[0],16)*k(c[9],16)})()`

func shortKeyRoutes(key []byte, js string) map[string][]byte {
	return map[string][]byte{
		warmPageURL: legacyHTML(key),
		"https://abs.twimg.com/responsive-web/client-web/ondemand.s.deadbe0a.js": []byte(js),
	}
}

// A key too short for the script's indices used to build from fewer bytes than
// X reads: a well-formed id that X rejects, for the 30 minutes the keys live.
// It must fail instead, so the manager keeps its previous keys and backs off.
func TestBuildAnimationKey_IndexOutsideKeyFails(t *testing.T) {
	page, js := string(legacyHTML(fixtureKey)), shortKeyJS
	_, err := newClientTransaction(page, js)
	if err == nil || !strings.Contains(err.Error(), "outside the 8-byte verification key") {
		t.Fatalf("newClientTransaction = %v, want an index-outside-the-key error", err)
	}
	// Row index outside the key: the script's first index.
	if _, err := newClientTransaction(page, `(function(){return k(c[12],16)*k(c[1],16)})()`); err == nil ||
		!strings.Contains(err.Error(), "row index 12") {
		t.Fatalf("row index outside the key: %v", err)
	}
}

func TestGenerateIDWith_ShortKeyRefreshKeepsPreviousKeys(t *testing.T) {
	good := newExactFetcher(shortKeyRoutes(fixtureKey, ondemandJS))
	mgr := NewManager(good)
	mgr.cacheDir = t.TempDir()
	clock := time.Now()
	mgr.now = func() time.Time { return clock }
	hookCalls := make(chan error, 4)
	mgr.SetInitFailedHook(func(err error) { hookCalls <- err })

	id, err := mgr.GenerateID("GET", "/p")
	if err != nil {
		t.Fatalf("first init: %v", err)
	}
	if got := keyBytesOf(t, id, len(fixtureKey)); !bytes.Equal(got, fixtureKey) {
		t.Fatalf("first id built on %v, want %v", got, fixtureKey)
	}

	// The page now ships a script that reads past the end of the key.
	good.set(shortKeyRoutes(fixtureKey, shortKeyJS))
	clock = clock.Add(mgr.refreshInterval + time.Minute)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := mgr.GenerateIDWith(context.Background(), "GET", "/p", nil); err != nil {
			t.Fatalf("GenerateIDWith with stale keys: %v", err)
		}
		mgr.mu.RLock()
		failed := mgr.fails[kindGuest].err
		streak := mgr.fails[kindGuest].streak
		mgr.mu.RUnlock()
		if failed != nil {
			if !strings.Contains(failed.Error(), "outside the 8-byte verification key") {
				t.Fatalf("recorded failure = %v", failed)
			}
			if streak != 1 || !mgr.backoffActive(kindGuest) {
				t.Fatalf("streak %d, backoff active %v: want one failure and a backoff", streak, mgr.backoffActive(kindGuest))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refresh with a short key never recorded a failure")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-hookCalls:
		if errors.Is(err, ErrUnavailable) {
			t.Fatalf("hook got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the init-failed hook was not called")
	}
	if len(hookCalls) != 0 {
		t.Fatalf("the init-failed hook was called %d extra times", len(hookCalls))
	}
	// The previous keys keep serving.
	id, err = mgr.GenerateIDWith(context.Background(), "GET", "/p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := keyBytesOf(t, id, len(fixtureKey)); !bytes.Equal(got, fixtureKey) {
		t.Fatalf("id after the failed refresh built on %v, want the previous keys", got)
	}
}
