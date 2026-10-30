package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseDeviceFollow reads the sanitized fixture captured during the smoke
// run and asserts the parsed (tweet_id, author handle, text) triples. Fixture
// contents: two target-author tweets (one plain original, one RT) and one
// non-target tweet.
func TestParseDeviceFollow(t *testing.T) {
	body, err := os.ReadFile("testdata/device_follow.json")
	require.NoError(t, err)

	tweets, err := parseDeviceFollow(body)
	require.NoError(t, err)
	require.Len(t, tweets, 3, "fixture carries exactly three tweets")

	got := make(map[string]struct{ handle, text string }, len(tweets))
	for _, tw := range tweets {
		got[tw.ID] = struct{ handle, text string }{tw.AuthorHandle, tw.Text}
		assert.NotEmpty(t, tw.AuthorID, "tweet %s: author id", tw.ID)
	}

	want := map[string]struct{ handle, text string }{
		"1985000000000000001": {"DeItaone", "*FED HOLDS RATES STEADY"},
		"1985000000000000002": {"zerohedge", "RT @Reuters: markets up"},
		"1985000000000000003": {"NotATarget", "unrelated post"},
	}
	for id, w := range want {
		g, ok := got[id]
		require.True(t, ok, "tweet %s missing", id)
		assert.Equal(t, w.handle, g.handle, "tweet %s author handle", id)
		assert.Equal(t, w.text, g.text, "tweet %s text", id)
	}
}

// TestParseDeviceFollow_Errors proves an errors[] body surfaces, not an empty
// page.
func TestParseDeviceFollow_Errors(t *testing.T) {
	_, err := parseDeviceFollow([]byte(`{"errors":[{"code":88,"message":"Rate limit exceeded"}]}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error 88")
}

// tweetNotificationsFixture is one TimelineAddEntries instruction inside the
// tweetNotificationsQuery root — the same URT shape every other timeline test
// exercises, only the wrapper path differs.
const tweetNotificationsFixture = `{"data": {"viewer_v2": {"user_results": {"result": {"tweet_notification_timeline": {"timeline": {"instructions": [{"type": "TimelineAddEntries", "entries": [{"entryId": "tweet-1985000000000000009", "content": {"entryType": "TimelineTimelineItem", "itemContent": {"__typename": "TimelineTweet", "tweet_results": {"result": {"__typename": "Tweet", "rest_id": "1985000000000000009", "core": {"user_results": {"result": {"__typename": "User", "rest_id": "42", "core": {"screen_name": "BNOWeird", "name": "BNO"}}}}, "legacy": {"full_text": "bell tweet", "user_id_str": "42"}}}}}}]}]}}}}}}}`

// TestParseTweetNotifications proves the GraphQL fallback root
// (viewer_v2.user_results.result.tweet_notification_timeline.timeline) yields
// the same (id, handle) pairs a URT timeline carries anywhere else.
func TestParseTweetNotifications(t *testing.T) {
	tweets, _, err := parseTweetNotifications([]byte(tweetNotificationsFixture))
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, "1985000000000000009", tweets[0].ID)
	assert.Equal(t, "BNOWeird", tweets[0].AuthorHandle)
	assert.Equal(t, "bell tweet", tweets[0].Text)
}

// TestParseTweetNotifications_XWebFixture parses the x-web shape (snake_case
// entries, item nested under content.content, text under details) from a
// response captured live and reduced to two public news-account tweets with
// placeholder ids, cursors and text.
func TestParseTweetNotifications_XWebFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/tweet_notifications.json")
	require.NoError(t, err)

	tweets, _, err := parseTweetNotifications(body)
	require.NoError(t, err)
	require.Len(t, tweets, 2, "cursor entries must be skipped")

	assert.Equal(t, "1985000000000000001", tweets[0].ID)
	assert.Equal(t, "Reuters", tweets[0].AuthorHandle)
	assert.Equal(t, "Reuters", tweets[0].AuthorName)
	assert.Equal(t, "3100000000000000001", tweets[0].AuthorID)
	assert.Equal(t, "placeholder headline 1", tweets[0].Text)
	assert.Equal(t, int64(1793384687000), tweets[0].CreatedAt.UnixMilli())
	assert.Equal(t, "1985000000000000002", tweets[1].ID)
	assert.Equal(t, "zerohedge", tweets[1].AuthorHandle)
}

// The request must carry every Relay provided variable the operation declares;
// the server answers 422 when one is missing. It must also carry the count and
// no `features` parameter (the x-web persisted query declares none).
//
// Mutation: devicefollow.go tweetNotificationsVariables, delete the loop that
// copies tweetNotificationsProviders into v => this test goes RED
// ("provided variable missing").
func TestTweetNotificationsURL_CarriesProviders(t *testing.T) {
	raw, err := tweetNotificationsURL(20)
	require.NoError(t, err)
	u, err := url.Parse(raw)
	require.NoError(t, err)

	assert.Contains(t, u.Path, "/tweetNotificationsQuery")
	assert.Empty(t, u.Query().Get("features"))
	var vars map[string]any
	require.NoError(t, json.Unmarshal([]byte(u.Query().Get("variables")), &vars))
	assert.EqualValues(t, 20, vars["count"])

	want := []string{
		"__relay_internal__pv__xclientsfeaturesappviewerisloggedinrelayprovider",
		"__relay_internal__pv__xclientsfeaturestranslationgroktranslatedpostenabledrelayprovider",
		"__relay_internal__pv__xclientsfeaturestweetcomponentsgrokanalyzepostrelayprovider",
		"__relay_internal__pv__xclientsfeaturestranslationnoteautotranslationenabledrelayprovider",
		"__relay_internal__pv__liveeventpostcontextenabledrelayprovider",
		"__relay_internal__pv__sportspostcontextenabledrelayprovider",
	}
	for _, k := range want {
		v, ok := vars[k]
		require.True(t, ok, "provided variable missing: %s", k)
		assert.IsType(t, true, v, "provided variable %s must be a boolean", k)
	}
	assert.Len(t, vars, len(want)+1)
}

func TestDeviceNotifications(t *testing.T) {
	tw := []*Tweet{{ID: "1"}}
	gq := []*Tweet{{ID: "2"}}
	boom := errors.New("boom")
	notFound := &APIError{Status: http.StatusNotFound, Message: "gone"}

	src := func(ts []*Tweet, err error, calls *int) feedFetcher {
		return func(context.Context, int) ([]*Tweet, error) { *calls++; return ts, err }
	}
	tests := []struct {
		name        string
		rest, gql   func(*int) feedFetcher
		want        []*Tweet
		wantErr     bool
		restN, gqlN int
	}{
		{"rest has tweets", func(n *int) feedFetcher { return src(tw, nil, n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, tw, false, 1, 0},
		{"rest empty, graphql has tweets", func(n *int) feedFetcher { return src(nil, nil, n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, gq, false, 1, 1},
		{"rest empty, graphql fails: empty poll, no error", func(n *int) feedFetcher { return src(nil, nil, n) }, func(n *int) feedFetcher { return src(nil, boom, n) }, nil, false, 1, 1},
		{"rest 404, graphql ok", func(n *int) feedFetcher { return src(nil, notFound, n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, gq, false, 1, 1},
		{"rest 404, graphql fails: error", func(n *int) feedFetcher { return src(nil, notFound, n) }, func(n *int) feedFetcher { return src(nil, boom, n) }, nil, true, 1, 1},
		{"rest other error: no fallback", func(n *int) feedFetcher { return src(nil, boom, n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, nil, true, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var restN, gqlN int
			got, err := deviceNotifications(context.Background(), 20, tc.rest(&restN), tc.gql(&gqlN))
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.restN, restN, "rest calls")
			assert.Equal(t, tc.gqlN, gqlN, "graphql calls")
		})
	}
}
