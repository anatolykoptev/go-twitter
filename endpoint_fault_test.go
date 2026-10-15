package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	assert.EqualValues(t, calls*maxRetries, fake.hits.Load(),
		"every attempt reaches X: rotation keeps the full per-call budget")
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
	assert.EqualValues(t, maxSocialRetries, r.acquire.Load(),
		"endpoint faults still rotate across leases inside the retry budget")

	r.reportsMu.Lock()
	defer r.reportsMu.Unlock()
	require.NotEmpty(t, r.reports, "every lease must be reported")
	for i, s := range r.reports {
		assert.Equal(t, "success", s, "report %d: an endpoint fault must not be reported as %q", i, s)
	}
}

// F4 — a 200 carrying `errors` AND usable `data` counts as success (the rule
// generalizes the historical code-131 arm to every error code); a 200 with
// errors and NO usable data still fails with its class.
//
// Mutation: in request.go doPoolRequest drop `&& hasResponseData(body)` from
// the errors+data arm (accept any 200-with-errors) => RED on the no-data side.
func TestEndpointFault_ErrorsPlusDataSucceeds_ErrorsOnlyFails(t *testing.T) {
	t.Run("errors plus data succeeds", func(t *testing.T) {
		fake := newPoolWaitFake(t)
		// 179 (not authorized for a field) alongside usable timeline data.
		fake.body = strings.TrimSuffix(userTweetsBody(snowflakeAt(time.Now())), "}") +
			`,"errors":[{"code":179,"message":"Not authorized for field"}]}`

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
