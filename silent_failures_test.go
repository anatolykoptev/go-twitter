package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Silent-failure surface: an errors-only 200 must reach the caller as a typed
// error, a post-relogin retry must report its own error class, and a query
// value must survive percent-encoding intact.

// S1 — journey: a poller asks for a user's tweets and the API answers HTTP
// 200 with only an errors[] envelope carrying an unlisted code (classifyError
// returns errNone for it). The call must come back as a typed *APIError
// carrying the codes — not "no new tweets" — and the account must not be
// charged, benched, or sent to relogin: the cause is unknown and is not
// provably the account's.
//
// Mutation: in request.go doPoolRequest, make the errors-only arm return the
// old empty success (drop the arm) => RED.
func TestErrorsOnly200_UserTweets_ReturnsTypedError(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.body = `{"errors":[{"code":999,"message":"unlisted failure"}]}`

	c, accs := newPoolWaitClient(t, 1)
	origBase := twitterBase
	twitterBase = fake.srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })

	tweets, err := c.GetUserTweets(context.Background(), "999", 20)
	require.Error(t, err, "an errors-only 200 is a failed call, not 'no tweets'")
	assert.Nil(t, tweets)

	var ae *APIError
	require.True(t, errors.As(err, &ae), "want typed *APIError, got %T: %v", err, err)
	assert.Contains(t, ae.Codes, 999, "the body's error codes must reach the caller")
	assert.True(t, errors.Is(err, ErrUnexpectedResponse), "unrecognised errors-only 200 is its own terminal class, got %v", err)
	assert.False(t, errors.Is(err, ErrEndpoint), "an executed query is not a stale request shape")
	assert.False(t, IsAuthExpired(err), "an errors-only 200 must not trigger the auth-expired path")

	total, failed, _ := accs[0].Stats()
	assert.Zero(t, failed, "the cause is unknown: the account must not be charged")
	assert.Zero(t, total)
	assert.True(t, accs[0].IsActive(), "the account must not be benched")
	assert.EqualValues(t, 1, fake.hits.Load(), "one serving account, one attempt")
}

// S2 — same journey, but the 200 carries usable data AND a routine partial
// error (the way one unavailable tweet in a timeline arrives): the call
// returns the tweet and nil error, exactly like a clean 200.
//
// Mutation: in request.go doPoolRequest, drop the "has usable data"
// condition from the errors-only guard so any errors[] fails => RED.
func TestErrorsWithData_UserTweets_Succeeds(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.body = `{"errors":[{"code":144,"message":"No data found for a tweet"}],` +
		strings.TrimPrefix(userTweetsBody(snowflakeAt(time.Now())), "{")

	c, _ := newPoolWaitClient(t, 1)
	origBase := twitterBase
	twitterBase = fake.srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })

	tweets, err := c.GetUserTweets(context.Background(), "999", 20)
	require.NoError(t, err, "usable data wins over routine partial errors")
	require.Len(t, tweets, 1)
	assert.EqualValues(t, 1, fake.hits.Load())
}

// S3 — journey: the account's token expires (401 + code 32), relogin
// succeeds, and the retry answers 200 carrying code 64 — the account is
// suspended. The returned error must report the retry's own class
// (IsSuspended) and the account must be benched permanently, not reported as
// auth-expired and soft-cooled for a later retry.
//
// Coverage: every post-relogin failure site — the doPoolRequest 401/403 and
// 200 arms, the captcha-unlock retry, recoverCSRF, recoverCSRFPost and the
// doPOST arm — routes through postReloginFailure, so this journey (the
// doPoolRequest 401 arm) exercises the shared code all of them use.
//
// Mutation: in request.go, restore `Class: errAuthExpired` inside
// postReloginFailure => RED.
func TestPostReloginRetry_ReportsItsOwnClass(t *testing.T) {
	var hits atomic.Int32 // GraphQL hits only; the login flow rides the same server
	j := &reloginJourney{
		gqlHits: &hits,
		answerAuthed: func(w http.ResponseWriter, _ *http.Request) {
			// The post-relogin retry carries the account's real verdict.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"errors":[{"code":64,"message":"Account is suspended"}]}`))
		},
	}
	bc := proxyHopFakeX(t, j.serve)

	c, accs := newPoolWaitClient(t, 1)
	accs[0].Password = "pw" // relogin logs in on a token-free candidate account
	c.cfg.SessionStore = newRecordingStore()
	c.client = bc
	accs[0].client = bc // the login flow goes through the account's own transport

	_, err := c.GetUserTweets(context.Background(), "999", 20)
	require.Error(t, err)
	assert.True(t, IsSuspended(err), "the retry's code 64 must classify suspended, got %v", err)
	assert.False(t, IsAuthExpired(err), "a suspended retry must not masquerade as auth-expired")

	var ae *APIError
	require.True(t, errors.As(err, &ae))
	assert.Contains(t, ae.Codes, 64)
	assert.False(t, accs[0].IsActive(), "a suspended account is benched permanently")
	assert.EqualValues(t, 2, hits.Load(), "initial request + post-relogin retry")
}

// S4 — journey: a cursor containing '#', '+', '%', '=' and non-ASCII text
// must round-trip through the built URL: url.ParseQuery on the query yields
// exactly the JSON that went in. A raw '#' would truncate the URL into a
// fragment, '+' would decode as a space, '%' would start a bogus escape, and
// '=' would split the pair early.
//
// Mutation: in request.go jsonEscape, let '#' through raw => RED.
func TestJSONEscape_ReservedCharsRoundTrip(t *testing.T) {
	variables := map[string]any{
		"userId": "123",
		"cursor": `Hb+/#=%тест`, // one of every reserved class in a single value
	}
	built := addGraphQLParams("https://x.com/i/api/graphql/x/Op", variables, map[string]any{"f": true})

	u, err := url.Parse(built)
	require.NoError(t, err)
	vals, err := url.ParseQuery(u.RawQuery)
	require.NoError(t, err)

	wantVars, _ := json.Marshal(variables)
	assert.Equal(t, string(wantVars), vals.Get("variables"))
}

// S5 — byte-identity pin for the hot path: the encoding of the inputs
// today's callers actually send (numeric ids, fixed feature-flag names,
// booleans) must be unchanged.
//
// Mutation: in request.go jsonEscape, encode space as '+' => RED.
func TestJSONEscape_HotPathByteIdentical(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			`{"userId":"123456789","count":20,"includePromotedContent":false,"withVoice":true}`,
			`%7B%22userId%22%3A%22123456789%22%2C%22count%22%3A20%2C%22includePromotedContent%22%3Afalse%2C%22withVoice%22%3Atrue%7D`,
		},
		{
			`{"responsive_web_graphql_exclude_directive_enabled":true,"verified_phone_label_enabled":false}`,
			`%7B%22responsive_web_graphql_exclude_directive_enabled%22%3Atrue%2C%22verified_phone_label_enabled%22%3Afalse%7D`,
		},
		{
			`{"rawQuery":"from:test golang"}`,
			`%7B%22rawQuery%22%3A%22from%3Atest%20golang%22%7D`,
		},
		{`plain-unreserved_1.0~ok`, `plain-unreserved_1.0~ok`},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, jsonEscape([]byte(tc.in)), "input %s", tc.in)
	}
}

// #47 — isProxyError strips the quoted request URL before matching proxy
// keywords; user content carrying a quote plus the word "proxy" must be
// stripped with it. That strip depends on jsonEscape never emitting a raw
// '"': a raw quote inside the URL would end the regex match early and leave
// the hostile tail visible to the matcher.
//
// Mutation: in request.go jsonEscape, let '"' through raw => RED (the tail
// containing "proxy" survives the strip).
func TestJSONEscape_QuotedURLStripDependsOnEncodedQuotes(t *testing.T) {
	variables := map[string]any{
		"rawQuery": `x" onerror=proxy "`, // user input shaped to escape the strip
		"count":    20,
	}
	built := addGraphQLParams("https://x.com/i/api/graphql/x/SearchTimeline", variables, map[string]any{"f": true})

	require.NotContains(t, built, `"`, "the built URL must carry no raw quote")
	msg := `Get "` + built + `": dial tcp: i/o timeout`
	stripped := quotedURLRe.ReplaceAllString(msg, "")
	assert.NotContains(t, stripped, "proxy",
		"the quoted-URL strip must remove the whole URL, hostile tail included")
}
