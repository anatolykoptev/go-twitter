package twitter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// F1 — journey: a credential service rotated the account's auth_token (A -> B)
// while an old session file with A is still on disk. A *WithSocial-style client
// is built with the leased token B and makes one request against a fake x.com:
// the request must carry auth_token=B, never A, and the disk file must now hold B.
func TestLoadOrLogin_ProvidedCredentialsBeatStaleDiskSession(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, saveSession(dir, "rotated_acc", "tokenA", "ct0A"))

	var mu sync.Mutex
	var cookies []string
	bc := startFakeX(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cookies = append(cookies, r.Header.Get("Cookie"))
		mu.Unlock()
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
	})

	acc := &Account{Username: "rotated_acc", AuthToken: "tokenB", CT0: "ct0B"} // what tryWithAccount builds
	c, err := NewClient(ClientConfig{Accounts: []*Account{acc}, SessionDir: dir, skipPoolGauge: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.client = bc
	c.xtidMgr = xtid.NewManager(failFetcher{})
	c.cfg.DisableGuestFallback = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = c.GetUserTweets(ctx, "999", 20)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, cookies, "fake x.com saw no request")
	for _, ck := range cookies {
		assert.Contains(t, ck, "auth_token=tokenB", "request must carry the provided token")
		assert.NotContains(t, ck, "tokenA", "stale disk token leaked into the request")
	}

	tok, ct0, err := loadSession(dir, "rotated_acc", time.Hour)
	require.NoError(t, err)
	assert.Equal(t, "tokenB", tok, "disk file must be overwritten with the provided credentials")
	assert.Equal(t, "ct0B", ct0)
}

// F2 — username+password only (no provided token) and a valid disk session:
// the disk session is used and no login flow runs. The stealth client is routed
// through a counting proxy; any login attempt would hit it (and fail, since the
// proxy answers 502).
func TestLoadOrLogin_PasswordOnlyAccountUsesDiskSession(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, saveSession(dir, "pw_acc", "diskTok", "diskCT0"))

	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	bc, err := stealth.NewClient(stealth.WithProxy(proxy.URL), stealth.WithoutSSRFGuard())
	require.NoError(t, err)

	c := &Client{cfg: ClientConfig{SessionDir: dir, SessionTTL: time.Hour}}
	acc := &Account{Username: "pw_acc", Password: "hunter2"}

	require.NoError(t, c.loadOrLogin(acc, bc), "disk session must satisfy a password-only account")
	tok, ct0, _ := acc.Credentials()
	assert.Equal(t, "diskTok", tok)
	assert.Equal(t, "diskCT0", ct0)
	assert.Zero(t, proxyHits.Load(), "no login flow may run when a valid disk session exists")
}

// A caller-supplied auth_token without ct0 must not be silently replaced by a
// different token from disk either.
func TestLoadOrLogin_ProvidedTokenWithoutCT0IgnoresDifferingDisk(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, saveSession(dir, "half_acc", "tokenA", "ct0A"))

	c := &Client{cfg: ClientConfig{SessionDir: dir, SessionTTL: time.Hour}}
	acc := &Account{Username: "half_acc", AuthToken: "tokenB"} // no ct0, no password

	err := c.loadOrLogin(acc, nil)
	require.Error(t, err, "must not fall back to the differing disk session")
	tok, _, _ := acc.Credentials()
	assert.Equal(t, "tokenB", tok)
}

// F3 — saveSession is atomic. Writers rewrite a large (256 KiB) session in a loop
// while readers loop loadSession; with truncate-then-write a reader catches the
// file empty or half-written and gets a JSON decode error. The payload size makes
// the write window wide (many pages) so the race is hit within a few iterations
// rather than by luck. Rename-based writes never expose a partial file.
func TestSaveSession_AtomicUnderConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 256<<10)
	require.NoError(t, saveSession(dir, "atomic_acc", big, "ct0"))

	const writers, readers, iters = 2, 4, 40
	var (
		wg     sync.WaitGroup
		stop   = make(chan struct{})
		badMu  sync.Mutex
		badErr error
	)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := loadSession(dir, "atomic_acc", time.Hour); err != nil {
					badMu.Lock()
					if badErr == nil {
						badErr = err
					}
					badMu.Unlock()
				}
			}
		}()
	}
	var ww sync.WaitGroup
	for w := 0; w < writers; w++ {
		ww.Add(1)
		go func() {
			defer ww.Done()
			for i := 0; i < iters; i++ {
				assert.NoError(t, saveSession(dir, "atomic_acc", big, "ct0"))
			}
		}()
	}
	ww.Wait()
	close(stop)
	wg.Wait()

	assert.NoError(t, badErr, "a concurrent reader observed a truncated/partial session file")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "temp files must not be left behind")
	fi, err := os.Stat(sessionPath(dir, "atomic_acc"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm())
}
