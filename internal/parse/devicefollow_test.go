package parse

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseDeviceFollow reads the sanitized fixture captured during the smoke
// run and asserts the parsed (tweet_id, author handle, text) triples. Fixture
// contents: two target-author tweets (one plain original, one RT) and one
// non-target tweet.
func TestParseDeviceFollow(t *testing.T) {
	body, err := os.ReadFile("../../testdata/device_follow.json")
	require.NoError(t, err)

	tweets, err := DeviceFollow(body)
	require.NoError(t, err)
	require.Len(t, tweets, 3, "fixture carries exactly three tweets")

	got := make(map[string]struct{ handle, text string }, len(tweets))
	for _, tw := range tweets {
		got[tw.ID] = struct{ handle, text string }{tw.AuthorHandle, tw.Text}
		assert.NotEmpty(t, tw.AuthorID, "tweet %s: author id", tw.ID)
	}

	want := map[string]struct{ handle, text string }{
		"1985000000000000001": {"news_a", "*FED HOLDS RATES STEADY"},
		"1985000000000000002": {"news_b", "RT @news_c: markets up"},
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
	_, err := DeviceFollow([]byte(`{"errors":[{"code":88,"message":"Rate limit exceeded"}]}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error 88")
}

// tweetNotificationsFixture is one TimelineAddEntries instruction inside the
// tweetNotificationsQuery root — the same URT shape every other timeline test
// exercises, only the wrapper path differs.
const tweetNotificationsFixture = `{"data": {"viewer_v2": {"user_results": {"result": {"tweet_notification_timeline": {"timeline": {"instructions": [{"type": "TimelineAddEntries", "entries": [{"entryId": "tweet-1985000000000000009", "content": {"entryType": "TimelineTimelineItem", "itemContent": {"__typename": "TimelineTweet", "tweet_results": {"result": {"__typename": "Tweet", "rest_id": "1985000000000000009", "core": {"user_results": {"result": {"__typename": "User", "rest_id": "42", "core": {"screen_name": "news_d", "name": "BNO"}}}}, "legacy": {"full_text": "bell tweet", "user_id_str": "42"}}}}}}]}]}}}}}}}`

// TestParseTweetNotifications proves the GraphQL fallback root
// (viewer_v2.user_results.result.tweet_notification_timeline.timeline) yields
// the same (id, handle) pairs a URT timeline carries anywhere else.
func TestParseTweetNotifications(t *testing.T) {
	tweets, _, err := TweetNotifications([]byte(tweetNotificationsFixture))
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, "1985000000000000009", tweets[0].ID)
	assert.Equal(t, "news_d", tweets[0].AuthorHandle)
	assert.Equal(t, "bell tweet", tweets[0].Text)
}

// TestParseTweetNotifications_XWebFixture parses the x-web shape (snake_case
// entries, item nested under content.content, text under details) from a
// response captured live and reduced to two public news-account tweets with
// placeholder ids, cursors and text.
func TestParseTweetNotifications_XWebFixture(t *testing.T) {
	body, err := os.ReadFile("../../testdata/tweet_notifications.json")
	require.NoError(t, err)

	tweets, _, err := TweetNotifications(body)
	require.NoError(t, err)
	require.Len(t, tweets, 2, "cursor entries must be skipped")

	assert.Equal(t, "1985000000000000001", tweets[0].ID)
	assert.Equal(t, "news_a", tweets[0].AuthorHandle)
	assert.Equal(t, "news_a", tweets[0].AuthorName)
	assert.Equal(t, "3100000000000000001", tweets[0].AuthorID)
	assert.Equal(t, "placeholder headline 1", tweets[0].Text)
	assert.Equal(t, int64(1793384687000), tweets[0].CreatedAt.UnixMilli())
	assert.Equal(t, "1985000000000000002", tweets[1].ID)
	assert.Equal(t, "news_b", tweets[1].AuthorHandle)
}

// device_follow.json lists in globalObjects.tweets also the original behind a
// retweet, authored by an account the viewer never belled. Only timeline-listed,
// non-promoted tweets may come back, newest first, with CreatedAt set.
func TestParseDeviceFollow_ListedOnlyNewestFirstWithTimes(t *testing.T) {
	body := []byte(`{
	  "globalObjects": {
	    "users": {"11": {"screen_name": "belled_a", "name": "A"}, "22": {"screen_name": "stranger", "name": "S"}},
	    "tweets": {
	      "1985000000000000001": {"id_str": "1985000000000000001", "full_text": "old", "user_id_str": "11", "created_at": "Thu Oct 30 10:18:00 +0000 2026"},
	      "1985000000000000005": {"id_str": "1985000000000000005", "full_text": "new", "user_id_str": "11"},
	      "1985000000000000009": {"id_str": "1985000000000000009", "full_text": "rt original", "user_id_str": "22"},
	      "1985000000000000007": {"id_str": "1985000000000000007", "full_text": "ad", "user_id_str": "22"}
	    }
	  },
	  "timeline": {"instructions": [{"addEntries": {"entries": [
	    {"entryId": "tweet-1985000000000000001", "content": {"item": {"content": {"tweet": {"id": "1985000000000000001"}}}}},
	    {"entryId": "tweet-1985000000000000005", "content": {"item": {"content": {"tweet": {"id": "1985000000000000005"}}}}},
	    {"entryId": "promoted-tweet-1985000000000000007", "content": {"item": {"content": {"tweet": {"id": "1985000000000000007"}}}}}
	  ]}}]}
	}`)
	tweets, err := DeviceFollow(body)
	require.NoError(t, err)
	require.Len(t, tweets, 2, "the retweet original and the promoted entry must not leak")
	assert.Equal(t, "1985000000000000005", tweets[0].ID, "newest first")
	assert.Equal(t, "1985000000000000001", tweets[1].ID)
	assert.Equal(t, "belled_a", tweets[0].AuthorHandle)
	assert.Equal(t, time.Date(2026, 10, 30, 10, 18, 0, 0, time.UTC), tweets[1].CreatedAt, "created_at wins")
	snow, err := SnowflakeTime("1985000000000000005")
	require.NoError(t, err)
	assert.Equal(t, snow, tweets[0].CreatedAt, "snowflake time when created_at is absent")
}

func TestParseTweetNotifications_SkipsPromoted(t *testing.T) {
	body, err := os.ReadFile("../../testdata/tweet_notifications.json")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	ents := doc["data"].(map[string]any)["viewer_v2"].(map[string]any)["user_results"].(map[string]any)["result"].(map[string]any)["tweet_notification_timeline"].(map[string]any)["timeline"].(map[string]any)["instructions"].([]any)[0].(map[string]any)["entries"].([]any)
	ents[1].(map[string]any)["entry_id"] = "promoted-tweet-1985000000000000001"
	out, err := json.Marshal(doc)
	require.NoError(t, err)

	tweets, _, err := TweetNotifications(out)
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, "1985000000000000002", tweets[0].ID)
}

func entryJSON(id string, extra string) string {
	return `{"entryId": "tweet-` + id + `"` + extra + `, "content": {"item": {"content": {"tweet": {"id": "` + id + `"}}}}}`
}

const dfUsers = `"users": {"11": {"screen_name": "belled_a"}, "22": {"screen_name": "stranger"}}`

// A present timeline is authoritative even when it lists nothing usable: a
// promoted-only timeline must yield no tweets (neither the ad nor the stranger's
// retweet original that rides along in globalObjects.tweets).
//
// Mutation: internal/parse/devicefollow.go DeviceFollow, replace `if hasTimeline {` of the
// listed-filter with `if len(listed) > 0 {` => this test goes RED
// (`Should be empty, but was [...]`).
func TestParseDeviceFollow_PromotedOnlyTimelineYieldsNothing(t *testing.T) {
	body := []byte(`{"globalObjects": {` + dfUsers + `, "tweets": {
	  "1985000000000000007": {"id_str": "1985000000000000007", "full_text": "ad", "user_id_str": "22"},
	  "1985000000000000009": {"id_str": "1985000000000000009", "full_text": "rt original", "user_id_str": "22"}}},
	  "timeline": {"instructions": [{"addEntries": {"entries": [
	    {"entryId": "promoted-tweet-1985000000000000007", "content": {"item": {"content": {"tweet": {"id": "1985000000000000007"}}}}}]}}]}}`)
	tweets, err := DeviceFollow(body)
	require.NoError(t, err)
	assert.Empty(t, tweets)

	// An empty timeline object is still a timeline.
	tweets, err = DeviceFollow([]byte(`{"globalObjects": {"tweets": {"1": {"id_str": "1", "user_id_str": "22"}}}, "timeline": {"instructions": []}}`))
	require.NoError(t, err)
	assert.Empty(t, tweets)
}

// Tweets listed through timelineModule items and replaceEntry are feed items too.
func TestParseDeviceFollow_ModuleItemsAndReplaceEntryKept(t *testing.T) {
	body := []byte(`{"globalObjects": {` + dfUsers + `, "tweets": {
	  "1985000000000000001": {"id_str": "1985000000000000001", "user_id_str": "11"},
	  "1985000000000000002": {"id_str": "1985000000000000002", "user_id_str": "11"},
	  "1985000000000000003": {"id_str": "1985000000000000003", "user_id_str": "11"},
	  "1985000000000000009": {"id_str": "1985000000000000009", "user_id_str": "22"}}},
	  "timeline": {"instructions": [
	    {"addEntries": {"entries": [
	      ` + entryJSON("1985000000000000001", "") + `,
	      {"entryId": "module-1", "content": {"timelineModule": {"items": [
	        {"entryId": "m-1", "item": {"item": {"content": {"tweet": {"id": "1985000000000000002"}}}}}]}}}]}},
	    {"replaceEntry": {"entryIdToReplace": "x", "entry": ` + entryJSON("1985000000000000003", "") + `}}]}}`)
	tweets, err := DeviceFollow(body)
	require.NoError(t, err)
	var ids []string
	for _, tw := range tweets {
		ids = append(ids, tw.ID)
	}
	assert.Equal(t, []string{"1985000000000000003", "1985000000000000002", "1985000000000000001"}, ids)
}

// Without any timeline there is nothing to filter by: keep every tweet, count it.
func TestParseDeviceFollow_NoTimelineKeepsAllAndCounts(t *testing.T) {
	before := deviceFollowNoTimeline.Load()
	tweets, err := DeviceFollow([]byte(`{"globalObjects": {"tweets": {"1985000000000000001": {"id_str": "1985000000000000001", "user_id_str": "11"}}}}`))
	require.NoError(t, err)
	assert.Len(t, tweets, 1)
	assert.Equal(t, before+1, deviceFollowNoTimeline.Load())
}

// Entries carrying promotedMetadata are ads whatever their entryId says.
func TestPromotedMetadataSkipped(t *testing.T) {
	body := []byte(`{"globalObjects": {"tweets": {
	  "1985000000000000001": {"id_str": "1985000000000000001", "user_id_str": "11"},
	  "1985000000000000002": {"id_str": "1985000000000000002", "user_id_str": "11"}}},
	  "timeline": {"instructions": [{"addEntries": {"entries": [
	    ` + entryJSON("1985000000000000001", "") + `,
	    {"entryId": "tweet-1985000000000000002", "content": {"item": {"content": {"tweet": {"id": "1985000000000000002", "promotedMetadata": {"advertiser_id": "1"}}}}}}]}}]}}`)
	tweets, err := DeviceFollow(body)
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, "1985000000000000001", tweets[0].ID)

	fx, err := os.ReadFile("../../testdata/tweet_notifications.json")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(fx, &doc))
	ents := doc["data"].(map[string]any)["viewer_v2"].(map[string]any)["user_results"].(map[string]any)["result"].(map[string]any)["tweet_notification_timeline"].(map[string]any)["timeline"].(map[string]any)["instructions"].([]any)[0].(map[string]any)["entries"].([]any)
	ents[1].(map[string]any)["content"].(map[string]any)["promotedMetadata"] = map[string]any{"advertiser_id": "1"}
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	xt, _, err := TweetNotifications(out)
	require.NoError(t, err)
	require.Len(t, xt, 1)
	assert.Equal(t, "1985000000000000002", xt[0].ID)
}

// With a timeline present, a tweet the feed merely references (the original
// behind a retweet) is not a feed entry: when it lacks id_str nothing was lost,
// so no drop is counted. A LISTED entry without id_str is a real drop, and
// without any timeline every invalid object counts.
//
// Mutation: devicefollow.go DeviceFollow — move the `if hasTimeline { ... }`
// listed-filter block back below the `lt.IDStr == ""` check => RED: the
// unlisted referenced object is counted as no_id.
func TestParseDeviceFollow_ReferencedTweetWithoutIDIsNotADrop(t *testing.T) {
	count := func(body string) map[string]int {
		drops := map[string]int{}
		_, err := DeviceFollow([]byte(body), WithObserver(Observer{Drop: func(r string) { drops[r]++ }}))
		require.NoError(t, err)
		return drops
	}
	const tl = `"timeline":{"instructions":[{"addEntries":{"entries":[{"entryId":"tweet-1985000000000000001","content":{"item":{"content":{"tweet":{"id":"1985000000000000001"}}}}}]}}]}`
	const listed = `"1985000000000000001":{"id_str":"1985000000000000001","user_id_str":"11"}`

	// The retweet original sits under its own key with an empty id_str.
	assert.Empty(t, count(`{"globalObjects":{"tweets":{`+listed+`,"1985000000000000009":{"id_str":""}}},`+tl+`}`),
		"an unlisted referenced object is not a feed entry")

	// A listed entry that lost its id_str is a real drop.
	const tlKey = `"timeline":{"instructions":[{"addEntries":{"entries":[{"entryId":"tweet-1","content":{"item":{"content":{"tweet":{"id":"1985000000000000009"}}}}}]}}]}`
	assert.Equal(t, map[string]int{DropNoID: 1},
		count(`{"globalObjects":{"tweets":{"1985000000000000009":{"id_str":""}}},`+tlKey+`}`))

	// No timeline: nothing to filter by, every invalid object counts.
	assert.Equal(t, map[string]int{DropNoID: 1},
		count(`{"globalObjects":{"tweets":{`+listed+`,"1985000000000000009":{"id_str":""}}}}`))
}
