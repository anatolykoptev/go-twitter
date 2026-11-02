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

// C2 — a transport failure on an account with no Proxy is not charged to a
// proxy: the request outcome stays "transport" (not "proxy") and no proxy
// backoff starts. With a per-account client for every account, client identity
// no longer tells whether the attempt used the account's own proxy.
//
// Mutation: client.go observedPoolReq — pass `bc != c.client` instead of
// usesOwnProxy(acc) to attemptOutcome => RED (outcome=proxy).
func TestTransportFailure_NoProxyAccountIsNotChargedToProxy(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // connection refused: isProxyError matches this text

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
	_, _, err := c.doPoolRequest(ctx, "GET", "TestEndpoint", deadURL+"/i/api/graphql/test/TestOp", nil)
	require.Error(t, err)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	all := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			all[m.Name] = m
		}
	}
	pts := histPoints(t, all, metricRequestDuration)
	require.NotEmpty(t, pts)
	for _, dp := range pts {
		assert.Equal(t, outcomeTransport, attrMap(dp.Attributes)[attrOutcome], "a no-proxy account's transport failure must not be labelled proxy")
	}

	acc.mu.Lock()
	consec, backoff := acc.proxyConsecFails, acc.proxyBackoff
	acc.mu.Unlock()
	assert.Zero(t, consec, "no proxy backoff accounting for an account without a Proxy")
	assert.True(t, backoff.IsZero(), "no proxy backoff for an account without a Proxy")
	assert.NotContains(t, alerts, "proxy.down")
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
