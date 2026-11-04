package twitter

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

// Distinct cooldowns make every bench assertion name WHICH cooldown applied:
// the first-226 window [2h, 2.5h] and the repeat window [5h, 6.25h] cannot
// overlap, and neither equals the 1h default a fixture might coast on.
const (
	testAuthCooldown = 2 * time.Hour
	testBanCooldown  = 5 * time.Hour
)

func newAutomatedClient(t *testing.T, n int) (*Client, []*Account) {
	t.Helper()
	c, accs := newPoolWaitClient(t, n)
	c.cfg.AuthCooldown = testAuthCooldown
	c.cfg.BanCooldown = testBanCooldown
	return c, accs
}

func pointTwitterBase(t *testing.T, url string) {
	t.Helper()
	orig := twitterBase
	twitterBase = url + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = orig })
}

// The FIRST 226 on an account benches it for AuthCooldown (+ up to 25%
// jitter); a SECOND within 24h of that bench ending escalates to BanCooldown.
// Errors-only and data-bearing bodies behave alike and neither is a success.
//
// Mutation: errors.go classifyError — delete the `case automatedCode:` arm
// (226 falls through to errNone) => RED on every subtest. Mutation:
// automated.go automatedBenchFor — `base := c.cfg.BanCooldown` for the first
// 226 => RED on the first-window assertion; drop the `if repeat` => RED on the
// repeat window.
func TestDoGET_226_GraduatedBench(t *testing.T) {
	for name, body := range map[string]string{"errors_only": automated226Only, "with_data": automated226Data} {
		t.Run(name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = body
			c, accs := newAutomatedClient(t, 1)
			pointTwitterBase(t, fake.srv.URL)

			_, err := c.GetUserTweets(context.Background(), "999", 20)
			require.Error(t, err)
			assert.True(t, IsAutomated(err), "got %v", err)
			assertBenched(t, accs[0], testAuthCooldown, testAuthCooldown/4)

			accs[0].SetActive(true) // the bench ended; the account serves again
			accs[0].mu.Lock()
			accs[0].automatedUntil = time.Now().Add(-time.Minute)
			accs[0].mu.Unlock()
			_, err = c.GetUserTweets(context.Background(), "999", 20)
			require.Error(t, err)
			assert.True(t, IsAutomated(err), "got %v", err)
			assertBenched(t, accs[0], testBanCooldown, testBanCooldown/4)
		})
	}
}

// 226 usually arrives as HTTP 403: the 401/403 arm must bench too, and record
// the attempt with the legacy MetricsHook exactly once.
//
// Mutation: request.go doPoolRequest 401/403 switch — delete the
// `case errAutomated:` arm => RED (default arm: RecordFailure only, account
// stays active). Mutation: automated.go benchAutomated — re-add
// `c.recordAPICall(endpoint, false, false)` => RED (two hook calls).
func TestDoGET_226_On403_BenchesOnce(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.body = automated226Only
	fake.status.Store(http.StatusForbidden)
	c, accs := newAutomatedClient(t, 1)
	var calls atomic.Int32
	c.cfg.MetricsHook = func(string, bool, bool) { calls.Add(1) }
	pointTwitterBase(t, fake.srv.URL)

	_, err := c.GetUserTweets(context.Background(), "999", 20)
	require.Error(t, err)
	assert.True(t, IsAutomated(err), "got %v", err)
	assertBenched(t, accs[0], testAuthCooldown, testAuthCooldown/4)
	assert.EqualValues(t, 1, calls.Load(), "one MetricsHook record per attempt")
}

// The 200 arm records the attempt once as well.
//
// Mutation: request.go doPoolRequest 200 `case errAutomated:` — delete its
// `c.recordAPICall(...)` => RED (zero hook calls); delete the whole arm =>
// RED (default arm benches for AuthCooldown with a different error message
// path, and the repeat-escalation in TestDoGET_226_GraduatedBench).
func TestDoGET_226_On200_BenchesOnce(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.body = automated226Only
	c, accs := newAutomatedClient(t, 1)
	var calls atomic.Int32
	c.cfg.MetricsHook = func(string, bool, bool) { calls.Add(1) }
	pointTwitterBase(t, fake.srv.URL)

	_, err := c.GetUserTweets(context.Background(), "999", 20)
	require.Error(t, err)
	assert.True(t, IsAutomated(err), "got %v", err)
	assertBenched(t, accs[0], testAuthCooldown, testAuthCooldown/4)
	assert.EqualValues(t, 1, calls.Load(), "one MetricsHook record per attempt")
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
			c, accs := newAutomatedClient(t, 2)
			var calls atomic.Int32
			c.cfg.MetricsHook = func(string, bool, bool) { calls.Add(1) }
			body, err := c.doPOST(context.Background(), accs[0], "CreateTweet",
				fake.srv.URL+"/i/api/graphql/x/CreateTweet", []byte(`{}`))
			require.Error(t, err)
			assert.Nil(t, body, "226 next to data is still the verdict")
			assert.True(t, IsAutomated(err), "got %v", err)
			assert.EqualValues(t, 1, fake.hits.Load(), "a write is never replayed")
			assert.EqualValues(t, 1, calls.Load(), "one MetricsHook record per attempt")
			assertBenched(t, accs[0], testAuthCooldown, testAuthCooldown/4)
			assert.True(t, accs[1].IsActive(), "the verdict is account-scoped")
		})
	}
}

// The retry that follows a relogin meets 226: the account just logged in is
// benched with the same graduated cooldown, not charged a plain auth failure.
//
// Mutation: request.go postReloginFailure — delete the
// `case cls == errAutomated:` arm => RED (falls to fallbackCooldown 1h: the
// bench window assertion fails).
func TestPostReloginFailure_226_Benches(t *testing.T) {
	c, accs := newAutomatedClient(t, 1)
	ae := c.postReloginFailure(accs[0], "UserTweets", http.StatusOK, []byte(automated226Only), nil, nil, errAuthExpired, time.Hour, "post-relogin request failed")
	assert.Equal(t, errAutomated, ae.Class)
	assertBenched(t, accs[0], testAuthCooldown, testAuthCooldown/4)
}

// 226 is account-scoped for benching, but NOT for the 1 s retry-backoff skip:
// skipping it would let a shared cause burn several accounts within one call.
//
// Mutation: request.go accountScopedFailure — add errAutomated to the case
// list => RED.
func TestAccountScopedFailure_AutomatedKeepsBackoff(t *testing.T) {
	assert.False(t, accountScopedFailure(&APIError{Class: errAutomated}))
	assert.True(t, accountScopedFailure(&APIError{Class: errLocked}), "control: a lock is account-scoped")
}

// Jitter only lengthens the base.
//
// Mutation: automated.go automatedCooldown — `base - ...` => RED.
func TestAutomatedCooldown_JitterOnlyLengthens(t *testing.T) {
	for range 200 {
		d := automatedCooldown(6 * time.Hour)
		require.GreaterOrEqual(t, d, 6*time.Hour)
		require.LessOrEqual(t, d, 6*time.Hour+6*time.Hour/4)
	}
	assert.Zero(t, automatedCooldown(0))
}

// Shared-cause detection: 2 DISTINCT accounts within the window fire it once
// (log, counter, pool alert); the same account twice does not; a third distinct
// account in the same crossing does not fire again.
//
// Mutation: automated.go noteAutomatedHit — key the map by endpoint instead of
// acc.ID() => RED (same-account case fires, or two accounts collapse).
func TestAutomatedSharedSuspect(t *testing.T) {
	newC := func(n int) (*Client, []*Account, *[]string) {
		c, accs := newAutomatedClient(t, n)
		var topics []string
		var mu sync.Mutex
		c.alertHook = func(topic string, _ any) {
			mu.Lock()
			topics = append(topics, topic)
			mu.Unlock()
		}
		return c, accs, &topics
	}
	body := []byte(automated226Only)

	t.Run("two_distinct_fire_once", func(t *testing.T) {
		c, accs, topics := newC(3)
		c.benchAutomated(accs[0], "UserTweets", 200, body, nil)
		assert.Empty(t, *topics, "one account is not a shared signal")
		c.benchAutomated(accs[1], "UserTweets", 200, body, nil)
		assert.Equal(t, []string{"automated.shared_suspect"}, *topics)
		c.benchAutomated(accs[2], "SearchTimeline", 200, body, nil)
		assert.Len(t, *topics, 1, "one signal per window crossing")
	})
	t.Run("same_account_twice_does_not", func(t *testing.T) {
		c, accs, topics := newC(2)
		c.benchAutomated(accs[0], "UserTweets", 200, body, nil)
		c.benchAutomated(accs[0], "UserTweets", 200, body, nil)
		assert.Empty(t, *topics)
	})
	t.Run("window_expiry_rearms", func(t *testing.T) {
		c, accs, topics := newC(2)
		c.benchAutomated(accs[0], "UserTweets", 200, body, nil)
		c.benchAutomated(accs[1], "UserTweets", 200, body, nil)
		c.automated.mu.Lock()
		for id, h := range c.automated.hits {
			h.at = h.at.Add(-2 * sharedSuspectWindow)
			c.automated.hits[id] = h
		}
		c.automated.mu.Unlock()
		c.benchAutomated(accs[0], "UserTweets", 200, body, nil)
		assert.Len(t, *topics, 1, "an old window does not count")
		c.benchAutomated(accs[1], "UserTweets", 200, body, nil)
		assert.Len(t, *topics, 2, "a fresh crossing fires again")
	})
}

// Code 37 next to usable data is NOT a verdict (it may refer to the target):
// the data is returned and nobody is benched. An errors-only 37 still benches
// with AuthCooldown (the account's own cooldown, distinct from BanCooldown).
//
// Mutation: errors.go partialSuccess — delete the 37 branch (return false for
// errNotAuthorized) => RED on with_data (the data is discarded, the account
// benched). Mutation: errors.go classifyError — `case 179, 219:` => RED on
// errors_only.
func TestDoGET_37(t *testing.T) {
	good := userTweetsBody(snowflakeAt(time.Now()))
	withData := `{"errors":[{"code":37,"message":"m"}],` + strings.TrimPrefix(good, "{")

	t.Run("with_data_succeeds_no_bench", func(t *testing.T) {
		fake := newPoolWaitFake(t)
		fake.body = withData
		c, accs := newAutomatedClient(t, 1)
		pointTwitterBase(t, fake.srv.URL)
		_, err := c.GetUserTweets(context.Background(), "999", 20)
		require.NoError(t, err)
		assert.True(t, accs[0].IsActive())
	})
	t.Run("errors_only_benches_with_auth_cooldown", func(t *testing.T) {
		fake := newPoolWaitFake(t)
		fake.body = notAuth37Only
		c, accs := newAutomatedClient(t, 1)
		pointTwitterBase(t, fake.srv.URL)
		_, err := c.GetUserTweets(context.Background(), "999", 20)
		require.Error(t, err)
		assert.True(t, IsNotAuthorized(err), "got %v", err)
		assertBenched(t, accs[0], testAuthCooldown, 0)
	})
	t.Run("other_account_code_beside_37_still_verdict", func(t *testing.T) {
		assert.False(t, partialSuccess(errNotAuthorized, []byte(`{"errors":[{"code":37},{"code":219}],"data":{"x":1}}`)))
		assert.True(t, partialSuccess(errNotAuthorized, []byte(`{"errors":[{"code":37}],"data":{"x":1}}`)))
		assert.False(t, partialSuccess(errAutomated, []byte(automated226Data)), "226 with data stays a verdict")
	})
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
			c, accs := newAutomatedClient(t, 1)
			var calls atomic.Int32
			c.cfg.MetricsHook = func(string, bool, bool) { calls.Add(1) }
			_, err := c.doMediaRequest(context.Background(), accs[0], "POST",
				fake.srv.URL+"/1.1/media/upload.json?command=INIT", nil, "application/x-www-form-urlencoded")
			require.Error(t, err)
			assert.True(t, IsAutomated(err), "got %v", err)
			assert.EqualValues(t, 1, calls.Load(), "one MetricsHook record per attempt")
			assertBenched(t, accs[0], testAuthCooldown, testAuthCooldown/4)
		})
	}
}

// A repeat 226 is never shorter than the first, whatever the config: with a
// short BanCooldown (AuthCooldown 30m / BanCooldown 1m) the repeat base is
// 2*AuthCooldown.
//
// Mutation: automated.go automatedRepeatBase — `return c.cfg.BanCooldown` =>
// RED (repeat ~1m < first 30m).
func TestAutomatedBenchFor_RepeatNeverShorterThanFirst(t *testing.T) {
	c, accs := newAutomatedClient(t, 1)
	c.cfg.AuthCooldown = 30 * time.Minute
	c.cfg.BanCooldown = time.Minute
	acc := accs[0]

	first := c.automatedBenchFor(acc)
	assert.GreaterOrEqual(t, first, 30*time.Minute)
	assert.LessOrEqual(t, first, 30*time.Minute+30*time.Minute/4)

	acc.mu.Lock()
	acc.automatedUntil = time.Now().Add(-time.Minute) // first bench ended
	acc.mu.Unlock()
	repeat := c.automatedBenchFor(acc)
	assert.GreaterOrEqual(t, repeat, first, "repeat must not undercut the first bench")
	assert.GreaterOrEqual(t, repeat, time.Hour)

	// A larger BanCooldown still wins.
	c.cfg.BanCooldown = 5 * time.Hour
	acc.mu.Lock()
	acc.automatedUntil = time.Now().Add(-time.Minute)
	acc.mu.Unlock()
	assert.GreaterOrEqual(t, c.automatedBenchFor(acc), 5*time.Hour)
}

// Concurrent 226s on one account inside its own bench are one offence: the
// first sets the first-offence bench, the rest keep it and never escalate.
//
// Mutation: automated.go automatedBenchFor — delete the
// `if now.Before(acc.automatedUntil) { return ... }` guard => RED (the second
// hit lands in the repeat window and the bench jumps to BanCooldown).
func TestAutomatedBenchFor_ConcurrentHitsDoNotEscalate(t *testing.T) {
	c, accs := newAutomatedClient(t, 1)
	acc := accs[0]
	var wg sync.WaitGroup
	got := make([]time.Duration, 8)
	for i := range got {
		wg.Add(1)
		go func() { defer wg.Done(); got[i] = c.automatedBenchFor(acc) }()
	}
	wg.Wait()
	for _, d := range got {
		assert.GreaterOrEqual(t, d, time.Duration(0))
		assert.LessOrEqual(t, d, testAuthCooldown+testAuthCooldown/4, "must stay at first-offence length")
	}
	acc.mu.Lock()
	until := time.Until(acc.automatedUntil)
	acc.mu.Unlock()
	assert.LessOrEqual(t, until, testAuthCooldown+testAuthCooldown/4)
}

// Account verdicts outrank 37/179/219 in a multi-code body, in either order;
// 37 alone stays not-authorized.
//
// Mutation: errors.go classifyError — remove the accountVerdictClass call in
// the accessRefusedCode arm => RED on [37,226]/[37,326]/[37,64].
func TestClassifyError_VerdictBeatsNotAuthorized(t *testing.T) {
	cases := map[string]errorClass{
		`{"errors":[{"code":37},{"code":226}]}`:  errAutomated,
		`{"errors":[{"code":226},{"code":37}]}`:  errAutomated,
		`{"errors":[{"code":179},{"code":326}]}`: errLocked,
		`{"errors":[{"code":219},{"code":64}]}`:  errSuspended,
		`{"errors":[{"code":37},{"code":37}]}`:   errNotAuthorized,
		`{"errors":[{"code":37},{"code":179}]}`:  errNotAuthorized,
	}
	for body, want := range cases {
		assert.Equal(t, want, classifyError([]byte(body), nil), body)
	}
	// 88 outranks only when it is a ban (remaining > 0), not a plain rate limit.
	b88 := []byte(`{"errors":[{"code":37},{"code":88}]}`)
	assert.Equal(t, errBanned, classifyError(b88, map[string]string{xRateLimitRemaining: "5"}))
	assert.Equal(t, errNotAuthorized, classifyError(b88, map[string]string{xRateLimitRemaining: "0"}))
}

// [37,226] benches the account whether or not data rides along; [37] with
// data stays a partial success.
//
// Mutation: errors.go classifyError — same as above => RED on both
// [37,226] subtests (not-authorized, no bench).
func TestDoGET_37And226_Automated(t *testing.T) {
	for name, body := range map[string]string{
		"errors_only": `{"errors":[{"code":37},{"code":226}]}`,
		"with_data":   `{"data":{"user":{"result":{"timeline":{}}}},"errors":[{"code":37},{"code":226}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = body
			c, accs := newAutomatedClient(t, 1)
			pointTwitterBase(t, fake.srv.URL)
			_, err := c.GetUserTweets(context.Background(), "999", 20)
			require.Error(t, err)
			assert.True(t, IsAutomated(err), "got %v", err)
			assertBenched(t, accs[0], testAuthCooldown, testAuthCooldown/4)
		})
	}
	data37 := []byte(`{"data":{"user":{}},"errors":[{"code":37}]}`)
	assert.True(t, partialSuccess(classifyError(data37, nil), data37), "[37]+data stays a partial success")
	mixed := []byte(`{"data":{"user":{}},"errors":[{"code":37},{"code":226}]}`)
	assert.False(t, partialSuccess(classifyError(mixed, nil), mixed))
}
