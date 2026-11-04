package twitter

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Oracle: testdata/wire_loggedin_oracle.json, the request shapes of a signed-in
// Chromium 156 browser using x.com (home timeline and a profile page), taken
// from its network log with header values masked. Only header names, their
// order and non-secret fixed values are kept. Chrome's order is fixed per
// header name, so one table covers every family; the shapes below are the
// families that differ in which headers they carry.
type loggedInShape struct {
	Method  string            `json:"method"`
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	Count   int               `json:"count"`
	Order   []string          `json:"order"`
	Values  map[string]string `json:"values"`
	Referer string            `json:"referer"` // "page" | "site-root"
}

func loadLoggedInOracle(t *testing.T) []loggedInShape {
	t.Helper()
	raw, err := os.ReadFile("testdata/wire_loggedin_oracle.json")
	require.NoError(t, err)
	var f struct {
		Provenance struct {
			Absent []string `json:"absent_in_all_requests"`
		} `json:"provenance"`
		Shapes []loggedInShape `json:"shapes"`
	}
	require.NoError(t, json.Unmarshal(raw, &f))
	require.Equal(t, []string{"x-xp-forwarded-for"}, f.Provenance.Absent)
	require.NotEmpty(t, f.Shapes)
	return f.Shapes
}

// Headers a browser sends that go-twitter has no call for (a conditional GET's
// validator, the live-pipeline session id, the polling marker): the oracle's
// order is compared with them removed.
var oracleNotSet = []string{"x-twitter-polling", "if-none-match", "livepipeline-session"}

// shapeURL is a request URL of the shape's family.
func (s loggedInShape) url() string {
	if s.Path == "graphql" {
		return "https://x.com/i/api/graphql/q/HomeTimeline"
	}
	return "https://" + s.Host + s.Path
}

// A signed-in current identity sends every request family in the order the
// browser does, with no x-xp-forwarded-for, and carries the fixed values the
// browser sent.
//
// Mutation: wire.go chromeHeaderOrder — move "x-twitter-auth-type" before
// "x-client-transaction-id" => RED on every family row. Mutation: client.go
// doRequestWithBody — generate x-xp-forwarded-for for every generation => RED
// on the NotContains assertion of every row.
func TestLoggedInOracle_OrderAndValuesPerFamily(t *testing.T) {
	for _, s := range loadLoggedInOracle(t) {
		if s.Path == "/i/api/1.1/videoads/v2/prerolls.json" {
			continue // see TestLoggedInOracle_VideoadsDiverges
		}
		t.Run(s.Method+" "+s.Host+" "+s.Path, func(t *testing.T) {
			c, bc, rec, ua := chromeClient(t)
			h := twitterHeaders("AUTH", "CT0", ua)
			var body *strings.Reader
			if s.Method == "POST" {
				body = strings.NewReader("{}")
				if s.Values["content-type"] != "" {
					h["content-type"] = s.Values["content-type"]
				}
			}
			if body != nil {
				_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, s.Method, s.url(), h, body)
			} else {
				_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, s.Method, s.url(), h, nil)
			}
			got := rec.last()
			assert.Equal(t, without(s.Order, oracleNotSet...), wireNames(got), "wire order")
			assert.NotContains(t, got.Headers, "x-xp-forwarded-for", "the signed-in browser sends none")
			for _, k := range []string{"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "accept", "accept-encoding", "accept-language", "priority", "x-twitter-auth-type", "sec-ch-ua-mobile"} {
				assert.Equal(t, s.Values[k], got.Headers[k], k)
			}
			if ct, ok := s.Values["content-type"]; ok {
				assert.Equal(t, ct, got.Headers["content-type"], "content-type")
			} else {
				assert.NotContains(t, got.Headers, "content-type")
			}
			if s.Referer == "site-root" {
				assert.Equal(t, "https://x.com/", got.Headers["referer"], "api.x.com requests keep the site root")
			} else {
				assert.True(t, strings.HasPrefix(got.Headers["referer"], "https://x.com/"))
			}
		})
	}
}

// The one place the capture disagrees with the table: the browser's
// videoads call wrote accept before content-type, every other form or JSON
// POST (GraphQL and the live-pipeline form POST) wrote content-type first. One
// sample against many, so the table keeps content-type first; this pins the
// divergence so a later capture that makes it consistent is noticed.
func TestLoggedInOracle_VideoadsDiverges(t *testing.T) {
	var video, live loggedInShape
	for _, s := range loadLoggedInOracle(t) {
		switch s.Path {
		case "/i/api/1.1/videoads/v2/prerolls.json":
			video = s
		case "/1.1/live_pipeline/update_subscriptions":
			live = s
		}
	}
	require.NotEmpty(t, video.Order)
	idx := func(o []string, n string) int {
		for i, x := range o {
			if x == n {
				return i
			}
		}
		return -1
	}
	assert.Less(t, idx(video.Order, "accept"), idx(video.Order, "content-type"))
	assert.Less(t, idx(live.Order, "content-type"), idx(live.Order, "accept"))
	assert.Less(t, idx(chromeHeaderOrder, "content-type"), idx(chromeHeaderOrder, "accept"))
}

// A current identity's same-origin request carries the page a user would be on
// for that operation; one table maps the operation.
//
// Mutation: wire.go pageReferer — return "https://x.com/" for every operation
// => RED on every row.
func TestPageReferer(t *testing.T) {
	vars := func(m string) string {
		return "https://x.com/i/api/graphql/ID/" + m
	}
	withVars := func(op, v string) string {
		return vars(op) + "?variables=" + url.QueryEscape(v) + "&features=%7B%7D"
	}
	cases := []struct{ name, url, want string }{
		{"home", withVars("HomeTimeline", `{"count":20}`), "https://x.com/home"},
		{"home latest", withVars("HomeLatestTimeline", `{"count":20}`), "https://x.com/home"},
		{"profile", withVars("UserByScreenName", `{"screen_name":"NASA"}`), "https://x.com/NASA"},
		{"profile handle is path-escaped", withVars("UserByScreenName", `{"screen_name":"a/b?c"}`), "https://x.com/a%2Fb%3Fc"},
		{"user tweets: only an id is known", withVars("UserTweets", `{"userId":"1"}`), "https://x.com/home"},
		{"search", withVars("SearchTimeline", `{"rawQuery":"go lang & x"}`), "https://x.com/search?q=go%20lang%20%26%20x"},
		{"tweet detail", withVars("TweetDetail", `{"focalTweetId":"123"}`), "https://x.com/i/status/123"},
		{"notifications", "https://x.com/i/api/graphql/ID/tweetNotificationsQuery", "https://x.com/notifications"},
		{"bookmarks", withVars("Bookmarks", `{}`), "https://x.com/i/bookmarks"},
		{"list", withVars("ListLatestTweetsTimeline", `{"listId":"77"}`), "https://x.com/i/lists/77"},
		{"unknown operation", vars("Whatever"), "https://x.com/home"},
		{"search without a query", vars("SearchTimeline"), "https://x.com/home"},
		{"malformed variables", withVars("UserByScreenName", `{`), "https://x.com/home"},
		{"REST path", "https://x.com/i/api/1.1/hashflags.json", "https://x.com/home"},
		{"unparsable url", "://", "https://x.com/home"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pageReferer(tc.url, pageSource{}))
		})
	}
}

// Referer on the wire: current identities send the page on same-origin x.com
// requests and the site root on api.x.com; the frozen legacy shape always sends
// the site root (its goldens: TestLegacyFreeze_*).
//
// Mutation: wire.go shape — set the page referer whatever the fetch site
// => RED on the api.x.com rows. Mutation: wire.go shape — set it before the
// legacy early return => RED on the legacy row.
func TestChromeWire_RefererFollowsTheOperation(t *testing.T) {
	c, bc, rec, ua := chromeClient(t)
	send := func(u string) string {
		_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET", u, twitterHeaders("AUTH", "CT0", ua), nil)
		return rec.last().Headers["referer"]
	}
	q := "?variables=" + url.QueryEscape(`{"screen_name":"NASA"}`)
	assert.Equal(t, "https://x.com/NASA", send("https://x.com/i/api/graphql/ID/UserByScreenName"+q))
	assert.Equal(t, "https://x.com/home", send("https://x.com/i/api/graphql/ID/HomeTimeline"))
	assert.Equal(t, "https://x.com/", send("https://api.x.com/1.1/account/settings.json"))
	assert.Equal(t, "https://x.com/", send("https://api.twitter.com/1.1/account/settings.json"), "cross-site keeps the site root")

	lc, lbc, lrec := legacyClient(t)
	_, _, _, _ = lc.doRequestWithBody(context.Background(), nil, lbc, "GET", "https://x.com/i/api/graphql/ID/UserByScreenName"+q, twitterHeaders("AUTH", "CT0", legacyProfile.UserAgent), nil)
	assert.Equal(t, "https://x.com/", lrec.last().Headers["referer"])
}
