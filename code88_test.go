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
