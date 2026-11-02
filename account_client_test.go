package twitter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Every account owns its HTTP client, hence its cookie jar. fhttp appends jar
// cookies to the explicit cookie header, so with one shared client a set-cookie
// on a response to account A rode on account B's next request.

// cookieSeen is one request the fake X received: who sent it (the auth_token of
// the explicit cookie header) and the whole Cookie header.
type cookieSeen struct{ token, cookie string }

// startRotatingCookieX starts a fake X that answers every authenticated read
// with a set-cookie pair derived from the caller's auth_token, and records the
// Cookie header of every request. twitterBase points at it for the test.
func startRotatingCookieX(t *testing.T) (record func() []cookieSeen) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []cookieSeen
	)
	_ = startFakeX(t, func(w http.ResponseWriter, req *http.Request) {
		cookie := req.Header.Get("Cookie")
		tok := strings.TrimPrefix(authTokenCookieRe.FindString(cookie), "auth_token=")
		mu.Lock()
		seen = append(seen, cookieSeen{token: tok, cookie: cookie})
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "ct0", Value: "FROM" + tok, Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "twid", Value: "u%3D" + tok, Path: "/"})
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
	})
	return func() []cookieSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]cookieSeen(nil), seen...)
	}
}

// newLoopbackClient runs the real NewClient with the SSRF guard off on every
// client it builds (the fake X is loopback) and offline xtid.
func newLoopbackClient(t *testing.T, cfg ClientConfig) *Client {
	t.Helper()
	cfg.SessionDir = t.TempDir()
	cfg.skipPoolGauge = true
	cfg.DisableGuestFallback = true
	cfg.stealthOpts = []stealth.ClientOption{stealth.WithoutSSRFGuard()} // test-only
	c, err := NewClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.xtidMgr = xtid.NewManager(failFetcher{})
	c.accountPacer = nil
	c.qid = newQIDRefresher(qidConfig{disabled: true})
	c.warmDoerOverride = &fakeWarmBackend{} // warm fetches must not reach the real x.com
	return c
}

// C1 — journey: an operator runs two accounts without proxies in one Client.
// A's response carries set-cookie; B then makes a request. B's request must
// carry only B's cookies.
//
// Mutation: client.go NewClient — restore `if acc.Proxy == "" { usable++; continue }`
// at the top of the per-account loop => RED (B's Cookie contains A's FROM/twid).
func TestNewClient_AccountsDoNotShareCookies(t *testing.T) {
	record := startRotatingCookieX(t)
	c := newLoopbackClient(t, ClientConfig{Accounts: ParseAccounts("a:pw:tokA:ct0A,b:pw:tokB:ct0B")})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 8 && len(distinctTokens(record())) < 2; i++ {
		_, err := c.GetUserTweets(ctx, "999", 20)
		require.NoError(t, err)
	}
	reqs := record()
	require.GreaterOrEqual(t, len(distinctTokens(reqs)), 2, "the pool must have used both accounts, else the test proves nothing")

	// Every request after the first response (which set cookies for its sender)
	// is checked against every other account's values.
	for i, r := range reqs {
		for _, other := range []string{"tokA", "tokB"} {
			if other == r.token {
				continue
			}
			assert.NotContains(t, r.cookie, "FROM"+other, "request %d from %s carries a cookie set for %s", i, r.token, other)
			assert.NotContains(t, r.cookie, "u%3D"+other, "request %d from %s carries a cookie set for %s", i, r.token, other)
		}
	}
}

func distinctTokens(reqs []cookieSeen) map[string]bool {
	out := map[string]bool{}
	for _, r := range reqs {
		out[r.token] = true
	}
	return out
}

// An account without its own Proxy gets a client of its own and never the
// shared one; the shared client stays for the account-less guest paths.
func TestNewClient_EveryAccountOwnsAClient(t *testing.T) {
	c := newLoopbackClient(t, ClientConfig{Accounts: ParseAccounts("a:pw:tokA:ct0A,b:pw:tokB:ct0B")})
	a, b := c.AccountByUsername("a"), c.AccountByUsername("b")
	require.NotNil(t, a.client)
	require.NotNil(t, b.client)
	assert.NotSame(t, a.client, b.client)
	assert.NotSame(t, c.client, a.client)
	got, err := c.clientForAccount(a)
	require.NoError(t, err)
	assert.Same(t, a.client, got)
}

// Fact for the record (not a regression gate): what ONE account sends on its
// second request after a response rotated ct0 through set-cookie.
func TestNewClient_SingleAccountSecondRequestAfterCt0Rotation(t *testing.T) {
	record := startRotatingCookieX(t)
	c := newLoopbackClient(t, ClientConfig{Accounts: ParseAccounts("a:pw:tokA:ct0A")})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		_, err := c.GetUserTweets(ctx, "999", 20)
		require.NoError(t, err)
	}
	reqs := record()
	require.Len(t, reqs, 2)
	t.Logf("request 1 Cookie: %s", reqs[0].cookie)
	t.Logf("request 2 Cookie: %s", reqs[1].cookie)
}

// requestOutcomes drives one failing attempt (connection refused, which
// isProxyError matches) from a no-proxy account's own client through drive and
// returns the account, the twitter.outcome label of every request.duration
// point and the alert topics seen.
func requestOutcomes(t *testing.T, drive func(ctx context.Context, c *Client, acc *Account, deadURL string)) (*Account, []string, []string) {
	t.Helper()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	c := newLoopbackClient(t, ClientConfig{Accounts: ParseAccounts("a:pw:tokA:ct0A"), MeterProvider: mp})
	acc := c.AccountByUsername("a")
	require.NotNil(t, acc.client)
	require.NotSame(t, c.client, acc.client, "precondition: the account's client is not the shared one")
	var alerts []string
	c.alertHook = func(topic string, _ any) { alerts = append(alerts, topic) }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drive(ctx, c, acc, deadURL)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	all := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			all[m.Name] = m
		}
	}
	var labels []string
	for _, dp := range histPoints(t, all, metricRequestDuration) {
		labels = append(labels, attrMap(dp.Attributes)[attrOutcome])
	}
	return acc, labels, alerts
}

func assertNotChargedToProxy(t *testing.T, acc *Account, labels, alerts []string) {
	t.Helper()
	require.NotEmpty(t, labels)
	for _, l := range labels {
		assert.Equal(t, outcomeTransport, l, "a no-proxy account's transport failure must not be labelled proxy")
	}
	acc.mu.Lock()
	consec, backoff := acc.proxyConsecFails, acc.proxyBackoff
	acc.mu.Unlock()
	assert.Zero(t, consec, "no proxy backoff accounting for an account without a Proxy")
	assert.True(t, backoff.IsZero(), "no proxy backoff for an account without a Proxy")
	assert.NotContains(t, alerts, "proxy.down")
}

// C2 — a transport failure on an account with no Proxy is not charged to a
// proxy: the request outcome stays "transport" (not "proxy") and no proxy
// backoff starts. With a per-account client for every account, client identity
// no longer tells whether the attempt used the account's own proxy. One test per
// attemptOutcome call site.
//
// Mutation: client.go observedPoolReq / observedRequestWithBody /
// observedMediaReq — pass `bc != c.client` instead of usesOwnProxy(acc) to
// attemptOutcome => RED at that site's test (outcome=proxy).
func TestTransportFailure_NoProxyAccountIsNotChargedToProxy(t *testing.T) {
	acc, labels, alerts := requestOutcomes(t, func(ctx context.Context, c *Client, _ *Account, deadURL string) {
		_, _, err := c.doPoolRequest(ctx, "GET", "TestEndpoint", deadURL+"/i/api/graphql/test/TestOp", nil)
		require.Error(t, err)
	})
	assertNotChargedToProxy(t, acc, labels, alerts)
}

func TestTransportFailure_NoProxyAccountIsNotChargedToProxy_RequestWithBody(t *testing.T) {
	acc, labels, alerts := requestOutcomes(t, func(ctx context.Context, c *Client, acc *Account, deadURL string) {
		_, _, _, err := c.observedRequestWithBody(ctx, acc, "TestEndpoint", acc.client, "POST", deadURL+"/x", map[string]string{}, strings.NewReader("a=b"))
		require.Error(t, err)
	})
	assertNotChargedToProxy(t, acc, labels, alerts)
}

func TestTransportFailure_NoProxyAccountIsNotChargedToProxy_Media(t *testing.T) {
	acc, labels, alerts := requestOutcomes(t, func(ctx context.Context, c *Client, acc *Account, deadURL string) {
		_, _, _, err := c.observedMediaReq(ctx, acc, acc.client, "POST", deadURL+"/x", []byte("a=b"), map[string]string{})
		require.Error(t, err)
	})
	assertNotChargedToProxy(t, acc, labels, alerts)
}

// C3 — with every account now building a client, a malformed pinned proxy still
// disables only its own account (the healthy one keeps its own client), and
// NewClient still fails when no account is usable.
//
// Mutation: client.go NewClient — remove the acc.markProxyClientFailed() call
// => RED (the reactivated bad account reports IsActive true).
func TestNewClient_BuildFailureDisablesOnlyThatAccount(t *testing.T) {
	accs := ParseAccounts("bad:pw:tokBad:ct0Bad,good:pw:tokGood:ct0Good")
	accs[0].Proxy = "http://%zz"
	c, err := NewClient(ClientConfig{Accounts: accs, SessionDir: t.TempDir(), skipPoolGauge: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	bad, good := c.AccountByUsername("bad"), c.AccountByUsername("good")
	assert.False(t, bad.IsActive(), "the account whose client failed to build must be unusable")
	// The pool's own reactivation (a past ReactivateAt, then Next) flips active
	// back on; only the permanent proxyClientFailed mark keeps the account out.
	bad.SetActive(true)
	assert.False(t, bad.IsActive(), "a reactivated account without a client must stay unselectable")
	assert.Nil(t, bad.client)
	_, err = c.clientForAccount(bad)
	assert.ErrorIs(t, err, ErrProxyClientUnavailable)
	assert.True(t, good.IsActive(), "the healthy account must stay usable")
	require.NotNil(t, good.client, "the healthy account owns its client")

	all := ParseAccounts("a:pw:tokA:ct0A,b:pw:tokB:ct0B")
	all[0].Proxy, all[1].Proxy = "http://%zz", "http://%zz"
	c2, err := NewClient(ClientConfig{Accounts: all, SessionDir: t.TempDir(), skipPoolGauge: true})
	require.Error(t, err, "no usable account must still fail NewClient")
	assert.Nil(t, c2)
}

// addPoolAccount adds an account the way a consumer does after NewClient:
// through Pool().Add, with the per-account state NewClient sets for its own.
func addPoolAccount(t *testing.T, c *Client, spec string) *Account {
	t.Helper()
	acc := ParseAccounts(spec)[0]
	acc.active = true
	acc.installRateLimiter(c.cfg.RateLimit)
	seedRateLimits(acc)
	acc.HealthTracker = pool.DefaultHealthTracker()
	c.pool.Add(acc)
	return acc
}

// C4 — journey: accounts added after NewClient (loginOpenAccount, Pool().Add)
// have no client of their own. They must get one on first use, never the shared
// client whose jar the guest paths use: neither account sees the other's
// cookies and a guest request sees neither.
//
// Mutation: client.go clientForAccount — return c.client when acc.client is nil
// instead of building => RED (B's Cookie carries A's values).
func TestClientForAccount_PoolAddedAccountsDoNotShareJar(t *testing.T) {
	record := startRotatingCookieX(t)
	c := newLoopbackClient(t, ClientConfig{})
	a := addPoolAccount(t, c, "a:pw:tokA:ct0A")
	b := addPoolAccount(t, c, "b:pw:tokB:ct0B")
	require.Nil(t, a.client, "precondition: NewClient never built a client for it")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 8 && len(distinctTokens(record())) < 2; i++ {
		_, err := c.GetUserTweets(ctx, "999", 20)
		require.NoError(t, err)
	}
	require.GreaterOrEqual(t, len(distinctTokens(record())), 2, "the pool must have used both accounts")
	require.NotNil(t, a.client)
	require.NotNil(t, b.client)
	assert.NotSame(t, a.client, b.client)
	assert.NotSame(t, c.client, a.client)

	// An account-less (guest) request goes through the shared client.
	_, _, _, err := c.observedRequest(ctx, nil, "Guest", c.client, "GET", twitterBase+"/guest", map[string]string{})
	require.NoError(t, err)

	reqs := record()
	for i, r := range reqs {
		for _, other := range []string{"tokA", "tokB"} {
			if other == r.token {
				continue
			}
			assert.NotContains(t, r.cookie, "FROM"+other, "request %d from %q carries a cookie set for %s", i, r.token, other)
			assert.NotContains(t, r.cookie, "u%3D"+other, "request %d from %q carries a cookie set for %s", i, r.token, other)
		}
	}
	guest := reqs[len(reqs)-1]
	require.Empty(t, guest.token, "the last request is the guest one")
	assert.NotContains(t, guest.cookie, "FROM", "the guest request must carry no account's cookie")
	assert.NotContains(t, guest.cookie, "twid")
}

// Concurrent first use of an account without a client builds exactly one.
//
// Mutation: client.go clientForAccount — drop the acc.mu.Lock()/Unlock() pair
// => RED (several distinct clients returned; -race also flags the field).
func TestClientForAccount_ConcurrentFirstUseBuildsOneClient(t *testing.T) {
	c := newLoopbackClient(t, ClientConfig{})
	acc := addPoolAccount(t, c, "a:pw:tokA:ct0A")

	const n = 64
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		got   = make([]*stealth.BrowserClient, n)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			bc, err := c.clientForAccount(acc)
			assert.NoError(t, err)
			got[i] = bc
		}()
	}
	close(start)
	wg.Wait()
	for _, bc := range got {
		require.NotNil(t, bc)
		assert.Same(t, got[0], bc, "every concurrent first use must see the one client")
	}
}

// Fail-closed on the lazy path: a build failure marks only this account,
// returns ErrProxyClientUnavailable, never the shared client, and the alert
// payload does not talk about a proxy the account does not have.
//
// Mutation: client.go clientForAccount — drop `acc.proxyClientFailed = true`
// => RED (the reactivated account reports IsActive).
func TestClientForAccount_LazyBuildFailureIsFailClosed(t *testing.T) {
	c := newLoopbackClient(t, ClientConfig{})
	good := addPoolAccount(t, c, "good:pw:tokG:ct0G")
	bad := addPoolAccount(t, c, "bad:pw:tokB:ct0B")
	var payloads []map[string]any
	c.alertHook = func(topic string, payload any) {
		if topic == "account.proxy_client_failed" {
			payloads = append(payloads, payload.(map[string]any))
		}
	}

	c.cfg.stealthOpts = []stealth.ClientOption{stealth.WithProxy("http://%zz")} // makes the build fail
	bc, err := c.clientForAccount(bad)
	require.ErrorIs(t, err, ErrProxyClientUnavailable)
	assert.Nil(t, bc, "the shared client must never be lent to an account")
	bad.SetActive(true)
	assert.False(t, bad.IsActive(), "a failed account stays unselectable even when reactivated")
	_, err = c.clientForAccount(bad)
	assert.ErrorIs(t, err, ErrProxyClientUnavailable, "a failed account is not rebuilt")

	require.Len(t, payloads, 1)
	assert.Equal(t, "bad", payloads[0]["user"])
	assert.Equal(t, "direct", payloads[0]["proxy"], "no proxy was configured: the payload must not claim an unparseable proxy URL")
	assert.NotContains(t, payloads[0]["error"], "proxy")

	c.cfg.stealthOpts = []stealth.ClientOption{stealth.WithoutSSRFGuard()}
	_, err = c.clientForAccount(good)
	assert.NoError(t, err, "the other account is unaffected")
	assert.True(t, good.IsActive())
}
