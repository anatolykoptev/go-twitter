package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// One malformed per-account proxy must disable ONLY that account, never the
// whole Client (go-twitter #96): pooled consumers (go-social's sync.Once-built
// client) would otherwise lose tweet publishing until restart on one bad row.
// Fail-closed is kept: an account whose pinned proxy client was never built
// has no transport at all — the pool cannot select it (IsActive) and the
// send site (clientForAccount) refuses to lend it the shared client, which
// may be direct.

// tokenRecorder is an httptest X that records every auth_token cookie it sees.
type tokenRecorder struct {
	mu     sync.Mutex
	tokens []string
	srv    *httptest.Server
}

func newTokenRecorder(t *testing.T) *tokenRecorder {
	t.Helper()
	r := &tokenRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		cookie := req.Header.Get("Cookie")
		tok := ""
		if m := authTokenCookieRe.FindString(cookie); m != "" {
			tok = strings.TrimPrefix(m, "auth_token=")
		}
		r.mu.Lock()
		r.tokens = append(r.tokens, tok)
		r.mu.Unlock()
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *tokenRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokens...)
}

// newBadProxyClient runs the real NewClient construction path with two
// accounts — baduser pinned to a malformed proxy, gooduser with none — and
// swaps the shared client for the fake-X one afterwards (the build inside
// NewClient never dials, and provided credentials make loadOrLogin offline).
func newBadProxyClient(t *testing.T, cfgMut func(*ClientConfig)) (*Client, *tokenRecorder) {
	t.Helper()
	rec := newTokenRecorder(t)
	bc := startFakeX(t, func(w http.ResponseWriter, req *http.Request) {
		cookie := req.Header.Get("Cookie")
		tok := ""
		if m := authTokenCookieRe.FindString(cookie); m != "" {
			tok = strings.TrimPrefix(m, "auth_token=")
		}
		rec.mu.Lock()
		rec.tokens = append(rec.tokens, tok)
		rec.mu.Unlock()
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
	})
	rec.srv.Close() // startFakeX owns the fake; the helper's own server is unused

	accs := ParseAccounts("baduser:pw:tokBad:ct0Bad,gooduser:pw:tokGood:ct0Good")
	accs[0].Proxy = "http://%zz"
	cfg := ClientConfig{
		Accounts:      accs,
		SessionDir:    t.TempDir(),
		skipPoolGauge: true,
	}
	if cfgMut != nil {
		cfgMut(&cfg)
	}
	c, err := NewClient(cfg)
	require.NoError(t, err, "one bad proxy must not take the whole Client down")
	t.Cleanup(func() { _ = c.Close() })
	c.client = bc
	c.cfg.DisableGuestFallback = true
	return c, rec
}

// F1 — journey: a pooled consumer builds one shared NewClient for accounts
// [A: malformed proxy, B: none]. Construction must succeed and a read must
// succeed through B: one bad row degrades to "A is out", not "client is dead".
//
// Mutation: in client.go restore `return nil, fmt.Errorf(...)` on the
// per-account build failure => RED (NewClient errors).
func TestBadProxyClient_PooledClientSurvivesOneBadProxy(t *testing.T) {
	c, rec := newBadProxyClient(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tweets, err := c.GetUserTweets(ctx, "999", 20)
	require.NoError(t, err, "a read through the healthy account must succeed")
	require.NotEmpty(t, tweets)

	accA := c.AccountByUsername("baduser")
	require.NotNil(t, accA)
	assert.False(t, accA.IsActive(), "the bad-proxy account must be unusable")
	accB := c.AccountByUsername("gooduser")
	require.NotNil(t, accB)
	assert.True(t, accB.IsActive(), "the healthy account must stay usable")

	for _, tok := range rec.seen() {
		assert.Equal(t, "tokGood", tok, "every request must go out through the healthy account")
	}
}

// F2 — journey: after construction the bad account is pushed back toward
// eligible exactly the way the pool's own machinery would — a past
// ReactivateAt, then Next's auto-reactivate — and then it is forced through a
// caller-pinned path (doPOST). Its auth_token must never egress directly: the
// pinned call fails with the sentinel, and the pool read rotates to B.
//
// Mutation: in client.go clientForAccount drop the ErrProxyClientUnavailable
// arm so a missing per-account client falls back to the shared one => RED
// (doPOST egresses tokBad through the direct shared client).
func TestBadProxyClient_BadAccountNeverEgressesDirectly(t *testing.T) {
	c, rec := newBadProxyClient(t, nil)

	accA := c.AccountByUsername("baduser")
	require.NotNil(t, accA)

	// The real reactivation: a past-due ReactivateAt is what pool.Next
	// auto-restores to active on the next pick.
	accA.SetReactivateAt(time.Now().Add(-time.Second))
	_, _ = c.pool.Next(nil)
	accA.mu.Lock()
	rawActive := accA.active
	accA.mu.Unlock()
	require.True(t, rawActive, "sanity: the pool's auto-reactivation did flip the raw flag")
	assert.False(t, accA.IsActive(),
		"a missing per-account transport must keep the account unselectable even reactivated")

	// A pooled read must rotate to B.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.GetUserTweets(ctx, "999", 20)
	require.NoError(t, err, "the pool must serve the healthy account")

	// Force A through a caller-pinned path: no pool selection involved, only
	// the send-site guard stands between its cookies and this host's IP.
	_, err = c.doPOST(ctx, accA, "CreateTweet", twitterBase+"/x/CreateTweet", []byte(`{}`))
	assert.True(t, errors.Is(err, ErrProxyClientUnavailable),
		"a pinned call on the bad account must fail with the sentinel, got %v", err)

	for _, tok := range rec.seen() {
		assert.Equal(t, "tokGood", tok,
			"the bad account's auth_token must never reach X through the shared client")
	}
}

// F3 — journey: every account's per-account proxy fails to build. With no
// usable account NewClient must still fail — a zero-transport client would
// silently serve nothing while looking constructed.
//
// Mutation: in client.go NewClient drop the "no usable account" check => RED
// (construction succeeds).
func TestBadProxyClient_AllAccountsBadNewClientFails(t *testing.T) {
	accs := ParseAccounts("a:pw:tokA:ct0A,b:pw:tokB:ct0B")
	accs[0].Proxy = "http://%zz"
	accs[1].Proxy = "http://%zz"

	c, err := NewClient(ClientConfig{
		Accounts:      accs,
		SessionDir:    t.TempDir(),
		skipPoolGauge: true,
	})
	require.Error(t, err, "no usable account must still fail NewClient")
	assert.Nil(t, c)
}

// F4 — journey: the *WithSocial shape — one account per Client — with a bad
// proxy. NewClient must return an error exactly as before.
//
// Mutation: same as F3 (drop the "no usable account" check) => RED.
func TestBadProxyClient_SingleAccountBadNewClientFails(t *testing.T) {
	accs := ParseAccounts("leased:pw:tokL:ct0L")
	accs[0].Proxy = "http://%zz"

	c, err := NewClient(ClientConfig{
		Accounts:      accs,
		SessionDir:    t.TempDir(),
		skipPoolGauge: true,
	})
	require.Error(t, err, "the single-account path must keep failing on a bad proxy")
	assert.Nil(t, c)
}

// F5 — journey: a consumer watching PoolAlertHook (and the OTel counter) sees
// exactly one account.proxy_client_failed for A — observable without grepping
// logs, per the issue's alert-on-proxy-failures ask.
//
// Mutation: in client.go NewClient drop the alertHook + metric record for a
// failed build => RED (no alert, counter stays 0).
func TestBadProxyClient_FailureAlertFiresOnceForBadAccount(t *testing.T) {
	type alert struct {
		topic   string
		payload any
	}
	var alerts []alert
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	newBadProxyClient(t, func(cfg *ClientConfig) {
		cfg.MeterProvider = mp
		cfg.PoolAlertHook = func(topic string, payload any) {
			alerts = append(alerts, alert{topic, payload})
		}
	})

	var proxyClientFailed []alert
	for _, a := range alerts {
		if a.topic == "account.proxy_client_failed" {
			proxyClientFailed = append(proxyClientFailed, a)
		}
	}
	require.Len(t, proxyClientFailed, 1, "exactly one account.proxy_client_failed alert must fire")
	m, ok := proxyClientFailed[0].payload.(map[string]any)
	require.True(t, ok, "payload must be map[string]any")
	assert.Equal(t, "baduser", m["user"])
	assert.NotContains(t, m["proxy"], "%zz", "the payload proxy must be masked, got %v", m["proxy"])
	assert.NotContains(t, m["error"], "%zz", "the payload error must not embed the raw proxy URL, got %v", m["error"])

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var got int64
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != metricProxyClientFailures {
				continue
			}
			s, ok := mm.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s must be an Int64 sum", metricProxyClientFailures)
			for _, dp := range s.DataPoints {
				got += dp.Value
			}
		}
	}
	assert.Equal(t, int64(1), got, "%s must record the one failed build", metricProxyClientFailures)
}
