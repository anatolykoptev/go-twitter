package twitter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Endpoint-wide faults (go-twitter #61): a 404 or GraphQL-shaped 400 means the
// committed request shape is stale for EVERY account (dead queryId, missing
// xtid, feature mismatch); a 5xx or LoadShed entry means X itself is shedding
// load. Neither is attributable to the serving account: reads rotate within the
// attempt budget, account health is never charged, and writes are never
// replayed on another account.

// F1 — journey: X ships a deploy, the SearchTimeline queryId goes stale, every
// account gets a 404. After more calls than the soft-deactivation threshold
// (5 consecutive failures per account) every account must still be active with
// zero charged failures.
//
// Mutation: in request.go doPoolRequest's `status != 200` branch, restore
// `acc.RecordFailure()` on the endpoint-fault arm => RED.
func TestEndpointFault_404NeverChargedToAccount(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.status.Store(http.StatusNotFound)
	fake.body = `{"errors":[{"message":"Not Found"}]}`

	c, accs := newPoolWaitClient(t, 2)

	origBase := twitterBase
	twitterBase = fake.srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const calls = 12 // > 5 consecutive failures needed to trip one account
	for i := 0; i < calls; i++ {
		_, err := c.SearchTimeline(ctx, "golang", 10)
		require.Error(t, err, "call %d", i)
		assert.True(t, errors.Is(err, ErrEndpoint), "call %d: want endpoint fault, got %v", i, err)
	}

	for _, a := range accs {
		total, failed, _ := a.Stats()
		assert.Zero(t, failed, "account %s must not be charged endpoint faults", a.Username)
		assert.Zero(t, total, "account %s: endpoint faults are not the account's requests at all", a.Username)
		assert.True(t, a.IsActive(), "account %s must never soft-deactivate on endpoint faults", a.Username)
	}
	assert.EqualValues(t, calls*len(accs), fake.hits.Load(),
		"each account serves a deterministically-dead request at most once per call")
}

// F2 — journey: one account gets the fault, the next answers 200 — the read
// succeeds with the healthy account's data.
//
// Mutation: in request.go doPoolRequest endpoint-fault arm replace `continue`
// with `return nil, nil, apiErr` => RED.
func TestEndpointFault_RotatesToHealthyAccount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"stale queryId 404", http.StatusNotFound, `{"errors":[{"message":"Not Found"}]}`},
		{"graphql-shaped 400", http.StatusBadRequest, `{"errors":[{"code":336,"message":"The following features cannot be null"}]}`},
		{"503 overload", http.StatusServiceUnavailable, `upstream connect error`},
		{"LoadShed on a 200", http.StatusOK, `{"errors":[{"code":-1,"message":"LoadShed"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hitsA, hitsB atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.Header.Get("Cookie"), "auth_token=tokA") {
					hitsA.Add(1)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				hitsB.Add(1)
				_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
			}))
			t.Cleanup(srv.Close)

			c, accs := newPoolWaitClient(t, 2)
			accs[0].AuthToken, accs[1].AuthToken = "tokA", "tokB" // pool.Next picks accs[0] first

			origBase := twitterBase
			twitterBase = srv.URL + "/i/api/graphql"
			t.Cleanup(func() { twitterBase = origBase })

			tweets, err := c.GetUserTweets(context.Background(), "999", 20)
			require.NoError(t, err, "the call must succeed on the healthy account")
			require.Len(t, tweets, 1, "the response must carry the second account's data")

			assert.EqualValues(t, 1, hitsA.Load(), "the faulted account sees exactly one attempt")
			assert.EqualValues(t, 1, hitsB.Load())
			_, failedA, _ := accs[0].Stats()
			assert.Zero(t, failedA, "the faulted account must not be charged")
			assert.True(t, accs[0].IsActive())
		})
	}
}

// F3 — journey: a *WithSocial read against fake go-social + fake X answering
// 404. Endpoint faults must reach go-social as "success" (the credential
// worked; the endpoint shape is dead for everyone) — never "auth_error", which
// would poison a healthy account in the shared credential pool.
//
// Mutation: in social.go tryWithAccount drop the IsEndpointFault arm so the
// error falls through to the "auth_error" report => RED.
func TestSocial_EndpointFault_NeverReportsAuthError(t *testing.T) {
	r := newSocialRigX(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Not Found"}]}`))
	})

	_, err := GetUserWithSocial(context.Background(), r.sc, "tesla")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEndpoint), "want endpoint fault, got %v", err)
	assert.EqualValues(t, socialEndpointFaultStop, r.acquire.Load(),
		"two different leases both faulting proves the fault endpoint-wide: the call stops early")

	r.reportsMu.Lock()
	defer r.reportsMu.Unlock()
	require.NotEmpty(t, r.reports, "every lease must be reported")
	for i, s := range r.reports {
		assert.Equal(t, "success", s, "report %d: an endpoint fault must not be reported as %q", i, s)
	}
}

// F4 — a 200 carrying `errors` AND usable `data` counts as success, but only
// for X-side classes (131 internal, LoadShed, endpoint fault): an
// account-dead verdict riding alongside data takes its class arm. A 200 with
// errors and an empty/null/no "data" field still fails with its class.
//
// Mutation: in request.go doPoolRequest drop `&& hasResponseData(body)` from
// the errors+data arm (accept any 200-with-errors) => RED on the empty-data side.
func TestEndpointFault_ErrorsPlusDataSucceeds_ErrorsOnlyFails(t *testing.T) {
	t.Run("errors plus data succeeds", func(t *testing.T) {
		fake := newPoolWaitFake(t)
		// 131 (internal) alongside usable timeline data — an X-side class.
		fake.body = strings.TrimSuffix(userTweetsBody(snowflakeAt(time.Now())), "}") +
			`,"errors":[{"code":131,"message":"Internal error"}]}`

		c, _ := newPoolWaitClient(t, 2)
		body, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())
		require.NoError(t, err)
		assert.Contains(t, string(body), `"errors"`, "the caller still receives the full body")
	})

	t.Run("errors without data still fails", func(t *testing.T) {
		fake := newPoolWaitFake(t)
		fake.body = `{"errors":[{"code":179,"message":"Not authorized"}]}`

		c, _ := newPoolWaitClient(t, 2)
		_, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())
		require.Error(t, err, "a 200 with errors and no usable data must fail")
		assert.True(t, IsNotAuthorized(err), "want class 179 (not authorized), got %v", err)
	})

	for _, empty := range []struct {
		name string
		body string
	}{
		{"empty object data", `{"data":{},"errors":[{"code":131,"message":"Internal error"}]}`},
		{"empty array data", `{"data":[],"errors":[{"code":131,"message":"Internal error"}]}`},
		{"all-null fields", `{"data":{"user":null,"viewer":null},"errors":[{"code":131,"message":"Internal error"}]}`},
	} {
		t.Run(empty.name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = empty.body

			c, _ := newPoolWaitClient(t, 2)
			_, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())
			require.Error(t, err, "a 200 with errors and only empty data must fail")
			assert.True(t, IsInternal(err), "want class 131 (internal), got %v", err)
		})
	}
}

// F5 — journey: a write (account-pinned doPOST) getting a 5xx is NOT retried —
// the write may have landed — and the endpoint fault is not charged.
//
// Mutation: in request.go doPOST's `status != 200` branch replace the
// `return nil, apiErr` with `lastErr = apiErr; continue` => RED (3 hits).
func TestEndpointFault_WriteNotRetriedOnOverload(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.status.Store(http.StatusServiceUnavailable)
	fake.body = `upstream connect error`

	c, accs := newPoolWaitClient(t, 2)
	_, err := c.doPOST(context.Background(), accs[0], "CreateTweet",
		fake.srv.URL+"/i/api/graphql/x/CreateTweet", []byte(`{}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrOverloaded), "want overloaded, got %v", err)
	assert.EqualValues(t, 1, fake.hits.Load(), "a write is attempted exactly once — never replayed")
	_, failed, _ := accs[0].Stats()
	assert.Zero(t, failed, "the endpoint fault must not charge the writer")
	assert.True(t, accs[0].IsActive())
}

// F6 — journey: a stale queryId hits a WithSocial read and every lease answers
// 404. The call makes at most 2 X requests across 2 leases, with no backoff
// sleep: two different credentials both faulting proves the fault
// endpoint-wide, so the call stops instead of burning the whole lease budget.
//
// Mutation 1: in request.go doPoolRequest's endpoint-fault arm restore the
// same-account `continue` (drop the endpointFaulted marking) => RED (6 X hits,
// not 2). Mutation 2: in social.go withSocialAccount drop the
// socialEndpointFaultStop check => RED (6 leases, not 2).
func TestSocial_EndpointFault_StopsAfterTwoLeases(t *testing.T) {
	var xHits atomic.Int32
	r := newSocialRigX(t, func(w http.ResponseWriter, _ *http.Request) {
		xHits.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Not Found"}]}`))
	})

	start := time.Now()
	_, err := GetUserWithSocial(context.Background(), r.sc, "tesla")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEndpoint), "want endpoint fault, got %v", err)
	assert.EqualValues(t, 2, r.acquire.Load(), "the call must stop after 2 faulted leases")
	assert.EqualValues(t, 2, xHits.Load(), "each lease must make exactly one request — no same-account retry")
	assert.Less(t, elapsed, time.Second, "endpoint faults are deterministic: no backoff may be slept")
}

// F7a — journey (social path): every lease gets a 503. go-social must receive
// NO report for it — a "success" would reset the lease's ConsecutiveFails and
// keep a dead proxy healthy in the shared pool forever, and the vocabulary has
// no neutral status, so the lease is left unreported.
//
// Mutation: in social.go tryWithAccount report `success` for ErrOverloaded =>
// RED.
func TestSocial_OverloadedNeverReportsSuccess(t *testing.T) {
	r := newSocialRigX(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`upstream connect error`))
	})

	_, err := GetUserWithSocial(context.Background(), r.sc, "tesla")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrOverloaded), "want overloaded, got %v", err)
	assert.EqualValues(t, maxSocialRetries, r.acquire.Load(),
		"overload is not provably endpoint-wide: the full lease budget still rotates")

	r.reportsMu.Lock()
	defer r.reportsMu.Unlock()
	assert.Empty(t, r.reports,
		"an overloaded lease must get NO report — 'success' would keep a dead proxy leased")
}

// F7b — journey (pool path): an account whose own proxy answers 503 feeds the
// proxy-failure accounting, so the proxy benches itself and the call rotates
// to the healthy account — instead of reporting the dead proxy healthy forever.
// Repeated responses must ACCUMULATE proxyConsecFails (a 5xx through the proxy
// is not evidence the proxy works).
//
// Mutation: in request.go doPoolRequest's endpoint-fault arm drop the
// `acc.Proxy != ""` markProxyDown feed => RED (proxyConsecFails stays 0).
func TestEndpointFault_DeadProxy5xxBenchesItself(t *testing.T) {
	var hitsA, hitsB atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), "auth_token=tokA") {
			hitsA.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`upstream connect error`))
			return
		}
		hitsB.Add(1)
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
	}))
	t.Cleanup(srv.Close)

	c, accs := newPoolWaitClient(t, 2)
	accs[0].AuthToken, accs[1].AuthToken = "tokA", "tokB" // pool.Next picks accs[0] first
	accs[0].Proxy = "http://dead-residential:8080"        // field only: requests still reach the fake via the shared client
	c.cfg.ProxyBackoffInitial = time.Millisecond
	c.cfg.ProxyBackoffMax = 20 * time.Millisecond
	var alerts []proxyDownAlert
	c.alertHook = func(topic string, payload any) { alerts = append(alerts, proxyDownAlert{topic, payload}) }

	origBase := twitterBase
	twitterBase = srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })

	for i := 0; i < 2; i++ {
		tweets, err := c.GetUserTweets(context.Background(), "999", 20)
		require.NoError(t, err, "call %d must succeed on the healthy account", i)
		require.Len(t, tweets, 1)
		time.Sleep(30 * time.Millisecond) // let the tiny proxy backoff lift so accs[0] is retried next call
	}

	accs[0].mu.Lock()
	consec := accs[0].proxyConsecFails
	accs[0].mu.Unlock()
	assert.Equal(t, 2, consec, "each 5xx through the account's proxy must ACCUMULATE proxyConsecFails")
	_, failedA, _ := accs[0].Stats()
	assert.Zero(t, failedA, "a 5xx is endpoint-side: the account itself is never charged")
	assert.True(t, accs[0].IsActive(), "the proxy is benched, not the account")
	require.Len(t, alerts, 2, "each proxied 5xx must emit a proxy.down alert")
	for _, a := range alerts {
		assert.Equal(t, "proxy.down", a.topic)
	}
	assert.EqualValues(t, 2, hitsA.Load(), "the dead proxy serves one attempt per call")
	assert.EqualValues(t, 2, hitsB.Load())
}

// F8 — a 200 carrying usable `data` alongside an account-dead error code still
// takes the account-dead path: the errors+data bypass covers X-side classes
// only, so a locked/suspended verdict can never masquerade as a healthy
// success (which would skip the captcha/suspension arms and keep a dead
// account serving).
//
// Mutation: in request.go doPoolRequest widen the bypass back to all classes
// (`errClass != errNone && hasResponseData(body)`) => RED (the call succeeds).
func TestEndpointFault_ErrorsPlusData_KeepsAccountDeadClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want func(error) bool
	}{
		{"locked", 326, IsLocked},
		{"suspended", 64, IsSuspended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = `{"data":{"user":{"result":{"rest_id":"999"}}},` +
				fmt.Sprintf(`"errors":[{"code":%d,"message":"account verdict"}]}`, tc.code)

			c, accs := newPoolWaitClient(t, 1)
			_, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())
			require.Error(t, err, "a 200 with data plus code %d must NOT count as success", tc.code)
			assert.True(t, tc.want(err), "want the account-dead class for code %d, got %v", tc.code, err)
			assert.False(t, accs[0].IsActive(), "the dead account must be benched, not reported healthy")
		})
	}
}

// F9 — journey: go-social hands out a lease whose proxy string is malformed.
// A caller-requested proxy must never fail open to a direct connection: the
// per-call NewClient errors, the read fails, and ZERO requests reach X — the
// leased auth_token/ct0 cannot egress from this host's IP through the
// proxy-less shared client (clientForAccount falls back to it when
// acc.client is nil, and the *WithSocial path sets no DefaultProxy).
//
// Mutation: in client.go NewClient restore the warn-and-continue on a failed
// per-account client build => RED (the read goes out through the shared
// client and fake X counts the hit).
func TestSocial_LeaseProxyMalformed_FailsClosedNoEgress(t *testing.T) {
	var xHits atomic.Int32
	r := newSocialRigX(t, func(w http.ResponseWriter, _ *http.Request) {
		xHits.Add(1)
		_, _ = w.Write([]byte(socialUserBody))
	})
	r.leaseProxy = "not a url"

	_, err := GetUserWithSocial(context.Background(), r.sc, "tesla")
	require.Error(t, err)
	assert.EqualValues(t, 0, xHits.Load(),
		"a lease whose proxy cannot be built must never reach X — its creds would egress direct")

	r.reportsMu.Lock()
	defer r.reportsMu.Unlock()
	require.NotEmpty(t, r.reports)
	for i, s := range r.reports {
		assert.Equal(t, "auth_error", s,
			"report %d: a lease that could not even build its client reports auth_error", i)
	}
}

// F10 — an errOverloaded with no other eligible account must not sleep the
// retry backoff: the next pick is a guaranteed "pool exhausted" (the single
// account is endpointFaulted), so the ~1 s delay is pure waste.
//
// Mutation: in request.go doPoolRequest drop the Healthy gate so the backoff
// is unconditional again => RED (the call sleeps the full backoff).
func TestEndpointFault_OverloadedSkipsBackoffWhenPoolEmpty(t *testing.T) {
	fake := newPoolWaitFake(t)
	c, _ := newPoolWaitClient(t, 1)
	// Deterministic backoff near the production pace: long enough that
	// sleeping it is unmistakable, short enough to keep the suite fast.
	stealth.DefaultBackoff = stealth.BackoffConfig{
		InitialWait: 800 * time.Millisecond,
		MaxWait:     800 * time.Millisecond,
		Multiplier:  1,
	}

	// Warm the transport: the first request pays a ~1 s one-off setup that
	// would otherwise eat the measured window (same as the pool-wait tests).
	_, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())
	require.NoError(t, err)
	fake.hits.Store(0)

	fake.status.Store(http.StatusServiceUnavailable)
	fake.body = `upstream connect error`
	start := time.Now()
	_, _, err = c.doGET(context.Background(), poolWaitEndpoint, fake.url())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrOverloaded), "want overloaded, got %v", err)
	assert.EqualValues(t, 1, fake.hits.Load(), "one attempt, then the pool is provably empty")
	assert.Less(t, elapsed, 500*time.Millisecond,
		"with no eligible account the retry backoff must be skipped, not slept")
}
