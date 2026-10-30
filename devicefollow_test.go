package twitter

import (
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
