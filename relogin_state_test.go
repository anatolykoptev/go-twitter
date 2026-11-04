package twitter

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	reloginOldTok = "tokOLD"
	reloginOldCT0 = "ct0OLD"
	reloginNewTok = "tokNEW"
)

// reloginJourney is a fake service for the code-32 -> relogin journey. One
// loopback server plays the GraphQL API (rejects every token except
// reloginNewTok with code 32) and, when it is reached through the client's
// proxy hop as plain HTTP, the login flow host (which hands out reloginNewTok).
type reloginJourney struct {
	logins atomic.Int32
	// guestTok, when set, is the guest token the activate endpoint hands out
	// (default "g1"); open accounts take their username from it.
	guestTok string
	// gqlHits, when non-nil, counts each request the GraphQL branch answers.
	// Requests arrive at handler directly AND tunnelled inside a CONNECT (the
	// client tunnels plain HTTP too), so only handler sees them all.
	gqlHits *atomic.Int32
	// answerAuthed, when set, replaces the timeline answer for a GraphQL
	// request carrying reloginNewTok — the verdict a post-relogin retry meets
	// (S3 in silent_failures_test.go answers suspended).
	answerAuthed func(w http.ResponseWriter, r *http.Request)
	// pre, when set, sees every GraphQL-host request before the token check;
	// returning true means it answered the request itself.
	pre func(w http.ResponseWriter, r *http.Request) bool
}

// oneConnListener hands a single, already-accepted connection to http.Serve.
type oneConnListener struct {
	ch   chan net.Conn
	addr net.Addr
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{ch: make(chan net.Conn, 1), addr: c.LocalAddr()}
	l.ch <- c
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if c, ok := <-l.ch; ok {
		return c, nil
	}
	return nil, net.ErrClosed
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.addr }

// serve answers CONNECT (the client reaches every host through the proxy hop)
// by accepting the tunnel and serving the tunnelled plain HTTP with handler.
func (j *reloginJourney) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		j.handler(w, r)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	go func() { _ = http.Serve(newOneConnListener(conn), http.HandlerFunc(j.handler)) }()
}

func (j *reloginJourney) handler(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Host, "api.twitter.com") {
		switch {
		case strings.HasSuffix(r.URL.Path, "/guest/activate.json"):
			tok := j.guestTok
			if tok == "" {
				tok = "g1"
			}
			_, _ = w.Write([]byte(`{"guest_token":"` + tok + `"}`))
		case strings.HasSuffix(r.URL.Path, "/onboarding/task.json"):
			j.logins.Add(1)
			w.Header().Add("Set-Cookie", "auth_token="+reloginNewTok+"; Path=/")
			_, _ = w.Write([]byte(`{"flow_token":"f1","subtasks":[{"subtask_id":"LoginSuccessSubtask"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}
	if j.gqlHits != nil {
		j.gqlHits.Add(1)
	}
	if j.pre != nil && j.pre(w, r) {
		return
	}
	if !strings.Contains(r.Header.Get("Cookie"), "auth_token="+reloginNewTok) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"code":32,"message":"Could not authenticate you"}]}`))
		return
	}
	if j.answerAuthed != nil {
		j.answerAuthed(w, r)
		return
	}
	if r.URL.Path != "" && containsFold(r.URL.Path, "UserByScreenName") {
		_, _ = w.Write([]byte(socialUserBody))
		return
	}
	_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
}

// proxyHopFakeX stands serve up as fake X (twitterBase points at it) and aims
// the login host at the same server: twitterAPIURL becomes api.twitter.com
// over plain HTTP, which the returned BrowserClient reaches through the fake
// as a proxy hop — serve sees both branches.
func proxyHopFakeX(t *testing.T, serve http.HandlerFunc) *stealth.BrowserClient {
	t.Helper()
	_ = startFakeX(t, serve) // serves, points twitterBase at it, shortens jitter/backoff
	srvURL := strings.TrimSuffix(twitterBase, "/i/api/graphql")
	origAPI := twitterAPIURL
	twitterAPIURL = "http://api.twitter.com"
	t.Cleanup(func() { twitterAPIURL = origAPI })
	bc, err := stealth.NewClient(stealth.WithProxy(srvURL), stealth.WithoutSSRFGuard())
	require.NoError(t, err)
	return bc
}

// newReloginJourneyClient builds a Client whose single account holds the OLD
// token (loaded from the seeded store) and whose transport reaches the fake
// service, with the login host redirected through a proxy hop to the same
// server. pw is the account password ("" = token-only account).
func newReloginJourneyClient(t *testing.T, store SessionStore, pw string) (*Client, *Account, *reloginJourney) {
	t.Helper()
	j := &reloginJourney{}
	bc := proxyHopFakeX(t, j.serve)

	c, err := NewClient(ClientConfig{
		Accounts:      ParseAccounts("u1:" + pw + ":" + reloginOldTok + ":" + reloginOldCT0),
		SessionStore:  store,
		skipPoolGauge: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	adoptFakeX(c, bc)
	c.xtidMgr = xtid.NewManager(failFetcher{})
	c.cfg.DisableGuestFallback = true
	return c, c.cfg.Accounts[0], j
}

func seededStore() *recordingStore {
	rec := newRecordingStore()
	rec.data["u1"] = Session{AuthToken: reloginOldTok, CT0: reloginOldCT0, SavedAt: time.Now()}
	return rec
}

// R1 — a token-only account (no password) is told code 32; the relogin cannot
// establish a replacement. The account must keep its token and its stored
// session: the old token may only have been transiently rejected.
func TestRelogin_FailedReloginKeepsCredentialsAndSession(t *testing.T) {
	rec := seededStore()
	c, acc, j := newReloginJourneyClient(t, rec, "")

	err := getTweets(t, c)
	require.Error(t, err, "every token is rejected and the relogin cannot replace it")

	tok, ct0, _ := acc.Credentials()
	assert.Equal(t, reloginOldTok, tok, "a failed relogin must leave the auth token in place")
	assert.Equal(t, reloginOldCT0, ct0, "a failed relogin must leave ct0 in place")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, reloginOldTok, rec.data["u1"].AuthToken, "a failed relogin must leave the stored session in place")
	assert.Empty(t, rec.deletes, "a failed relogin must not delete the stored session")
	assert.Zero(t, j.logins.Load(), "no password configured: no login flow may start")
}

// R2 — the relogin succeeds: the account holds the new credentials, the store
// the new session, and the retried request goes through with the new token.
func TestRelogin_SuccessReplacesCredentialsAndPersistsSession(t *testing.T) {
	rec := seededStore()
	c, acc, j := newReloginJourneyClient(t, rec, "pw")

	require.NoError(t, getTweets(t, c))

	tok, _, _ := acc.Credentials()
	assert.Equal(t, reloginNewTok, tok, "a successful relogin must apply the new token")
	assert.EqualValues(t, 1, j.logins.Load())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, reloginNewTok, rec.data["u1"].AuthToken, "a successful relogin must persist the new session")
}

// R3 — the stale session cannot be deleted. The failed token must still not be
// re-adopted: relogin never reads the store, and the new login wins.
func TestRelogin_UndeletableStaleSessionIsNotReadopted(t *testing.T) {
	rec := seededStore()
	rec.delErr = errors.New("delete failed")
	c, acc, _ := newReloginJourneyClient(t, rec, "pw")
	rec.mu.Lock()
	rec.ops = nil // forget the construction-time load
	rec.mu.Unlock()

	require.NoError(t, getTweets(t, c))

	tok, _, _ := acc.Credentials()
	assert.Equal(t, reloginNewTok, tok, "relogin re-adopted the token that just failed")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, op := range rec.ops {
		assert.NotContains(t, op, "load:", "relogin must not read the store; ops=%v", rec.ops)
	}
}

// R3b — the replacement cannot be persisted: the stale entry is dropped so a
// restart cannot reload the token that failed.
func TestRelogin_UnpersistableReplacementDropsStaleSession(t *testing.T) {
	rec := seededStore()
	rec.saveErr = errors.New("db down")
	c, acc, _ := newReloginJourneyClient(t, rec, "pw")

	require.NoError(t, getTweets(t, c))

	tok, _, _ := acc.Credentials()
	assert.Equal(t, reloginNewTok, tok)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	_, stillThere := rec.data["u1"]
	assert.False(t, stillThere, "stale session must be dropped when the replacement could not be saved")
}
