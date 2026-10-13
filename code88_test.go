package twitter

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rateLimit88(remaining string, resetIn time.Duration) (map[string]string, string) {
	return map[string]string{
		xRateLimitRemaining: remaining,
		xRateLimitReset:     strconv.FormatInt(time.Now().Add(resetIn).Unix(), 10),
	}, `{"errors":[{"code":88,"message":"Rate limit exceeded"}]}`
}

// F4 (journey: X answers code 88 inside a 200 body with an exhausted window):
// the account stays active, only that endpoint is blocked until the reset, and
// the error is a rate limit.
//
// Mutation: in errors.go classifyError restore `case 88: return errBanned`
// unconditionally => RED (account soft-deactivated, error IsBanned).
func TestCode88_ExhaustedWindow_BlocksEndpointOnly(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.hdrs, fake.body = rateLimit88("0", time.Hour)
	c, accs := newPoolWaitClient(t, 1)
	acc := accs[0]

	_, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())

	require.Error(t, err)
	assert.True(t, IsRateLimited(err), "want IsRateLimited, got %v", err)
	assert.False(t, IsBanned(err), "an exhausted window is not a ban: %v", err)
	assert.True(t, acc.IsActive(), "code 88 with remaining=0 must not deactivate the account")
	assert.True(t, acc.IsEndpointRateLimited(poolWaitEndpoint))
	assert.WithinDuration(t, time.Now().Add(time.Hour), acc.EndpointAvailableAt(poolWaitEndpoint), 5*time.Second,
		"endpoint must be blocked until x-rate-limit-reset")
	assert.False(t, acc.IsEndpointRateLimited("Followers"), "other endpoints stay usable")
}

// F5: code 88 while the window still has requests remaining is not a rate
// limit: the account is benched and the error is a ban.
//
// Mutation: in errors.go classifyCode88 (or the case 88 arm) return
// errRateLimited for every 88 => RED (account stays active, error not IsBanned).
func TestCode88_RequestsRemaining_StaysBanned(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.hdrs, fake.body = rateLimit88("5", time.Hour)
	c, accs := newPoolWaitClient(t, 1)
	acc := accs[0]

	_, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())

	require.Error(t, err)
	assert.True(t, IsBanned(err), "want IsBanned, got %v", err)
	assert.False(t, IsRateLimited(err))
	assert.False(t, acc.IsActive(), "code 88 with requests remaining must soft-deactivate the account")
	assert.False(t, acc.IsEndpointRateLimited(poolWaitEndpoint), "a ban is account-wide, not an endpoint block")
}

// doPOST has the caller's account and no rotation: an exhausted-window 88 must
// not charge the healthy account a failure.
func TestCode88_DoPOST_ExhaustedWindow_NoFailureCharged(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.hdrs, fake.body = rateLimit88("0", time.Hour)
	c, accs := newPoolWaitClient(t, 1)
	acc := accs[0]

	_, err := c.doPOST(context.Background(), acc, "CreateTweet", fake.srv.URL+"/i/api/graphql/x/CreateTweet", []byte(`{}`))

	require.Error(t, err)
	assert.True(t, IsRateLimited(err), "got %v", err)
	_, failed, _ := acc.Stats()
	assert.Equal(t, 0, failed, "a rate limit is not an account failure")
	assert.True(t, acc.IsEndpointRateLimited("CreateTweet"))
}

// A banned account that answers every endpoint with a code 88 carrying NO
// rate-limit headers is invisible to the exhausted-window classification: each
// response looks like a fresh per-endpoint limit. A run of
// headerless88BanThreshold consecutive header-less 88s (any endpoints, no
// successful response in between) is the ban signature: the account is benched
// for BanCooldown and the error satisfies IsBanned. Below the threshold the
// behaviour is today's — endpoint-limited, IsRateLimited.
//
// Mutation: in request.go doPoolRequest drop the noteRateLimited88 escalation
// check => RED.
func TestCode88_Headerless_EscalatesToBan(t *testing.T) {
	fake := newPoolWaitFake(t)
	// 200 + code 88, no x-rate-limit-* headers at all.
	fake.body = `{"errors":[{"code":88,"message":"Rate limit exceeded"}]}`
	c, accs := newPoolWaitClient(t, 1)
	acc := accs[0]
	url := func(ep string) string { return fake.srv.URL + "/i/api/graphql/x/" + ep }

	below := []string{"UserTweets", "Followers"}
	require.Len(t, below, headerless88BanThreshold-1, "test drives threshold-1 responses first")
	for i, ep := range below {
		_, _, err := c.doGET(context.Background(), ep, url(ep))
		require.Error(t, err)
		assert.True(t, IsRateLimited(err), "call %d: want IsRateLimited, got %v", i+1, err)
		assert.False(t, IsBanned(err), "call %d: a header-less 88 below the threshold is not a ban: %v", i+1, err)
		assert.True(t, acc.IsActive(), "call %d: account must stay active below the threshold", i+1)
	}

	_, _, err := c.doGET(context.Background(), "Bookmarks", url("Bookmarks"))
	require.Error(t, err)
	assert.True(t, IsBanned(err), "want IsBanned, got %v", err)
	assert.False(t, IsRateLimited(err))
	assert.False(t, acc.IsActive(), "the threshold-th consecutive header-less 88 must soft-deactivate")
	assert.WithinDuration(t, time.Now().Add(time.Hour), acc.ReactivateAt(), 5*time.Second,
		"escalation benches for BanCooldown (1h in this client)")
	assert.EqualValues(t, headerless88BanThreshold, fake.hits.Load(), "one hit per doGET")
}

// A successful response in the middle of the run proves the credentials work:
// the header-less-88 count restarts from zero, so threshold more header-less
// 88s are required before the account is benched.
//
// Mutation: in account.go RecordSuccess drop the counter reset => RED.
func TestCode88_Headerless_SuccessResetsCount(t *testing.T) {
	fake := newPoolWaitFake(t)
	c, accs := newPoolWaitClient(t, 1)
	acc := accs[0]
	url := func(ep string) string { return fake.srv.URL + "/i/api/graphql/x/" + ep }

	fake.body = `{"errors":[{"code":88,"message":"Rate limit exceeded"}]}`
	_, _, err := c.doGET(context.Background(), "UserTweets", url("UserTweets"))
	require.Error(t, err) // count = 1

	fake.body = `{"data":{"ok":true}}`
	_, _, err = c.doGET(context.Background(), "Followers", url("Followers"))
	require.NoError(t, err) // success: count resets to 0

	fake.body = `{"errors":[{"code":88,"message":"Rate limit exceeded"}]}`
	for i, ep := range []string{"Bookmarks", "HomeTimeline"} { // count = 1, 2 post-reset
		_, _, err = c.doGET(context.Background(), ep, url(ep))
		require.Error(t, err)
		assert.True(t, acc.IsActive(),
			"post-reset header-less 88 #%d must not ban: the count restarted at the success", i+1)
	}
	// And the run still escalates once it reaches the threshold again.
	_, _, err = c.doGET(context.Background(), "SearchTimeline", url("SearchTimeline"))
	require.Error(t, err)
	assert.True(t, IsBanned(err), "want IsBanned, got %v", err)
	assert.False(t, acc.IsActive())
}

// An 88 WITH x-rate-limit-remaining is explained by its window: it never
// advances the header-less-88 count, so the account is never escalated.
//
// Mutation: in account.go noteRateLimited88 advance the count for every 88
// (drop the rateLimitRemaining check) => RED.
func TestCode88_RemainingZero_NeverEscalates(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.hdrs, fake.body = rateLimit88("0", time.Hour)
	c, accs := newPoolWaitClient(t, 1)
	acc := accs[0]

	for i, ep := range []string{"UserTweets", "Followers", "Bookmarks", "HomeTimeline"} {
		_, _, err := c.doGET(context.Background(), ep, fake.srv.URL+"/i/api/graphql/x/"+ep)
		require.Error(t, err)
		assert.True(t, IsRateLimited(err), "call %d: want IsRateLimited, got %v", i+1, err)
		assert.False(t, IsBanned(err))
		assert.True(t, acc.IsActive(),
			"call %d: remaining=0 is a windowed limit, never the ban signature", i+1)
	}
}

func TestClassifyCode88_HeaderMatrix(t *testing.T) {
	cases := []struct {
		name string
		hdrs map[string]string
		want errorClass
	}{
		{"nil headers", nil, errRateLimited},
		{"remaining 0", map[string]string{xRateLimitRemaining: "0"}, errRateLimited},
		{"remaining absent", map[string]string{}, errRateLimited},
		{"remaining garbage", map[string]string{xRateLimitRemaining: "n/a"}, errRateLimited},
		{"remaining negative", map[string]string{xRateLimitRemaining: "-1"}, errRateLimited},
		{"remaining 1", map[string]string{xRateLimitRemaining: "1"}, errBanned},
		{"remaining 5", map[string]string{xRateLimitRemaining: "5"}, errBanned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyError([]byte(`{"errors":[{"code":88}]}`), tc.hdrs))
		})
	}
}

// doPOST escalates too: the account-pinned POST path (CreateTweet) counts the
// same per-account run of consecutive header-less 88s and benches the account
// at the threshold, with an IsBanned error.
//
// Mutation: in request.go doPOST drop the noteRateLimited88 escalation check
// => RED.
func TestCode88_Headerless_EscalatesToBan_DoPOST(t *testing.T) {
	fake := newPoolWaitFake(t)
	// 200 + code 88, no x-rate-limit-* headers at all.
	fake.body = `{"errors":[{"code":88,"message":"Rate limit exceeded"}]}`
	// CreateTweet builds its URL from twitterBase — point it at the fake.
	origBase := twitterBase
	twitterBase = fake.srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })

	c, accs := newPoolWaitClient(t, 1)
	acc := accs[0]

	for i := 0; i < headerless88BanThreshold-1; i++ {
		_, err := c.CreateTweet(context.Background(), acc, "hi")
		require.Error(t, err)
		assert.True(t, IsRateLimited(err), "call %d: want IsRateLimited, got %v", i+1, err)
		assert.False(t, IsBanned(err), "call %d: a header-less 88 below the threshold is not a ban: %v", i+1, err)
		assert.True(t, acc.IsActive(), "call %d: account must stay active below the threshold", i+1)
	}

	_, err := c.CreateTweet(context.Background(), acc, "hi")
	require.Error(t, err)
	assert.True(t, IsBanned(err), "want IsBanned, got %v", err)
	assert.False(t, IsRateLimited(err))
	assert.False(t, acc.IsActive(), "the threshold-th consecutive header-less 88 must soft-deactivate")
	assert.EqualValues(t, headerless88BanThreshold, fake.hits.Load(), "one hit per CreateTweet")
}

// doMediaRequest escalates too: a media upload step (INIT/APPEND/FINALIZE each
// go through it) counts the same per-account run and benches the account at
// the threshold.
//
// Mutation: in media.go doMediaRequest drop the noteRateLimited88 escalation
// check => RED.
func TestCode88_Headerless_EscalatesToBan_DoMediaRequest(t *testing.T) {
	fake := newPoolWaitFake(t)
	// 200 + code 88, no x-rate-limit-* headers at all.
	fake.body = `{"errors":[{"code":88,"message":"Rate limit exceeded"}]}`
	c, accs := newPoolWaitClient(t, 1)
	acc := accs[0]
	url := fake.srv.URL + "/i/media/upload.json"

	for i := 0; i < headerless88BanThreshold-1; i++ {
		_, err := c.doMediaRequest(context.Background(), acc, "POST", url, []byte(`x`), "application/octet-stream")
		require.Error(t, err)
		assert.True(t, IsRateLimited(err), "call %d: want IsRateLimited, got %v", i+1, err)
		assert.False(t, IsBanned(err), "call %d: a header-less 88 below the threshold is not a ban: %v", i+1, err)
		assert.True(t, acc.IsActive(), "call %d: account must stay active below the threshold", i+1)
	}

	_, err := c.doMediaRequest(context.Background(), acc, "POST", url, []byte(`x`), "application/octet-stream")
	require.Error(t, err)
	assert.True(t, IsBanned(err), "want IsBanned, got %v", err)
	assert.False(t, IsRateLimited(err))
	assert.False(t, acc.IsActive(), "the threshold-th consecutive header-less 88 must soft-deactivate")
	assert.EqualValues(t, headerless88BanThreshold, fake.hits.Load(), "one hit per doMediaRequest")
}

// Escalation benches the account AND restarts the count: after the cooldown a
// reactivated account needs a full fresh run of header-less 88s before it is
// benched again — a single header-less 88 post-reactivation is just a
// per-endpoint rate limit.
//
// Mutation: in request.go escalateHeaderless88 drop the consecHeaderless88
// reset => RED.
func TestCode88_Headerless_EscalationResetsCount(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.body = `{"errors":[{"code":88,"message":"Rate limit exceeded"}]}`
	c, accs := newPoolWaitClient(t, 1)
	acc := accs[0]
	url := func(ep string) string { return fake.srv.URL + "/i/api/graphql/x/" + ep }

	for _, ep := range []string{"UserTweets", "Followers", "Bookmarks"} {
		_, _, err := c.doGET(context.Background(), ep, url(ep))
		require.Error(t, err)
	}
	require.False(t, acc.IsActive(), "the threshold-th header-less 88 must bench the account")

	// Cooldown over: the pool auto-reactivates the account on the next pick.
	acc.SetReactivateAt(time.Now().Add(-time.Second))

	_, _, err := c.doGET(context.Background(), "HomeTimeline", url("HomeTimeline"))
	require.Error(t, err)
	assert.True(t, IsRateLimited(err), "want IsRateLimited, got %v", err)
	assert.True(t, acc.IsActive(),
		"one header-less 88 after reactivation must not re-ban: the count restarted at the escalation")
}

// UploadMedia's 2xx-body check: an exhausted-window 88 must not charge the
// healthy account a failure; the endpoint is blocked until the reset instead.
//
// Mutation: in media.go add acc.RecordFailure() to the errRateLimited arm of
// doMediaRequest => RED.
func TestCode88_DoMediaRequest_ExhaustedWindow_NoFailureCharged(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.hdrs, fake.body = rateLimit88("0", time.Hour)
	c, accs := newPoolWaitClient(t, 1)
	acc := accs[0]

	_, err := c.doMediaRequest(context.Background(), acc, "POST", fake.srv.URL+"/i/media/upload.json", []byte(`x`), "application/octet-stream")

	require.Error(t, err)
	assert.True(t, IsRateLimited(err), "got %v", err)
	_, failed, _ := acc.Stats()
	assert.Equal(t, 0, failed, "a rate limit is not an account failure")
	assert.True(t, acc.IsEndpointRateLimited(epUploadMedia))
}
