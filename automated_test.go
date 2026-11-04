package twitter

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/anatolykoptev/go-twitter/internal/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	automated226Only = `{"errors":[{"code":226,"message":"This request looks like it might be automated."}]}`
	automated226Data = `{"data":{"user":{"result":{"timeline":{}}}},"errors":[{"code":226,"message":"automated"}]}`
	notAuth37Only    = `{"errors":[{"code":37,"message":"Not authorized."}]}`
)

// assertBenched requires acc to be soft-deactivated for at least min and at
// most min plus slack: the cooldown the verdict chose, not merely "inactive".
func assertBenched(t *testing.T, acc *Account, min, slack time.Duration) {
	t.Helper()
	assert.False(t, acc.IsActive(), "account must be benched")
	until := time.Until(acc.ReactivateAt())
	assert.GreaterOrEqual(t, until, min-5*time.Second, "cooldown below the floor")
	assert.LessOrEqual(t, until, min+slack+5*time.Second, "cooldown above the ceiling")
}

func TestClassifyError_AutomatedAndNotAuthorized37(t *testing.T) {
	assert.Equal(t, errAutomated, classifyError([]byte(automated226Only), nil))
	assert.Equal(t, errNotAuthorized, classifyError([]byte(notAuth37Only), nil))
	assert.Equal(t, errAutomated, newAPIError(http.StatusForbidden, []byte(automated226Only), nil).Class, "226 on a 403 keeps its class")
	assert.True(t, IsAutomated(&APIError{Class: errAutomated}))
	assert.False(t, IsAutomated(&APIError{Class: errLocked}))
	assert.False(t, IsAutomated(nil))
}

func TestOutcomeForClass_Automated(t *testing.T) {
	assert.Equal(t, telemetry.OutcomeAutomated, outcomeForClass(errAutomated))
	assert.Equal(t, telemetry.OutcomeAutomated, attemptOutcome(200, []byte(automated226Only), nil, nil, false))
}

// 226 in an errors-only 200 and in a 200 that also carries data: both benchmark
// the serving account (BanCooldown floor, jitter ceiling +25%) and neither is a
// success. The data-bearing body must not take the partial-success bypass.
//
// Mutation: errors.go classifyError — delete the `case automatedCode:` arm
// (226 falls through to errNone) => RED on both subtests (errors-only reads as
// ErrUnexpectedResponse, data+226 reads as a healthy success).
func TestDoGET_226_BenchesAccount(t *testing.T) {
	for name, body := range map[string]string{"errors_only": automated226Only, "with_data": automated226Data} {
		t.Run(name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = body
			c, accs := newPoolWaitClient(t, 1)
			origBase := twitterBase
			twitterBase = fake.srv.URL + "/i/api/graphql"
			t.Cleanup(func() { twitterBase = origBase })

			_, err := c.GetUserTweets(context.Background(), "999", 20)
			require.Error(t, err)
			assert.True(t, IsAutomated(err), "got %v", err)
			assertBenched(t, accs[0], time.Hour, time.Hour/4)
		})
	}
}

// 226 usually arrives as HTTP 403: the 401/403 arm must bench too.
//
// Mutation: request.go doPoolRequest 401/403 switch — delete the
// `case errAutomated:` arm => RED (falls to default: RecordFailure only, the
// account stays active).
func TestDoGET_226_On403_BenchesAccount(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.body = automated226Only
	fake.status.Store(http.StatusForbidden)
	c, accs := newPoolWaitClient(t, 1)
	origBase := twitterBase
	twitterBase = fake.srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })

	_, err := c.GetUserTweets(context.Background(), "999", 20)
	require.Error(t, err)
	assert.True(t, IsAutomated(err), "got %v", err)
	assertBenched(t, accs[0], time.Hour, time.Hour/4)
}

// The write path: a pinned POST is never replayed, and the verdict benches the
// account instead of merely charging a failure.
//
// Mutation: request.go doPOST 200 switch — delete the `case errAutomated:`
// arm => RED (default arm: RecordFailure only, account still active).
func TestDoPOST_226_BenchesAccount(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"200_errors_only": {http.StatusOK, automated226Only},
		"200_with_data":   {http.StatusOK, automated226Data},
		"403":             {http.StatusForbidden, automated226Only},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = tc.body
			fake.status.Store(int32(tc.status))
			c, accs := newPoolWaitClient(t, 2)
			body, err := c.doPOST(context.Background(), accs[0], "CreateTweet",
				fake.srv.URL+"/i/api/graphql/x/CreateTweet", []byte(`{}`))
			require.Error(t, err)
			assert.Nil(t, body, "226 next to data is still the verdict")
			assert.True(t, IsAutomated(err), "got %v", err)
			assert.EqualValues(t, 1, fake.hits.Load(), "a write is never replayed")
			assertBenched(t, accs[0], time.Hour, time.Hour/4)
			assert.True(t, accs[1].IsActive(), "the verdict is account-scoped")
		})
	}
}

// The 226 cooldown must never undercut the 326 (lock) path's.
//
// Mutation: automated.go automatedCooldown — `return base - time.Duration(...)`
// (or base/2) => RED.
func TestAutomatedCooldown_NeverShorterThanBase(t *testing.T) {
	for range 200 {
		d := automatedCooldown(6 * time.Hour)
		require.GreaterOrEqual(t, d, 6*time.Hour)
		require.LessOrEqual(t, d, 6*time.Hour+6*time.Hour/4)
	}
	assert.Zero(t, automatedCooldown(0))
}

// 37 is an access refusal: the same account-scoped AuthCooldown bench as 179/219.
//
// Mutation: errors.go classifyError — change `case 37, 179, 219:` to
// `case 179, 219:` => RED (errors-only 200 reads as ErrUnexpectedResponse, the
// account stays active).
func TestDoGET_37_BenchesWithAuthCooldown(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.body = notAuth37Only
	c, accs := newPoolWaitClient(t, 1)
	c.cfg.AuthCooldown = 2 * time.Hour // distinct from BanCooldown (1h): proves WHICH cooldown applied
	origBase := twitterBase
	twitterBase = fake.srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })

	_, err := c.GetUserTweets(context.Background(), "999", 20)
	require.Error(t, err)
	assert.True(t, IsNotAuthorized(err), "got %v", err)
	assertBenched(t, accs[0], 2*time.Hour, 0)
}

const settingsOK = `{"screen_name":"viewer_one","protected":false}`

func TestCheckSession(t *testing.T) {
	origAPI := twitterAPIURL
	t.Cleanup(func() { twitterAPIURL = origAPI })

	cases := []struct {
		name   string
		status int
		body   string
		check  func(t *testing.T, info *SessionInfo, err error)
	}{
		{"alive", 200, settingsOK, func(t *testing.T, info *SessionInfo, err error) {
			require.NoError(t, err)
			assert.Equal(t, "viewer_one", info.ScreenName)
		}},
		{"locked 326", 200, `{"errors":[{"code":326}]}`, func(t *testing.T, _ *SessionInfo, err error) { assert.True(t, IsLocked(err), "%v", err) }},
		{"expired 32", 403, `{"errors":[{"code":32}]}`, func(t *testing.T, _ *SessionInfo, err error) { assert.True(t, IsAuthExpired(err), "%v", err) }},
		{"suspended 64", 403, `{"errors":[{"code":64}]}`, func(t *testing.T, _ *SessionInfo, err error) { assert.True(t, IsSuspended(err), "%v", err) }},
		{"automated 226", 403, automated226Only, func(t *testing.T, _ *SessionInfo, err error) { assert.True(t, IsAutomated(err), "%v", err) }},
		{"bare 401", 401, ``, func(t *testing.T, _ *SessionInfo, err error) { assert.True(t, IsAuthExpired(err), "%v", err) }},
		{"429", 429, ``, func(t *testing.T, _ *SessionInfo, err error) { assert.True(t, IsRateLimited(err), "%v", err) }},
		{"200 without a handle", 200, `{}`, func(t *testing.T, _ *SessionInfo, err error) {
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUnexpectedResponse)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = tc.body
			fake.status.Store(int32(tc.status))
			twitterAPIURL = fake.srv.URL
			c, accs := newPoolWaitClient(t, 1)
			info, err := c.CheckSession(context.Background(), accs[0])
			tc.check(t, info, err)
			assert.EqualValues(t, 1, fake.hits.Load())
			assert.True(t, accs[0].IsActive() || tc.status == 429, "CheckSession reports; it never benches")
		})
	}
}

// CheckSession spends from the account's own per-endpoint window: a held
// endpoint is refused locally, with no request sent.
//
// Mutation: session_check.go — delete the `if !acc.AllowRequest(epCheckSession)`
// block => RED (a request reaches the fake).
func TestCheckSession_RespectsEndpointWindow(t *testing.T) {
	origAPI := twitterAPIURL
	t.Cleanup(func() { twitterAPIURL = origAPI })
	fake := newPoolWaitFake(t)
	fake.body = settingsOK
	twitterAPIURL = fake.srv.URL
	c, accs := newPoolWaitClient(t, 1)
	accs[0].MarkEndpointRateLimited(epCheckSession, time.Now().Add(time.Hour))

	_, err := c.CheckSession(context.Background(), accs[0])
	require.Error(t, err)
	assert.True(t, IsRateLimited(err))
	assert.Zero(t, fake.hits.Load(), "a blocked window sends nothing")
}

// The media upload arm benches on 226 too, in a 2xx body and a 403.
//
// Mutation: media.go doMediaRequest — delete the `if cls == errAutomated`
// block (2xx) or the `case errAutomated:` arm (403) => RED for that subtest.
func TestDoMediaRequest_226_BenchesAccount(t *testing.T) {
	for name, status := range map[string]int{"200": http.StatusOK, "403": http.StatusForbidden} {
		t.Run(name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = automated226Only
			fake.status.Store(int32(status))
			c, accs := newPoolWaitClient(t, 1)
			_, err := c.doMediaRequest(context.Background(), accs[0], "POST",
				fake.srv.URL+"/1.1/media/upload.json?command=INIT", nil, "application/x-www-form-urlencoded")
			require.Error(t, err)
			assert.True(t, IsAutomated(err), "got %v", err)
			assertBenched(t, accs[0], time.Hour, time.Hour/4)
		})
	}
}
