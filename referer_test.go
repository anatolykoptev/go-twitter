package twitter

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encodeURIComponent semantics, from ECMA-262 (the unreserved set is
// A-Za-z0-9 - _ . ! ~ * ' ( ) ): a space is %20, "+" and "&" are escaped, the
// marks are not. No capture of the web client's own search URL is in the oracle.
//
// Mutation: wire.go operationPages SearchTimeline — url.QueryEscape instead of
// encodeURIComponent => RED.
func TestPageReferer_SearchEncodesLikeEncodeURIComponent(t *testing.T) {
	op := func(q string) string {
		return "https://x.com/i/api/graphql/ID/SearchTimeline?variables=" + url.QueryEscape(`{"rawQuery":`+jsonString(q)+`}`)
	}
	assert.Equal(t, "https://x.com/search?q=go%20lang", pageReferer(op("go lang"), pageSource{}))
	assert.Equal(t, "https://x.com/search?q=a%2Bb%26c%3Dd%23e", pageReferer(op("a+b&c=d#e"), pageSource{}))
	assert.Equal(t, "https://x.com/search?q='()!*~-_.", pageReferer(op("'()!*~-_."), pageSource{}), "the marks encodeURIComponent keeps")
	assert.Equal(t, "https://x.com/search?q=%D0%BF%D1%80", pageReferer(op("пр"), pageSource{}))
}

func jsonString(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// A referer past 4096 bytes is the site root, as the browser sends.
//
// Mutation: wire.go pageReferer — delete the maxRefererBytes check => RED.
func TestPageReferer_CappedAt4096(t *testing.T) {
	long := strings.Repeat("a", 3000)
	u := "https://x.com/i/api/graphql/ID/SearchTimeline?variables=" + url.QueryEscape(`{"rawQuery":"`+long+`"}`)
	got := pageReferer(u, pageSource{})
	assert.Len(t, got, len("https://x.com/search?q=")+3000, "under the cap it is sent in full")
	long = strings.Repeat("a", 4100)
	u = "https://x.com/i/api/graphql/ID/SearchTimeline?variables=" + url.QueryEscape(`{"rawQuery":"`+long+`"}`)
	assert.Equal(t, "https://x.com/", pageReferer(u, pageSource{}))
	// the boundary: the whole referer is exactly maxRefererBytes
	pad := maxRefererBytes - len("https://x.com/search?q=")
	u = "https://x.com/i/api/graphql/ID/SearchTimeline?variables=" + url.QueryEscape(`{"rawQuery":"`+strings.Repeat("a", pad)+`"}`)
	assert.Len(t, pageReferer(u, pageSource{}), maxRefererBytes)
	u = "https://x.com/i/api/graphql/ID/SearchTimeline?variables=" + url.QueryEscape(`{"rawQuery":"`+strings.Repeat("a", pad+1)+`"}`)
	assert.Equal(t, "https://x.com/", pageReferer(u, pageSource{}))
}

// UserTweets sends the profile page when the handle of the user id is known.
//
// Mutation: wire.go operationPages — delete the UserTweets entry => RED on the
// known-handle row.
func TestPageReferer_UserTweetsUsesAKnownHandle(t *testing.T) {
	u := "https://x.com/i/api/graphql/ID/UserTweets?variables=" + url.QueryEscape(`{"userId":"42"}`)
	known := pageSource{handle: func(id string) string {
		if id == "42" {
			return "NASA"
		}
		return ""
	}}
	assert.Equal(t, "https://x.com/NASA", pageReferer(u, known))
	assert.Equal(t, "https://x.com/home", pageReferer(u, pageSource{}), "unknown handle")
	assert.Equal(t, "https://x.com/home", pageReferer(strings.Replace(u, "42", "43", 1), known), "another user")
}

// A SearchTimeline POST has its variables in the body; the referer is built from
// it and the body still reaches the transport whole.
//
// Mutation: client.go pageSourceFor — leave src.body unset => RED.
func TestSearchPost_RefererFromBody(t *testing.T) {
	c, bc, rec, ua := chromeClient(t)
	payload := []byte(`{"variables":{"rawQuery":"go lang","count":20},"features":{}}`)
	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "POST", "https://x.com/i/api/graphql/ID/SearchTimeline",
		twitterHeaders("AUTH", "CT0", ua), bytes.NewReader(payload))
	got := rec.last()
	assert.Equal(t, "https://x.com/search?q=go%20lang", got.Headers["referer"])
	assert.Equal(t, payload, got.Body, "peeking at the body must not consume it")
}

// GetUserByScreenName teaches the Client the handle, and the next UserTweets
// referer is the profile.
//
// Mutation: graphql.go GetUserByScreenName — delete c.handles.put => RED.
func TestUserTweetsReferer_FilledByGetUserByScreenName(t *testing.T) {
	fixture, err := os.ReadFile("testdata/ubsn_cz_binance.json")
	require.NoError(t, err)
	rec := &wireRecorder{reply: fixture}
	acc := &Account{Username: "ref", AuthToken: "t", CT0: "c"}
	c, err := NewClient(ClientConfig{Accounts: []*Account{acc}, DefaultProxy: deadProxy,
		stealthOpts: []stealth.ClientOption{stealth.WithBackend(rec.factory)}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.warmDoerOverride = &fakeWarmBackend{}

	user, err := c.GetUserByScreenName(context.Background(), "cz_binance")
	require.NoError(t, err)
	require.Equal(t, "cz_binance", user.Handle)

	rec.reply = []byte(`{}`)
	_, _ = c.GetUserTweets(context.Background(), user.ID, 5)
	got := rec.last()
	assert.Contains(t, got.URL, "/UserTweets")
	assert.Equal(t, "https://x.com/cz_binance", got.Headers["referer"])
}
