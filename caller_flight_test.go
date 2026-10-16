package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// F1 — journey: a *WithSocial read whose caller ctx is cancelled mid-request.
// The fake go-social must receive NO auth_error report for the leased account
// (a caller cancel is not the account's fault) and no second lease must be
// acquired (a dead ctx cannot be retried).
//
// Mutation: in social.go tryWithAccount remove the
// `ctx.Err() != nil || errors.Is(err, context.Canceled) || ...` guard => RED
// (the ctx error falls through to report("auth_error")).
func TestWithSocial_CallerCancel_ReportsNothingAndStops(t *testing.T) {
	release := make(chan struct{})
	r := newSocialRigX(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release // hang until the test lets the abandoned socket finish
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()

	_, err := GetUserWithSocial(ctx, r.sc, "tesla")
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "want context.Canceled, got %v", err)

	r.reportsMu.Lock()
	defer r.reportsMu.Unlock()
	for _, rep := range r.reports {
		assert.NotEqual(t, "auth_error", rep, "caller cancel must not mark the lease failed")
	}
	assert.EqualValues(t, 1, r.acquire.Load(), "a dead caller ctx must not lease another account")
}

// F2 — journey: X answers a 400 carrying validation code 214 for a bad cursor.
// The call fails with ErrBadRequest after exactly ONE request — no retry, no
// rotation, no RecordFailure, no endpoint-fault count — on a 2-account pooled
// client and on *WithSocial alike.
//
// Mutation: in errors.go newAPIError classify the code-214 arm as errEndpoint
// => RED (the request rotates to the second account and errors.Is(ErrEndpoint)).
func TestBadRequest_Code214_FailsFastNoRotation(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		// Real capture shape: the code can sit nested under extensions only.
		_, _ = w.Write([]byte(`{"errors":[{"message":"Variable $cursor: invalid value",` +
			`"extensions":{"code":214,"kind":"Validation","name":"BadRequestError"}}]}`))
	}))
	t.Cleanup(srv.Close)

	c, accs := newPoolWaitClient(t, 2)
	qid := newQIDRefresher(qidConfig{faultWindow: time.Minute, faultThreshold: 3, timeout: time.Second})
	c.qid = qid

	_, _, err := c.doGET(context.Background(), poolWaitEndpoint, srv.URL+"/i/api/graphql/x/"+poolWaitEndpoint)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBadRequest), "want ErrBadRequest, got %v", err)
	assert.False(t, errors.Is(err, ErrEndpoint), "code 214 must not classify as an endpoint fault")
	assert.EqualValues(t, 1, hits.Load(), "deterministic caller error must not retry or rotate")

	for i, a := range accs {
		_, failed, _ := a.HealthTracker.Stats()
		assert.Zero(t, failed, "account %d must not be charged a failure for a caller error", i)
		assert.Zero(t, a.InFlight(), "account %d in-flight slot must be released", i)
	}
	qid.mu.Lock()
	defer qid.mu.Unlock()
	assert.Empty(t, qid.faults, "caller input rejection must not count as an endpoint fault")
}

// F2 social leg: same 400+214 through *WithSocial — one lease, a "success"
// report (the account is innocent), ErrBadRequest out.
func TestWithSocial_BadRequest_NoRetryReportsSuccess(t *testing.T) {
	var hits atomic.Int32
	r := newSocialRigX(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"code":214,"message":"Bad Request"}]}`))
	})

	_, err := GetUserWithSocial(context.Background(), r.sc, "tesla")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBadRequest), "want ErrBadRequest, got %v", err)
	assert.EqualValues(t, 1, hits.Load(), "one request only: no retry on the same lease")
	assert.EqualValues(t, 1, r.acquire.Load(), "deterministic caller error must not burn another lease")
	r.reportsMu.Lock()
	defer r.reportsMu.Unlock()
	require.Len(t, r.reports, 1)
	assert.Equal(t, "success", r.reports[0], "the lease served the request fine — the caller's input was bad")
}

// F3: a GraphQL 400 that is NOT a validation error (features mismatch, code
// 336 — same kind:"Validation"/name:"BadRequestError" markers as 214) still
// classifies as an endpoint fault and rotates as today.
//
// Mutation: in errors.go newAPIError classify EVERY GraphQL-shaped 400 as
// errBadRequest => RED (no rotation: 1 hit, and errors.Is(ErrBadRequest)).
func TestBadRequest_FeaturesMismatch_StaysEndpointFault(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		// The real features-mismatch shape (twscrape #336): identical
		// kind/name markers to a caller error — only the code differs.
		_, _ = w.Write([]byte(`{"errors":[{"code":336,"kind":"Validation","name":"BadRequestError",` +
			`"message":"The following features cannot be null: rweb_video_screen_enabled"}]}`))
	}))
	t.Cleanup(srv.Close)

	c, _ := newPoolWaitClient(t, 2)
	_, _, err := c.doGET(context.Background(), poolWaitEndpoint, srv.URL+"/i/api/graphql/x/"+poolWaitEndpoint)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEndpoint), "features mismatch must stay ErrEndpoint, got %v", err)
	assert.False(t, errors.Is(err, ErrBadRequest), "kind/name markers alone must not classify as caller error")
	assert.EqualValues(t, 2, hits.Load(), "endpoint fault rotates to the second account")
}

// F4 — journey: lease 1 answers 401 (auth-class failure), leases 2 and 3
// answer 404 (endpoint faults). The final error must still carry the
// credential verdict — errors.Is(err, ErrEndpoint) for the last fault, and
// the auth-class failure of lease 1 must be reachable with its lease name.
//
// Mutation: in social.go withSocialAccount return only lastErr => RED
// (IsBlocked fails and user_a's name vanishes from the message).
func TestWithSocial_FinalError_KeepsCredentialFailure(t *testing.T) {
	var hits atomic.Int32
	r := newSocialRigX(t, func(w http.ResponseWriter, _ *http.Request) {
		// Lease 1 retries the 401 inside its own pooled client (maxRetries),
		// so the first maxRetries hits are all its attempts; every later hit
		// belongs to leases 2+ and answers the endpoint fault.
		if hits.Add(1) <= maxRetries {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"code":161,"message":"blocked"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound) // later leases: endpoint fault
		_, _ = w.Write([]byte(`{}`))
	})

	_, err := GetUserWithSocial(context.Background(), r.sc, "tesla")
	require.Error(t, err)
	assert.True(t, IsBlocked(err), "lease 1's credential verdict must survive later endpoint faults, got %v", err)
	assert.True(t, errors.Is(err, ErrEndpoint), "the last endpoint fault must stay reachable, got %v", err)
	assert.Contains(t, err.Error(), "user_a", "the error must name the lease that failed with the credential verdict")
	assert.True(t, r.acquire.Load() >= 2, "endpoint faults rotate to the next lease")
}

// F4 variant: the budget path (no endpoint-fault early stop). Lease 1 answers
// 401 (auth class), every later lease answers 503 (overloaded). All
// maxSocialRetries leases are spent; the final error must still carry the
// credential verdict next to the last overload.
//
// Mutation: social.go withSocialAccount final `if bestErr != nil && bestErr !=
// lastErr` => `if false` => RED.
func TestWithSocial_FinalError_BudgetPathKeepsCredentialFailure(t *testing.T) {
	var hits atomic.Int32
	r := newSocialRigX(t, func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= maxRetries {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"code":161,"message":"blocked"}]}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{}`))
	})
	_, err := GetUserWithSocial(context.Background(), r.sc, "tesla")
	require.Error(t, err)
	assert.True(t, IsBlocked(err), "lease 1's credential verdict must survive later overloads, got %v", err)
	assert.True(t, errors.Is(err, ErrOverloaded), "the last overload must stay reachable, got %v", err)
	assert.EqualValues(t, maxSocialRetries, r.acquire.Load(), "overloads rotate through the whole lease budget")
}

// F5 — journey: one account, cap 2, five concurrent callers against a slow
// fake X. The fake must never see more than 2 requests in flight, and all 5
// callers eventually succeed (a full account is waited on, not failed).
//
// Mutation: in request.go doPoolRequest drop `a.tryAcquireFlight(flightLimit)`
// from poolFilter (or the acquireFlight in doPOST) => RED (in-flight > 2).
func TestFlightCap_PoolPath_NeverExceedsCap(t *testing.T) {
	var cur, maxSeen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := cur.Add(1)
		for {
			m := maxSeen.Load()
			if n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(120 * time.Millisecond)
		cur.Add(-1)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	t.Cleanup(srv.Close)

	c, _ := newPoolWaitClient(t, 1)
	c.cfg.MaxInFlightPerAccount = 2
	url := srv.URL + "/i/api/graphql/x/" + poolWaitEndpoint

	const callers = 5
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _, err := c.doGET(ctx, poolWaitEndpoint, url)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "caller %d must succeed by waiting for a slot, not failing", i)
	}
	assert.EqualValues(t, 2, maxSeen.Load(), "fake X must see exactly the cap of 2 in flight")
}

// F6 — every error path releases the slot: after failures of every class
// (401, 404, 5xx, caller ctx cancel) the in-flight count returns to 0 and the
// next request is not blocked.
//
// Mutation: in request.go doPoolRequest remove `defer release()` => RED
// (the last attempt's slot leaks; InFlight stays > 0 after a failed call).
func TestFlightCap_ErrorPathsReleaseSlots(t *testing.T) {
	var status atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status.Load() == -1 {
			<-release // hang until the test lets the abandoned socket finish
			return
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	c, accs := newPoolWaitClient(t, 1)
	c.cfg.MaxInFlightPerAccount = 2
	acc := accs[0]
	url := srv.URL + "/i/api/graphql/x/" + poolWaitEndpoint
	bg := context.Background()

	for _, tc := range []struct {
		name   string
		status int32
	}{
		{"401", 401},
		{"404", 404},
		{"500", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status.Store(tc.status)
			_, _, err := c.doGET(bg, poolWaitEndpoint, url)
			require.Error(t, err)
			assert.Zero(t, acc.InFlight(), "slot leaked after HTTP %d", tc.status)
		})
	}

	t.Run("ctx cancel", func(t *testing.T) {
		status.Store(-1)
		ctx, cancel := context.WithCancel(bg)
		done := make(chan error, 1)
		go func() {
			_, _, err := c.doGET(ctx, poolWaitEndpoint, url)
			done <- err
		}()
		require.Eventually(t, func() bool { return acc.InFlight() == 1 }, 2*time.Second, 5*time.Millisecond,
			"the hanging request must hold exactly one slot")
		cancel()
		require.Error(t, <-done)
		require.Eventually(t, func() bool { return acc.InFlight() == 0 }, 2*time.Second, 5*time.Millisecond,
			"caller cancel must release the slot")
	})

	t.Run("next request not blocked", func(t *testing.T) {
		status.Store(200)
		ctx, cancel := context.WithTimeout(bg, 5*time.Second)
		defer cancel()
		body, _, err := c.doGET(ctx, poolWaitEndpoint, url)
		require.NoError(t, err, "a healthy request must get a slot after the failure run")
		assert.NotEmpty(t, body)
	})
}

// Pinned-path leg of F5/F6: doPOST on the same account also honours the cap
// and releases on every path.
func TestFlightCap_PinnedDoPOST_WaitsAndReleases(t *testing.T) {
	var cur, maxSeen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := cur.Add(1)
		for {
			m := maxSeen.Load()
			if n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		cur.Add(-1)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	t.Cleanup(srv.Close)

	c, accs := newPoolWaitClient(t, 1)
	c.cfg.MaxInFlightPerAccount = 2
	acc := accs[0]
	url := srv.URL + "/i/api/graphql/x/CreateTweet"

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range 3 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := c.doPOST(ctx, acc, "CreateTweet", url, []byte(`{}`))
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "pinned caller %d must wait for a slot and succeed", i)
	}
	assert.LessOrEqual(t, maxSeen.Load(), int32(2), "pinned path must never exceed the cap")
	assert.Zero(t, acc.InFlight(), "all pinned slots released")
}

// The cap disables cleanly: a negative MaxInFlightPerAccount never limits.
func TestFlightCap_NegativeDisables(t *testing.T) {
	var cur, maxSeen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := cur.Add(1)
		for {
			m := maxSeen.Load()
			if n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		cur.Add(-1)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	t.Cleanup(srv.Close)

	c, _ := newPoolWaitClient(t, 1)
	c.cfg.MaxInFlightPerAccount = -1
	url := srv.URL + "/i/api/graphql/x/" + poolWaitEndpoint

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := c.doGET(context.Background(), poolWaitEndpoint, url)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 4, maxSeen.Load(), "negative cap must disable the limit entirely")
}

// Config default: zero means "use the default of 2" (defaults() fills it).
func TestClientConfig_MaxInFlightPerAccountDefault(t *testing.T) {
	cfg := ClientConfig{}
	cfg.defaults()
	assert.Equal(t, defaultMaxInFlightPerAccount, cfg.MaxInFlightPerAccount)

	cfg = ClientConfig{MaxInFlightPerAccount: -5}
	cfg.defaults()
	assert.Equal(t, -5, cfg.MaxInFlightPerAccount, "negative stays negative — cap disabled")
}

// slowFirstServer answers request #1 slowly with 200 (it occupies one account's
// slot); later requests are answered by next(n, w). started closes when
// request #1 has arrived.
func slowFirstServer(t *testing.T, hold time.Duration, next func(n int32, w http.ResponseWriter)) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	var hits atomic.Int32
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			close(started)
			time.Sleep(hold)
			_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
			return
		}
		next(n, w)
	}))
	t.Cleanup(srv.Close)
	return srv, started
}

// F7 — an endpoint-fault retry (rotateNow) against cap-saturated accounts must
// WAIT for a slot, not fail with "pool exhausted". Two accounts, cap 1: call B
// holds account 0 slowly; call A lands on account 1, gets a 404 (endpoint fault,
// account 1 excluded), and its retry finds only account 0, at cap.
//
// Mutation: request.go doPoolRequest, in the else-branch cap wait, replace
// `accErr != nil && ctx.Err() == nil && c.anyCapWaitable(capWaitable)` with
// `false` (the old fail-fast pool.Next) => RED.
func TestFlightCap_EndpointFaultRetryWaitsForSlot(t *testing.T) {
	srv, started := slowFirstServer(t, 300*time.Millisecond, func(n int32, w http.ResponseWriter) {
		if n == 2 {
			w.WriteHeader(http.StatusNotFound) // stale-route endpoint fault
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})
	c, accs := newPoolWaitClient(t, 2)
	c.cfg.MaxInFlightPerAccount = 1
	url := srv.URL + "/i/api/graphql/x/" + poolWaitEndpoint
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bDone := make(chan error, 1)
	go func() {
		_, _, err := c.doGET(ctx, poolWaitEndpoint, url)
		bDone <- err
	}()
	<-started
	body, _, err := c.doGET(ctx, poolWaitEndpoint, url)
	require.NoError(t, err, "the retry must wait for the capped account, not fail with pool exhausted")
	assert.NotEmpty(t, body)
	require.NoError(t, <-bDone)
	for i, a := range accs {
		assert.Zero(t, a.InFlight(), "account %d slot leaked", i)
	}
}

// F8 — a guest-eligible (non-requiresAuth) endpoint with every account at cap
// waits for a slot and is served by an account. DisableGuestFallback is set, so
// falling through to the guest path would surface as an error.
//
// Mutation: same line as F7 (`... c.anyCapWaitable(capWaitable)` => `false`)
// => RED ("no authenticated account and guest fallback disabled").
func TestFlightCap_NonAuthEndpointWaitsInsteadOfGuest(t *testing.T) {
	srv, started := slowFirstServer(t, 300*time.Millisecond, func(_ int32, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})
	c, accs := newPoolWaitClient(t, 1)
	c.cfg.MaxInFlightPerAccount = 1
	const nonAuth = "PublicLookup"
	require.False(t, requiresAuth(nonAuth))
	url := srv.URL + "/i/api/graphql/x/" + nonAuth
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bDone := make(chan error, 1)
	go func() {
		_, _, err := c.doGET(ctx, nonAuth, url)
		bDone <- err
	}()
	<-started
	body, _, err := c.doGET(ctx, nonAuth, url)
	require.NoError(t, err, "a capped pool must be waited on, not downgraded to the guest path")
	assert.NotEmpty(t, body)
	require.NoError(t, <-bDone)
	assert.Zero(t, accs[0].InFlight())
}
