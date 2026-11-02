package twitter

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/social"
	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connCount counts the connections a loopback server holds open, through
// http.Server.ConnState.
type connCount struct {
	mu   sync.Mutex
	open map[net.Conn]struct{}
}

func (cc *connCount) state(c net.Conn, s http.ConnState) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	switch s {
	case http.StateNew:
		cc.open[c] = struct{}{}
	case http.StateClosed, http.StateHijacked:
		delete(cc.open, c)
	}
}

func (cc *connCount) n() int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return len(cc.open)
}

// waitOpen polls until exactly want connections are open, or fails after 5 s.
func (cc *connCount) waitOpen(t *testing.T, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for cc.n() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d connections open, want %d", what, cc.n(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startCountedX is a fake x.com on loopback that counts open connections and
// points twitterBase at itself. Paths under /gate block until gate is closed.
func startCountedX(t *testing.T, gate <-chan struct{}) (*httptest.Server, *connCount) {
	t.Helper()
	cc := &connCount{open: map[net.Conn]struct{}{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/gate"):
			<-gate
			_, _ = w.Write([]byte("ok"))
		case strings.Contains(req.URL.Path, "UserByScreenName"):
			_, _ = w.Write([]byte(socialUserBody))
		case strings.Contains(req.URL.Path, "/i/api/graphql"):
			_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	srv.Config.ConnState = cc.state
	srv.Start()
	t.Cleanup(srv.Close)

	origBase, origJitter, origBackoff := twitterBase, stealth.DefaultJitter, stealth.DefaultBackoff
	twitterBase = srv.URL + "/i/api/graphql"
	stealth.DefaultJitter.Min, stealth.DefaultJitter.Max = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { twitterBase, stealth.DefaultJitter, stealth.DefaultBackoff = origBase, origJitter, origBackoff })
	return srv, cc
}

// offlineClient makes c touch nothing outside loopback: offline xtid, no pacer,
// and a disabled queryId refresher.
func offlineClient(t *testing.T, c *Client) {
	t.Helper()
	c.xtidMgr = xtid.NewManager(failFetcher{})
	c.accountPacer = nil
	c.qid = newQIDRefresher(qidConfig{disabled: true})
	c.warmDoerOverride = &fakeWarmBackend{} // authenticated warm fetches must not reach the real x.com
}

func newCountedClient(t *testing.T, accounts string) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{
		Accounts:             ParseAccounts(accounts),
		SessionDir:           t.TempDir(),
		skipPoolGauge:        true,
		DisableGuestFallback: true,
		stealthOpts:          []stealth.ClientOption{stealth.WithoutSSRFGuard()}, // test-only: loopback
	})
	require.NoError(t, err)
	offlineClient(t, c)
	return c
}

func sharedGet(t *testing.T, c *Client, srv *httptest.Server) {
	t.Helper()
	_, _, code, err := c.client.Do("GET", srv.URL+"/shared", nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
}

// Close must release the idle keep-alive connections of every HTTP client the
// Client built: the shared one and the account's. Without it each short-lived
// Client leaves its sockets to idle out and the next one pays a fresh TLS
// handshake.
//
// Mutation: client.go NewClient — delete `owned.add(bc)` (shared client) => RED
// at "shared + account client idle".. Close; delete `owned.add(accClient)` (the
// per-account loop) => RED likewise.
func TestClientClose_ReleasesSharedAndAccountConnections(t *testing.T) {
	srv, cc := startCountedX(t, nil)
	c := newCountedClient(t, "a:pw:tokA:ct0A")

	sharedGet(t, c, srv)
	_, err := c.GetUserTweets(context.Background(), "999", 20)
	require.NoError(t, err)
	cc.waitOpen(t, 2, "precondition: shared + account client each hold one idle connection")

	require.NoError(t, c.Close())
	cc.waitOpen(t, 0, "after Close")

	// Safe twice, safe on nil, and the Client keeps working.
	require.NoError(t, c.Close())
	require.NoError(t, (*Client)(nil).Close())
	sharedGet(t, c, srv)
	cc.waitOpen(t, 1, "a Client stays usable after Close")
	require.NoError(t, c.Close())
	cc.waitOpen(t, 0, "second Close releases the new connection")
}

// An account added after NewClient gets its client lazily; Close must release
// that one too.
//
// Mutation: client.go clientForAccount — delete `c.owned.add(bc)` => RED
// ("after Close: 1 connections open, want 0").
func TestClientClose_ReleasesLazilyBuiltAccountClient(t *testing.T) {
	srv, cc := startCountedX(t, nil)
	c := newCountedClient(t, "a:pw:tokA:ct0A")

	late := ParseAccounts("late:pw:tokL:ct0L")[0]
	late.active = true
	c.Pool().Add(late)
	bc, err := c.clientForAccount(late)
	require.NoError(t, err)
	_, _, code, err := bc.Do("GET", srv.URL+"/late", nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	cc.waitOpen(t, 1, "precondition: the lazy account client holds one idle connection")

	require.NoError(t, c.Close())
	cc.waitOpen(t, 0, "after Close")
}

// An Account handed to two Clients carries only the client stored last. Each
// Client must release what IT built, and a Client must never touch a client it
// did not build.
func TestClientClose_AccountSharedByTwoClients(t *testing.T) {
	srv, cc := startCountedX(t, nil)
	accs := ParseAccounts("a:pw:tokA:ct0A")
	mk := func() *Client {
		c, err := NewClient(ClientConfig{
			Accounts: accs, SessionDir: t.TempDir(), skipPoolGauge: true, DisableGuestFallback: true,
			stealthOpts: []stealth.ClientOption{stealth.WithoutSSRFGuard()},
		})
		require.NoError(t, err)
		offlineClient(t, c)
		return c
	}
	c1 := mk()
	_, _, _, err := c1.client.Do("GET", srv.URL+"/c1", nil, nil)
	require.NoError(t, err)
	c2 := mk() // overwrites accs[0].client
	_, _, _, err = c2.client.Do("GET", srv.URL+"/c2", nil, nil)
	require.NoError(t, err)
	cc.waitOpen(t, 2, "precondition")

	require.NoError(t, c1.Close())
	cc.waitOpen(t, 1, "c1.Close releases only c1's connection")
	require.NoError(t, c2.Close())
	cc.waitOpen(t, 0, "c2.Close releases c2's")
}

// socialFixture is a fake go-social lease server plus the process-wide state
// the *WithSocial path shares, kept offline.
func socialFixture(t *testing.T) (*social.Client, *xtid.Manager, *fakeWarmBackend, []ClientOption) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // sessions are persisted under $HOME
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(social.Credentials{
				ID:          "acc-1",
				Credentials: map[string]string{"username": "user_a", "auth_token": "toka", "ct0": "ct0a"},
			})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	shared := xtid.NewManager(failFetcher{})
	opts := []ClientOption{func(cfg *ClientConfig) {
		cfg.stealthOpts = []stealth.ClientOption{stealth.WithoutSSRFGuard()}
		cfg.sharedXtid = shared
	}}
	warm := &fakeWarmBackend{}
	testHookSocialClient = func(c *Client) {
		c.accountPacer = nil
		c.qid = newQIDRefresher(qidConfig{disabled: true})
		c.warmDoerOverride = warm // authenticated warm fetches must not reach the real x.com
	}
	t.Cleanup(func() { testHookSocialClient = nil })
	return social.NewClient(srv.URL, "tok", "test"), shared, warm, opts
}

// The real call site: each *WithSocial call builds a Client and defers Close.
// After the call returns no connection of that call may stay open, and the
// process-wide xtid manager must keep serving the next call.
//
// Mutation: social.go tryWithAccount — delete `defer func() { _ = tw.Close() }()`
// => RED ("after the call returns: 2 connections open, want 0").
func TestWithSocial_ClosesIdleConnectionsAfterEveryCall(t *testing.T) {
	_, cc := startCountedX(t, nil)
	sc, shared, warm, opts := socialFixture(t)

	var mgrs []*xtid.Manager
	for call := 1; call <= 2; call++ {
		open := -1
		_, err := withSocialAccount(context.Background(), sc, "UserTweets", func(ctx context.Context, tw *Client) ([]*Tweet, error) {
			mgrs = append(mgrs, tw.xtidMgr)
			res, err := tw.GetUserTweets(ctx, "999", 20)
			_, _, _, _ = tw.client.Do("GET", twitterBase+"/shared", nil, nil)
			open = cc.n()
			return res, err
		}, opts...)
		require.NoError(t, err, "call %d", call)
		require.GreaterOrEqual(t, open, 1, "precondition: the call held idle connections")
		cc.waitOpen(t, 0, "after the call returns")
	}
	require.Len(t, mgrs, 2)
	assert.Same(t, shared, mgrs[0], "the shared xtid manager serves call 1")
	assert.Same(t, shared, mgrs[1], "Close must leave the shared manager in place for call 2")
	warm.mu.Lock()
	defer warm.mu.Unlock()
	assert.Equal(t, 1, warm.hits["https://x.com/tesla"], "the shared manager initialised once; call 2 reused its keys")
}

// gateFetcher stands in for the warm-page fetcher of a queryId refresh: its
// first fetch is a real request through the Client's own account client that
// parks on the server, then it serves the fake bundle.
type gateFetcher struct {
	bc      *stealth.BrowserClient
	url     string
	started chan struct{}
	once    sync.Once
	inner   bundle.Fetcher
	err     error
}

func (g *gateFetcher) Fetch(ctx context.Context, u string) ([]byte, error) {
	g.once.Do(func() {
		close(g.started)
		_, _, _, g.err = g.bc.Do("GET", g.url, nil, nil)
	})
	if g.err != nil {
		return nil, g.err
	}
	return g.inner.Fetch(ctx, u)
}

// Proves: a queryId refresh that is in flight on the Client's account client
// when Client.Close runs succeeds and records no failure (no failing state, no
// backoff streak, no alert). The refresh's first fetch is a real request parked
// on the loopback server across the Close.
//
// It does not prove that Close avoids BrowserClient.Close: on loopback that
// variant also passes this test. It says nothing about the xtid refresh.
func TestClientClose_BackgroundRefreshInFlightIsNotFailed(t *testing.T) {
	gate := make(chan struct{})
	srv, cc := startCountedX(t, gate)
	r, _ := useQIDRefresher(t)
	var alerts []string
	var alertMu sync.Mutex
	r.setAlertHook(func(topic string, _ any) { alertMu.Lock(); alerts = append(alerts, topic); alertMu.Unlock() })

	c := newCountedClient(t, "a:pw:tokA:ct0A")
	c.qid = nil // the process-wide refresher installed above
	w := newQIDWorld(t)
	g := &gateFetcher{
		bc: c.Pool().Items()[0].client, url: srv.URL + "/gate", started: make(chan struct{}),
		inner: &bundle.StealthFetcher{Client: w, UserAgent: defaultUserAgent},
	}

	r.onAuthedRequest(g, c.metrics) // fires the first-use refresh in the background
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the background refresh never started")
	}
	cc.waitOpen(t, 1, "precondition: the refresh holds a request in flight")

	require.NoError(t, c.Close()) // mid-flight
	close(gate)
	r.wg.Wait()

	require.NoError(t, g.err, "the in-flight request must not be failed by Close")
	assert.NotZero(t, r.lastSuccessNS.Load(), "the refresh must complete successfully")
	assert.Equal(t, 1, w.bundleFetches(), "the refresh walked the bundle")
	r.mu.Lock()
	failing, streak := r.failing, r.streak
	r.mu.Unlock()
	assert.False(t, failing, "no refresh failure may be recorded")
	assert.Zero(t, streak)
	alertMu.Lock()
	assert.Empty(t, alerts, "no alert may fire")
	alertMu.Unlock()
}
