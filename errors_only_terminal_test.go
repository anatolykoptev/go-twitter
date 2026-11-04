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

// An errors-only 2xx (an errors[] envelope with no usable data) proves the
// query EXECUTED: the answer is deterministic, so every arm that sees one
// returns it at once — a recognised miss as ErrNotFound, anything else as
// ErrUnexpectedResponse — with no retry, rotation, guest fallback, relogin,
// second lease or account charge. Each test below drives one arm through a
// real call and counts the requests the fake server saw.

const unrecognisedErrorsOnly = `{"errors":[{"code":999,"message":"unlisted failure"}]}`

// T1 — journey: a 3-account pool, UserTweets answered with an errors-only 200
// carrying an unrecognised code. One request, the new sentinel, nobody charged.
//
// Mutation: request.go doPoolRequest errors-only arm — replace the
// `return nil, nil, &APIError{...}` with `lastErr = &APIError{...}; continue`
// => RED (3 hits, not 1).
func TestErrorsOnly200_Unrecognised_OneRequestNoRotation(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.body = unrecognisedErrorsOnly
	c, accs := newPoolWaitClient(t, 3)
	origBase := twitterBase
	twitterBase = fake.srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })

	tweets, err := c.GetUserTweets(context.Background(), "999", 20)
	require.Error(t, err)
	assert.Nil(t, tweets)
	assert.ErrorIs(t, err, ErrUnexpectedResponse)
	assert.NotErrorIs(t, err, ErrEndpoint, "an executed query is not a stale request shape")
	assert.NotErrorIs(t, err, ErrNotFound)
	var ae *APIError
	require.True(t, errors.As(err, &ae))
	assert.Contains(t, ae.Codes, 999, "the codes stay on the *APIError")
	assert.EqualValues(t, 1, fake.hits.Load(), "terminal: no rotation to the other accounts, no guest request")
	for i, a := range accs {
		total, failed, _ := a.Stats()
		assert.Zero(t, failed, "account %d must not be charged", i)
		assert.Zero(t, total, "account %d", i)
		assert.True(t, a.IsActive(), "account %d must not be benched", i)
	}
}

// T2 — journey: TweetDetail answered errors-only with each recognised miss
// shape. ErrNotFound, one request.
//
// Mutation: errors.go errorsOnlyClass — make `miss` return false for every
// input (or drop a case from `case 34, 50, 63, 144`, or the NotFoundError /
// "_Missing:" tests) => the matching subtest goes RED (ErrUnexpectedResponse).
func TestErrorsOnly200_RecognisedMiss_IsNotFound(t *testing.T) {
	shapes := map[string]string{
		"code34":             `{"errors":[{"code":34,"message":"Sorry, that page does not exist"}]}`,
		"code50":             `{"errors":[{"code":50,"message":"User not found"}]}`,
		"code63":             `{"errors":[{"code":63,"message":"User has been suspended"}]}`,
		"code144":            `{"errors":[{"code":144,"message":"No status found with that ID."}]}`,
		"name":               `{"errors":[{"message":"x","name":"NotFoundError"}]}`,
		"kind":               `{"errors":[{"message":"x","kind":"NotFoundError"}]}`,
		"extensionsName":     `{"errors":[{"message":"x","extensions":{"name":"NotFoundError"}}]}`,
		"extensionsKind":     `{"errors":[{"message":"x","extensions":{"kind":"NotFoundError"}}]}`,
		"extensionsCode":     `{"errors":[{"message":"x","extensions":{"code":144}}]}`,
		"missingMessage":     `{"errors":[{"message":"_Missing: No status found with that ID."}]}`,
		"missAmongUnrelated": `{"errors":[{"code":999,"message":"noise"},{"code":144,"message":"gone"}]}`,
	}
	for name, body := range shapes {
		t.Run(name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = body
			c, accs := newPoolWaitClient(t, 3)
			origBase := twitterBase
			twitterBase = fake.srv.URL + "/i/api/graphql"
			t.Cleanup(func() { twitterBase = origBase })

			_, err := c.GetTweetByID(context.Background(), "123")
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrNotFound)
			assert.NotErrorIs(t, err, ErrUnexpectedResponse)
			assert.NotErrorIs(t, err, ErrEndpoint)
			assert.EqualValues(t, 1, fake.hits.Load(), "a miss is deterministic: one request")
			for _, a := range accs {
				_, failed, _ := a.Stats()
				assert.Zero(t, failed)
				assert.True(t, a.IsActive())
			}
		})
	}
}

// Codes that look similar but are not misses stay unrecognised.
func TestErrorsOnlyClass_NonMissStaysUnexpected(t *testing.T) {
	for _, body := range []string{
		`{"errors":[{"code":999,"message":"x"}]}`,
		`{"errors":[{"message":"Missing: nothing special"}]}`, // no underscore prefix
		`{"errors":[{"message":"x","name":"OtherError"}]}`,
	} {
		assert.Equal(t, errUnexpected, errorsOnlyClass([]byte(body)), body)
	}
}

// T3 — journey: 401 code 32, relogin succeeds, the retry answers errors-only
// with an unrecognised code. The account was just answered for real: it stays
// active, the error is the new sentinel (not auth-expired), no further
// attempt is made.
//
// Mutation: request.go postReloginFailure — delete the `if errorsOnly(body)`
// branch so `cls = fallback` always => RED (auth-expired + account benched).
func TestPostRelogin_ErrorsOnlyRetry_NotAuthExpired(t *testing.T) {
	c, acc, j := newReloginJourneyClient(t, seededStore(), "pw")
	var hits atomic.Int32
	j.gqlHits = &hits
	j.answerAuthed = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(unrecognisedErrorsOnly))
	}

	err := getTweets(t, c)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnexpectedResponse)
	assert.False(t, IsAuthExpired(err), "an errors-only retry is not an auth verdict")
	assert.True(t, acc.IsActive(), "a freshly logged-in account that was answered must not be benched")
	assert.EqualValues(t, 2, hits.Load(), "initial request + post-relogin retry; the terminal answer stops the loop")
}

// T4 — doPOST arm: a write answered errors-only is returned at once.
//
// Mutation: request.go doPOST — `if false && errClass == errNone &&
// errorsOnly(body)` => RED (the body comes back as a success).
func TestDoPOST_ErrorsOnly200_Terminal(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want error
	}{
		"unrecognised": {unrecognisedErrorsOnly, ErrUnexpectedResponse},
		"miss":         {`{"errors":[{"code":144,"message":"gone"}]}`, ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = tc.body
			c, accs := newPoolWaitClient(t, 2)
			body, err := c.doPOST(context.Background(), accs[0], "CreateTweet",
				fake.srv.URL+"/i/api/graphql/x/CreateTweet", []byte(`{}`))
			require.Error(t, err, "an errors-only 200 must not read as a success")
			assert.Nil(t, body)
			assert.ErrorIs(t, err, tc.want)
			assert.EqualValues(t, 1, fake.hits.Load(), "a write is never replayed")
			_, failed, _ := accs[0].Stats()
			assert.Zero(t, failed)
			assert.True(t, accs[0].IsActive())
		})
	}
}

// T5 — media arm: an upload step answered errors-only is returned at once.
//
// Mutation: media.go doMediaRequest — `if false && cls == errNone &&
// errorsOnly(body)` => RED (the body comes back as a success).
func TestDoMediaRequest_ErrorsOnly200_Terminal(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.body = unrecognisedErrorsOnly
	c, accs := newPoolWaitClient(t, 1)
	body, err := c.doMediaRequest(context.Background(), accs[0], "POST",
		fake.srv.URL+"/1.1/media/upload.json?command=INIT", nil, "application/x-www-form-urlencoded")
	require.Error(t, err, "an errors-only 200 must not read as a success")
	assert.Nil(t, body)
	assert.ErrorIs(t, err, ErrUnexpectedResponse)
	assert.EqualValues(t, 1, fake.hits.Load())
	_, failed, _ := accs[0].Stats()
	assert.Zero(t, failed)
	assert.True(t, accs[0].IsActive())
}

// T5b — media post-relogin arm: 401 code 32, relogin succeeds, the retry
// answers errors-only. Terminal, account kept.
//
// Mutation: media.go doMediaRequest post-relogin — `if false &&
// apiErr.Class == errNone && errorsOnly(body2)` => RED (the loop retries).
func TestDoMediaRequest_PostReloginErrorsOnly_Terminal(t *testing.T) {
	c, acc, j := newReloginJourneyClient(t, seededStore(), "pw")
	var hits atomic.Int32
	j.gqlHits = &hits
	j.answerAuthed = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(unrecognisedErrorsOnly))
	}
	_, err := c.doMediaRequest(context.Background(), acc, "POST",
		strings.TrimSuffix(twitterBase, "/i/api/graphql")+"/1.1/media/upload.json?command=INIT", nil, "application/x-www-form-urlencoded")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnexpectedResponse)
	assert.False(t, IsAuthExpired(err))
	assert.True(t, acc.IsActive())
	assert.EqualValues(t, 2, hits.Load(), "initial request + post-relogin retry only")
}

// guestFake answers authenticated requests (no x-guest-token header) with 404
// so a guest-eligible read falls through to the guest fallback; guest
// requests are answered by guestAnswer; the guest-activation call hands out
// "fresh-guest".
type guestFake struct {
	srv        *httptest.Server
	guestHits  atomic.Int32
	activated  atomic.Int32
	guestReply func(w http.ResponseWriter, token string, n int32)
}

func newGuestFake(t *testing.T) *guestFake {
	t.Helper()
	g := &guestFake{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/guest/activate.json"):
			g.activated.Add(1)
			_, _ = w.Write([]byte(`{"guest_token":"fresh-guest"}`))
		case r.Header.Get("x-guest-token") != "":
			g.guestReply(w, r.Header.Get("x-guest-token"), g.guestHits.Add(1))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Not Found"}]}`))
		}
	}))
	t.Cleanup(g.srv.Close)
	orig := twitterAPIURL
	twitterAPIURL = g.srv.URL
	t.Cleanup(func() { twitterAPIURL = orig })
	return g
}

// T6a — guest fallback, direct arm: the pool fails, the guest request answers
// errors-only 200. Returned at once as the typed class.
//
// Mutation: request.go doPoolRequest final guest arm — `if false &&
// errorsOnly(body)` (the one after the guest `status != 200` block) => RED.
func TestGuestFallback_ErrorsOnly200_Terminal(t *testing.T) {
	g := newGuestFake(t)
	g.guestReply = func(w http.ResponseWriter, _ string, _ int32) { _, _ = w.Write([]byte(unrecognisedErrorsOnly)) }

	c, _ := newPoolWaitClient(t, 1)
	c.cfg.DisableGuestFallback = false
	c.setGuestToken("cached-guest")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	body, _, err := c.doGET(ctx, "UserByRestId", g.srv.URL+"/i/api/graphql/x/UserByRestId")
	require.Error(t, err, "an errors-only guest answer must not read as a success")
	assert.Nil(t, body)
	assert.ErrorIs(t, err, ErrUnexpectedResponse)
	assert.EqualValues(t, 1, g.guestHits.Load())
}

// T6b — guest fallback, token-refresh arm: the cached guest token is
// rejected (403), a fresh one is activated, and the retry answers errors-only
// 200. Returned at once.
//
// Mutation: request.go doPoolRequest guest-retry arm — `if false &&
// errorsOnly(body)` (the one inside the 401/403 re-acquire block) => RED.
func TestGuestFallback_RefreshedTokenErrorsOnly200_Terminal(t *testing.T) {
	g := newGuestFake(t)
	g.guestReply = func(w http.ResponseWriter, token string, _ int32) {
		if token == "cached-guest" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":[{"code":239,"message":"Bad guest token."}]}`))
			return
		}
		_, _ = w.Write([]byte(unrecognisedErrorsOnly))
	}

	c, _ := newPoolWaitClient(t, 1)
	c.cfg.DisableGuestFallback = false
	c.setGuestToken("cached-guest")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	body, _, err := c.doGET(ctx, "UserByRestId", g.srv.URL+"/i/api/graphql/x/UserByRestId")
	require.Error(t, err, "an errors-only guest answer must not read as a success")
	assert.Nil(t, body)
	assert.ErrorIs(t, err, ErrUnexpectedResponse)
	assert.EqualValues(t, 1, g.activated.Load())
	assert.EqualValues(t, 2, g.guestHits.Load())
}

// T7 — *WithSocial: an unrecognised errors-only answer is X-side and
// deterministic — one lease, one X request, reported as a healthy-account
// outcome, no second lease.
//
// Mutation: social.go withSocialAccount — drop `|| errors.Is(err,
// ErrUnexpectedResponse)` from the terminal check => RED (leases rotate).
func TestSocial_ErrorsOnlyUnrecognised_NoSecondLease(t *testing.T) {
	var xHits atomic.Int32
	r := newSocialRigX(t, func(w http.ResponseWriter, _ *http.Request) {
		xHits.Add(1)
		_, _ = w.Write([]byte(unrecognisedErrorsOnly))
	})

	_, err := GetUserWithSocial(context.Background(), r.sc, "someone")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnexpectedResponse)
	assert.EqualValues(t, 1, r.acquire.Load(), "no second lease for an identical answer")
	assert.EqualValues(t, 1, xHits.Load())
}
