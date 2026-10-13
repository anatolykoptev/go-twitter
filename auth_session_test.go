package twitter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/social"
	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cookieRecorder is a fake x.com handler recording every Cookie header.
type cookieRecorder struct {
	mu      sync.Mutex
	cookies []string
}

func (r *cookieRecorder) handler(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.cookies = append(r.cookies, req.Header.Get("Cookie"))
	r.mu.Unlock()
	if strings.Contains(req.URL.Path, "UserByScreenName") {
		_, _ = w.Write([]byte(socialUserBody))
		return
	}
	_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
}

func (r *cookieRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cookies...)
}

func readSessionFile(t *testing.T, dir, user string) []byte {
	t.Helper()
	p, err := sessionPath(dir, user)
	require.NoError(t, err)
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	return b
}

// F1 — journey (*WithSocial path): a credential service rotated the account's
// auth_token (A -> B) while an old session file with A is still on disk. The
// public GetUserWithSocial leases B, makes one request against a fake x.com: it
// must carry auth_token=B, never A, and the session file must be left untouched.
func TestWithSocial_LeasedCredentialsBeatStaleDiskSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".go-twitter", "sessions")
	require.NoError(t, saveSession(dir, "rotated_acc", "tokenA", "ct0A"))
	before := readSessionFile(t, dir, "rotated_acc")

	rec := &cookieRecorder{}
	bc := startFakeX(t, rec.handler)
	testHookSocialClient = func(c *Client) {
		c.client = bc
		c.xtidMgr = xtid.NewManager(failFetcher{})
		c.accountPacer = nil
	}
	t.Cleanup(func() { testHookSocialClient = nil })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(social.Credentials{
				ID: "acc-1",
				Credentials: map[string]string{
					"username": "rotated_acc", "auth_token": "tokenB", "ct0": "ct0B",
				},
			})
		}
	}))
	t.Cleanup(srv.Close)
	sc := social.NewClient(srv.URL, "tok", "test")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := GetUserWithSocial(ctx, sc, "tesla")
	require.NoError(t, err)

	cookies := rec.snapshot()
	require.NotEmpty(t, cookies, "fake x.com saw no request")
	for _, ck := range cookies {
		assert.Contains(t, ck, "auth_token=tokenB", "request must carry the leased token")
		assert.NotContains(t, ck, "tokenA", "stale disk token leaked into the request")
	}
	assert.Equal(t, before, readSessionFile(t, dir, "rotated_acc"), "leased path must not touch the session file")
}

// F5 — shape (b), long-lived client from static config (ParseAccounts
// "user:pass:auth_token:ct0"): an earlier in-process relogin saved the newer
// token B to disk; after a restart the config still says A. The disk session
// (B) must win — otherwise A (revoked) forces a password relogin on every restart.
func TestParseAccounts_FreshDiskSessionBeatsStaticConfigToken(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, saveSession(dir, "cfg_acc", "tokenB", "ct0B"))

	accs := ParseAccounts("cfg_acc:pw:tokenA:ct0A")
	require.Len(t, accs, 1)

	rec := &cookieRecorder{}
	bc := startFakeX(t, rec.handler)
	c, err := NewClient(ClientConfig{Accounts: accs, SessionDir: dir, skipPoolGauge: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.client = bc
	c.xtidMgr = xtid.NewManager(failFetcher{})
	c.cfg.DisableGuestFallback = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = c.GetUserTweets(ctx, "999", 20)
	require.NoError(t, err, "no relogin may be needed: the disk session is the newest token")

	cookies := rec.snapshot()
	require.NotEmpty(t, cookies)
	for _, ck := range cookies {
		assert.Contains(t, ck, "auth_token=tokenB", "restart must resume the newest (disk) token")
		assert.NotContains(t, ck, "tokenA", "stale static-config token won over the disk session")
	}
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
	p, err := sessionPath(dir, "atomic_acc")
	require.NoError(t, err)
	fi, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm())
}

// A username from the credential service must never escape the session dir.
func TestSessionPath_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, u := range []string{"", "../evil", "a/b", `a\b`, "..", "x..y", "/abs", "nul\x00byte"} {
		_, err := sessionPath(dir, u)
		require.ErrorIs(t, err, errInvalidSessionUsername, "username %q", u)
		assert.Error(t, saveSession(dir, u, "t", "c"), "save %q", u)
		_, _, lerr := loadSession(dir, u, time.Hour)
		assert.Error(t, lerr, "load %q", u)
	}
	_, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.json"))
	assert.True(t, os.IsNotExist(err), "traversal wrote outside the session dir")

	p, err := sessionPath(dir, "good_user.1")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "good_user.1.json"), p)
}
