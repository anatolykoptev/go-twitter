package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"bytes"
	"fmt"
	"log/slog"
	"sync/atomic"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/social"
	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rotatedCT0 = "rotatedCT0value123"

// recordingStore is an in-memory SessionStore that logs every call.
type recordingStore struct {
	mu      sync.Mutex
	data    map[string]Session
	loads   []string
	saves   []savedCall
	deletes []string
	saveErr error
	loadErr error
	delErr  error
	ops     []string // call order: "load:<u>", "save:<u>", "delete:<u>"
}

type savedCall struct {
	user string
	s    Session
}

func newRecordingStore() *recordingStore { return &recordingStore{data: map[string]Session{}} }

func (r *recordingStore) Load(_ context.Context, u string) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loads = append(r.loads, u)
	r.ops = append(r.ops, "load:"+u)
	if r.loadErr != nil {
		return Session{}, r.loadErr
	}
	s, ok := r.data[u]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return s, nil
}

func (r *recordingStore) Save(_ context.Context, u string, s Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saves = append(r.saves, savedCall{u, s})
	r.ops = append(r.ops, "save:"+u)
	if r.saveErr != nil {
		return r.saveErr
	}
	r.data[u] = s
	return nil
}

func (r *recordingStore) Delete(_ context.Context, u string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes = append(r.deletes, u)
	r.ops = append(r.ops, "delete:"+u)
	if r.delErr != nil {
		return r.delErr
	}
	delete(r.data, u)
	return nil
}

func (r *recordingStore) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.loads) + len(r.saves) + len(r.deletes)
}

func (r *recordingStore) savedCT0s(user string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.saves {
		if c.user == user {
			out = append(out, c.s.CT0)
		}
	}
	return out
}

// rotatingFakeX answers every request with valid user-tweets JSON and a
// set-cookie carrying a rotated ct0, like x.com does on a normal request.
func rotatingFakeX(w http.ResponseWriter, req *http.Request) {
	w.Header().Add("Set-Cookie", "ct0="+rotatedCT0+"; Path=/; Secure")
	if req.URL.Path != "" && containsFold(req.URL.Path, "UserByScreenName") {
		_, _ = w.Write([]byte(socialUserBody))
		return
	}
	_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
}

// newRotatingClient builds a Client over a fake x.com that rotates ct0.
func newRotatingClient(t *testing.T, cfg ClientConfig, accSpec string) *Client {
	t.Helper()
	cfg.Accounts = ParseAccounts(accSpec)
	cfg.skipPoolGauge = true
	c, err := NewClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.client = startFakeX(t, rotatingFakeX)
	c.xtidMgr = xtid.NewManager(failFetcher{})
	c.cfg.DisableGuestFallback = true
	return c
}

func getTweets(t *testing.T, c *Client) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.GetUserTweets(ctx, "999", 20)
	return err
}

// F1 — journey: a consumer-injected store receives the rotated ct0 that x.com
// sets on a normal request, keyed by username.
func TestSessionStore_InjectedStoreReceivesRotatedCT0(t *testing.T) {
	rec := newRecordingStore()
	c := newRotatingClient(t, ClientConfig{SessionStore: rec, SessionDir: t.TempDir()}, "u1:pw:tokA:ct0A")

	require.NoError(t, getTweets(t, c))

	assert.Contains(t, rec.savedCT0s("u1"), rotatedCT0, "rotated ct0 never reached the injected store")
	_, err := os.Stat(filepath.Join(c.cfg.SessionDir, "u1.json"))
	assert.True(t, os.IsNotExist(err), "injected store set: no session file may be written")
}

// F2 — default config: a session file in the OLD on-disk format loads, and a
// rotation re-saves to the same path in the same format.
func TestSessionStore_DefaultFileStoreKeepsLegacyBehaviour(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")
	old := `{"auth_token":"tokFromDisk","ct0":"ct0FromDisk","saved_at":"` +
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + `"}`
	require.NoError(t, os.WriteFile(path, []byte(old), 0600))

	c := newRotatingClient(t, ClientConfig{SessionDir: dir}, "legacy:pw:tokStatic:ct0Static")
	tok, ct0, _ := c.cfg.Accounts[0].Credentials()
	require.Equal(t, "tokFromDisk", tok, "legacy session file did not load")
	require.Equal(t, "ct0FromDisk", ct0)

	require.NoError(t, getTweets(t, c))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, rotatedCT0, got["ct0"], "rotation must re-save to the same path")
	assert.Equal(t, "tokFromDisk", got["auth_token"])
	assert.Contains(t, got, "saved_at")
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm())
}

// F3 — *WithSocial path: leased credentials never touch the store, even when
// x.com rotates ct0 mid-call.
func TestSessionStore_WithSocialMakesZeroStoreCalls(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := newRecordingStore()
	bc := startFakeX(t, rotatingFakeX)
	testHookSocialClient = func(c *Client) {
		c.client = bc
		c.xtidMgr = xtid.NewManager(failFetcher{})
		c.accountPacer = nil
		c.cfg.SessionStore = rec
	}
	t.Cleanup(func() { testHookSocialClient = nil })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(social.Credentials{
				ID:          "acc-1",
				Credentials: map[string]string{"username": "leased", "auth_token": "tokL", "ct0": "ct0L"},
			})
		}
	}))
	t.Cleanup(srv.Close)
	sc := social.NewClient(srv.URL, "tok", "test")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tweets, err := GetUserTweetsWithSocial(ctx, sc, "tesla", 5)
	require.NoError(t, err)
	require.NotEmpty(t, tweets)
	assert.Zero(t, rec.calls(), "leased credentials must never touch the session store")
}

// F4 — relogin deletes the stored session for that username.
func TestSessionStore_ReloginDeletesStoredSession(t *testing.T) {
	rec := newRecordingStore()
	rec.data["u4"] = Session{AuthToken: "t", CT0: "c", SavedAt: time.Now()}
	c := &Client{cfg: ClientConfig{SessionStore: rec}, client: startFakeX(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})}
	acc := &Account{Username: "u4", Password: "pw"}

	_ = c.relogin(acc) // the login itself fails against the fake; only the delete matters
	assert.Equal(t, []string{"u4"}, rec.deletes, "relogin must delete the stored session")
}

// F5 — a store whose Save fails never fails the API call.
func TestSessionStore_SaveErrorDoesNotFailRequest(t *testing.T) {
	rec := newRecordingStore()
	rec.saveErr = errors.New("db down")
	c := newRotatingClient(t, ClientConfig{SessionStore: rec}, "u5:pw:tokA:ct0A")

	require.NoError(t, getTweets(t, c), "store failure must not fail the request")
	assert.NotEmpty(t, rec.savedCT0s("u5"), "Save must have been attempted")
}

func TestFileSessionStore_TTL(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	fresh := &FileSessionStore{Dir: dir, TTL: time.Hour}
	require.NoError(t, fresh.Save(ctx, "x", Session{AuthToken: "a", CT0: "b", SavedAt: time.Now().Add(-2 * time.Hour)}))

	_, err := fresh.Load(ctx, "x")
	assert.ErrorIs(t, err, ErrSessionNotFound, "expired session must read as not found")

	s, err := (&FileSessionStore{Dir: dir}).Load(ctx, "x")
	require.NoError(t, err, "TTL 0 disables expiry")
	assert.Equal(t, "a", s.AuthToken)

	require.NoError(t, fresh.Delete(ctx, "x"))
	require.NoError(t, fresh.Delete(ctx, "x"), "deleting a missing session is not an error")
	_, err = fresh.Load(ctx, "x")
	assert.ErrorIs(t, err, ErrSessionNotFound)
}

// failingClient answers 500 to everything: a login attempt against it fails fast.
func failingClient(t *testing.T, store SessionStore) *Client {
	t.Helper()
	return &Client{cfg: ClientConfig{SessionStore: store}, client: startFakeX(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})}
}

// A non-NotFound Load error leaves the stored state unknown: a password-only
// account must NOT fall through to a login (no login storm), provided
// credentials are used as-is.
func TestSessionStore_LoadErrorDoesNotTriggerLogin(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	bc, err := stealth.NewClient(stealth.WithProxy(proxy.URL), stealth.WithoutSSRFGuard())
	require.NoError(t, err)

	boom := errors.New("db timeout")
	rec := newRecordingStore()
	rec.loadErr = boom
	c := &Client{cfg: ClientConfig{SessionStore: rec}}

	err = c.loadOrLogin(&Account{Username: "pwonly", Password: "hunter2"}, bc)
	require.ErrorIs(t, err, boom)
	assert.Zero(t, proxyHits.Load(), "store error must not cause a password login")

	prov := &Account{Username: "prov", Password: "pw", AuthToken: "tokP", CT0: "ct0P"}
	require.NoError(t, c.loadOrLogin(prov, bc), "provided credentials must be used when the store errors")
	tok, ct0, _ := prov.Credentials()
	assert.Equal(t, "tokP", tok)
	assert.Equal(t, "ct0P", ct0)
	assert.Empty(t, rec.saves, "no save while the store state is unknown")
	assert.Zero(t, proxyHits.Load())
}

// relogin deletes BEFORE it loads, so the stale session is never re-adopted.
func TestSessionStore_ReloginDeletesBeforeLoad(t *testing.T) {
	rec := newRecordingStore()
	rec.data["u"] = Session{AuthToken: "old", CT0: "oldct0", SavedAt: time.Now()}
	c := failingClient(t, rec)
	acc := &Account{Username: "u", Password: "pw"}

	_ = c.relogin(acc)
	require.GreaterOrEqual(t, len(rec.ops), 1)
	assert.Equal(t, "delete:u", rec.ops[0], "Delete must precede any Load; got %v", rec.ops)
	tok, _, _ := acc.Credentials()
	assert.NotEqual(t, "old", tok, "relogin adopted the stale session")
}

// If Delete fails, relogin must not reload from the store (the old session
// would still be there): it goes straight to the password login.
func TestSessionStore_ReloginAfterFailedDeleteSkipsLoad(t *testing.T) {
	rec := newRecordingStore()
	rec.data["u"] = Session{AuthToken: "old", CT0: "oldct0", SavedAt: time.Now()}
	rec.delErr = errors.New("delete failed")
	c := failingClient(t, rec)
	acc := &Account{Username: "u", Password: "pw"}

	_ = c.relogin(acc)
	assert.Equal(t, []string{"delete:u"}, rec.ops, "no Load may follow a failed Delete")
	tok, _, _ := acc.Credentials()
	assert.NotEqual(t, "old", tok, "relogin adopted the old token after a failed Delete")
}

// A leased (credsAuthoritative) account never touches the store on relogin.
func TestSessionStore_LeasedReloginMakesZeroStoreCalls(t *testing.T) {
	rec := newRecordingStore()
	c := failingClient(t, rec)
	acc := &Account{Username: "leased", Password: "pw", AuthToken: "t", CT0: "c", credsAuthoritative: true}

	_ = c.relogin(acc)
	assert.Empty(t, rec.ops, "leased account relogin touched the store: %v", rec.ops)
}

func TestSession_RedactsTokens(t *testing.T) {
	s := Session{AuthToken: "SECRETTOKEN", CT0: "SECRETCT0", SavedAt: time.Now()}
	for _, out := range []string{fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%s", s), s.String()} {
		assert.NotContains(t, out, "SECRET")
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", slog.Any("session", s))
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", slog.Any("session", s))
	assert.NotContains(t, buf.String(), "SECRET")
	b, err := json.Marshal(s)
	require.NoError(t, err)
	assert.Contains(t, string(b), "SECRETTOKEN", "JSON persistence format must be unaffected")
}
