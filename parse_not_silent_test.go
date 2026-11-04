package twitter

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// A tweet parse must not go silently empty: a result that cannot yield an ID
// and a creation time is dropped AND counted, and a response that carries no
// timeline at all is an error rather than an empty list. These tests drive
// the real GetUserTweets path over a fake transport.

const parseDroppedMetric = "twitter.client.parse.dropped"

// userTweetsEnvelope wraps raw timeline entries in the UserTweets response root.
func userTweetsEnvelope(entries ...string) string {
	joined := ""
	for i, e := range entries {
		if i > 0 {
			joined += ","
		}
		joined += e
	}
	return `{"data":{"user":{"result":{"timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[` + joined + `]}]}}}}}}`
}

// itemEntry wraps an itemContent object in a timeline entry.
func itemEntry(entryID, itemContent string) string {
	return fmt.Sprintf(`{"entryId":%q,"content":{"entryType":"TimelineTimelineItem","__typename":"TimelineTimelineItem","itemContent":%s}}`, entryID, itemContent)
}

// tweetItem wraps a tweet result object in a TimelineTweet item.
func tweetItem(result string) string {
	return `{"__typename":"TimelineTweet","tweet_results":{"result":` + result + `}}`
}

// parseDrops returns the recorded drop counter as reason -> total, summed over
// operations, and asserts the operation attribute is the one the call used.
func parseDrops(t *testing.T, f *metricsFixture, op string) map[string]int64 {
	t.Helper()
	m, ok := f.collect(t)[parseDroppedMetric]
	if !ok {
		return map[string]int64{}
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s is not an int64 counter", parseDroppedMetric)
	out := map[string]int64{}
	for _, dp := range sum.DataPoints {
		attrs := attrMap(dp.Attributes)
		assert.Equal(t, op, attrs["twitter.operation"])
		out[attrs["twitter.reason"]] += dp.Value
	}
	return out
}

// L1 — journey: UserTweets results carry no `legacy` text/created_at; the X web
// shape keeps them under details (full_text, created_at_ms). The poller must
// receive the tweets with ID, text and creation time, not blanks.
//
// Mutation: internal/parse/parsers.go parseTweetResult — drop the
// `r.Details.FullText` text fallback => RED (empty Text); drop the
// `r.Details.CreatedAtMs` read => RED (CreatedAt falls to the snowflake time).
func TestUserTweets_NoLegacy_FieldsComeFromDetails(t *testing.T) {
	created := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	fake := newPoolWaitFake(t)
	fake.body = userTweetsEnvelope(
		itemEntry("tweet-1985000000000000001", tweetItem(fmt.Sprintf(
			`{"__typename":"Tweet","rest_id":"1985000000000000001","core":{"user_results":{"result":{"rest_id":"999","core":{"screen_name":"alice","name":"Alice"}}}},"details":{"full_text":"hello from details","created_at_ms":%d},"legacy":{"conversation_id_str":"1985000000000000001","lang":"en"}}`,
			created.UnixMilli()))),
	)
	c, _ := newPoolWaitClient(t, 2)
	origBase := twitterBase
	twitterBase = fake.srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })

	tweets, err := c.GetUserTweets(context.Background(), "999", 20)
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, "1985000000000000001", tweets[0].ID)
	assert.Equal(t, "hello from details", tweets[0].Text)
	assert.True(t, created.Equal(tweets[0].CreatedAt), "CreatedAt = %v, want details.created_at_ms %v", tweets[0].CreatedAt, created)
	assert.Equal(t, "alice", tweets[0].AuthorHandle)
}

// L2 — a result with no usable ID, and one with an ID but no creation time from
// any source, are dropped and counted with their own reason.
//
// Mutation: internal/parse/parsers.go extractTweets — skip the
// `observe(reason)` call in the "tweet parse error" arm => RED (counter empty).
func TestUserTweets_UnusableResult_DroppedAndCounted(t *testing.T) {
	cases := []struct {
		name, result, reason string
	}{
		{"no id", `{"__typename":"Tweet","legacy":{"full_text":"orphan","created_at":"Mon Jan 02 15:04:05 +0000 2006"}}`, "no_id"},
		// A non-numeric id has no snowflake time either, so nothing dates it.
		{"no created_at", `{"__typename":"Tweet","rest_id":"not-a-snowflake","legacy":{"full_text":"undated"}}`, "no_created_at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f *metricsFixture
			f = newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
				good := tweetItem(`{"__typename":"Tweet","rest_id":"` + snowflakeAt(f.now.Add(-time.Minute)) + `","legacy":{"full_text":"ok","user_id_str":"999","created_at":"Mon Jan 02 15:04:05 +0000 2006"}}`)
				_, _ = w.Write([]byte(userTweetsEnvelope(
					itemEntry("tweet-bad", tweetItem(tc.result)),
					itemEntry("tweet-good", good),
				)))
			}, "l2_acc")

			tweets, err := f.c.GetUserTweets(context.Background(), "999", 20)
			require.NoError(t, err)
			require.Len(t, tweets, 1, "the unusable result is dropped, the good one kept")
			assert.Equal(t, map[string]int64{tc.reason: 1}, parseDrops(t, f, "UserTweets"))
		})
	}
}

// L3 — the other silent skips are counted too: an entry of a type the parser
// does not handle, and a tombstone (deleted/unavailable tweet).
//
// Mutation: internal/parse/parsers.go extractTweets — replace the
// unhandled-type `observe(DropUnhandledType)` with the old Debug-only log =>
// RED on "unhandled type"; make the tombstone case fall to the Debug path =>
// RED on "tombstone".
func TestUserTweets_UnhandledTypeAndTombstone_Counted(t *testing.T) {
	cases := []struct {
		name, item, reason string
	}{
		{"unhandled type", `{"__typename":"TimelineSomethingNew"}`, "unhandled_type"},
		{"tombstone", tweetItem(`{"__typename":"TweetTombstone","tombstone":{"__typename":"TextTombstone"}}`), "tombstone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f *metricsFixture
			f = newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
				good := tweetItem(`{"__typename":"Tweet","rest_id":"` + snowflakeAt(f.now.Add(-time.Minute)) + `","legacy":{"full_text":"ok","user_id_str":"999"}}`)
				_, _ = w.Write([]byte(userTweetsEnvelope(
					itemEntry("entry-x", tc.item),
					itemEntry("tweet-good", good),
				)))
			}, "l3_acc")

			tweets, err := f.c.GetUserTweets(context.Background(), "999", 20)
			require.NoError(t, err)
			require.Len(t, tweets, 1)
			assert.Equal(t, map[string]int64{tc.reason: 1}, parseDrops(t, f, "UserTweets"))
		})
	}
}

// A prompt, message prompt, user or cursor entry is expected noise, not a drop.
func TestUserTweets_ExpectedNonTweetItems_NotCounted(t *testing.T) {
	var f *metricsFixture
	f = newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		good := tweetItem(`{"__typename":"Tweet","rest_id":"` + snowflakeAt(f.now.Add(-time.Minute)) + `","legacy":{"full_text":"ok","user_id_str":"999"}}`)
		_, _ = w.Write([]byte(userTweetsEnvelope(
			itemEntry("p", `{"__typename":"TimelinePrompt"}`),
			itemEntry("u", `{"__typename":"TimelineUser"}`),
			itemEntry("tweet-good", good),
		)))
	}, "l3b_acc")

	tweets, err := f.c.GetUserTweets(context.Background(), "999", 20)
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Empty(t, parseDrops(t, f, "UserTweets"))
}

// E1 — journey: UserTweets answered with a data object that carries no
// timeline. That is a typed, terminal error — not an empty "no new tweets" —
// and on a 2-account pool exactly one request is made, nobody is charged.
//
// Mutation: internal/parse/parsers.go TweetTimeline — return
// `extractTweetsFromTimeline(tl, authorID)` unconditionally again (skip the
// missing-timeline error) => RED (nil error, empty list).
func TestUserTweets_NoTimelineObject_TerminalUnexpectedResponse(t *testing.T) {
	bodies := map[string]string{
		"empty user":         `{"data":{"user":{}}}`,
		"empty result":       `{"data":{"user":{"result":{}}}}`,
		"result no timeline": `{"data":{"user":{"result":{"__typename":"User","rest_id":"999"}}}}`,
		"empty data":         `{"data":{}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = body
			c, accs := newPoolWaitClient(t, 2)
			origBase := twitterBase
			twitterBase = fake.srv.URL + "/i/api/graphql"
			t.Cleanup(func() { twitterBase = origBase })

			tweets, err := c.GetUserTweets(context.Background(), "999", 20)
			require.Error(t, err)
			assert.Nil(t, tweets)
			assert.ErrorIs(t, err, ErrUnexpectedResponse)
			assert.NotErrorIs(t, err, ErrNotFound)
			assert.EqualValues(t, 1, fake.hits.Load(), "terminal: no rotation to the second account")
			for i, a := range accs {
				_, failed, _ := a.Stats()
				assert.Zero(t, failed, "account %d must not be charged", i)
				assert.True(t, a.IsActive(), "account %d must stay active", i)
			}
		})
	}
}

// E2 — a timeline object that exists with zero tweet entries is a legitimately
// empty profile: success, empty list.
//
// Mutation: internal/parse/parsers.go TweetTimeline — decide "missing" by
// `len(tl.Instructions) == 0` instead of by the timeline object's presence =>
// RED on the `empty instructions` and `v2` subtests.
func TestUserTweets_ZeroEntryTimeline_EmptySuccess(t *testing.T) {
	bodies := map[string]string{
		"empty entries":      `{"data":{"user":{"result":{"timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[]}]}}}}}}`,
		"empty instructions": `{"data":{"user":{"result":{"timeline":{"timeline":{"instructions":[]}}}}}}`,
		"v2 empty":           `{"data":{"user":{"result":{"timeline_v2":{"timeline":{"instructions":[]}}}}}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.body = body
			c, _ := newPoolWaitClient(t, 2)
			origBase := twitterBase
			twitterBase = fake.srv.URL + "/i/api/graphql"
			t.Cleanup(func() { twitterBase = origBase })

			tweets, err := c.GetUserTweets(context.Background(), "999", 20)
			require.NoError(t, err)
			assert.Empty(t, tweets)
			assert.EqualValues(t, 1, fake.hits.Load())
		})
	}
}

// E3 — a user X reports as unavailable (suspended / restricted) keeps mapping
// to ErrNotFound, not to the missing-timeline error.
//
// Mutation: internal/parse/parsers.go TweetTimeline — delete the
// `UserUnavailable` branch so the result falls to the missing-timeline error
// => RED (ErrUnexpectedResponse instead of ErrNotFound).
func TestUserTweets_UserUnavailable_IsNotFound(t *testing.T) {
	fake := newPoolWaitFake(t)
	fake.body = `{"data":{"user":{"result":{"__typename":"UserUnavailable","reason":"Suspended"}}}}`
	c, _ := newPoolWaitClient(t, 2)
	origBase := twitterBase
	twitterBase = fake.srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })

	tweets, err := c.GetUserTweets(context.Background(), "999", 20)
	require.Error(t, err)
	assert.Nil(t, tweets)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.NotErrorIs(t, err, ErrUnexpectedResponse)
	assert.EqualValues(t, 1, fake.hits.Load())
}

// D1 — the device-notification feed (the realtime hot path): a REST entry with
// no id_str is dropped AND counted, the good one is kept.
//
// Mutation: internal/parse/devicefollow.go DeviceFollow — delete
// `o.drop(DropNoID)` in the `lt.IDStr == ""` arm => RED.
func TestDeviceFollow_EntryWithoutID_DroppedAndCounted(t *testing.T) {
	var f *metricsFixture
	f = newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		id := snowflakeAt(f.now.Add(-time.Minute))
		_, _ = w.Write([]byte(`{"globalObjects":{"tweets":{"orphan":{"full_text":"no id","user_id_str":"9"},"` + id + `":{"id_str":"` + id + `","full_text":"ok","user_id_str":"9"}},"users":{}}}`))
	}, "d1_acc")
	origREST := deviceFollowURL
	deviceFollowURL = strings.TrimSuffix(twitterBase, "/i/api/graphql") + "/i/api/2/notifications/device_follow.json"
	t.Cleanup(func() { deviceFollowURL = origREST })

	tweets, err := f.c.GetDeviceFollowTweets(context.Background(), 20)
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, map[string]int64{"no_id": 1}, parseDrops(t, f, "DeviceFollow"))
}

// D2 — the x-web notifications timeline: a tweet result with no rest_id is
// dropped AND counted.
//
// Mutation: internal/parse/devicefollow.go TweetNotifications — delete
// `o.drop(DropNoID)` before "skip notification tweet without rest_id" => RED.
func TestTweetNotifications_ResultWithoutRestID_DroppedAndCounted(t *testing.T) {
	var f *metricsFixture
	f = newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		id := snowflakeAt(f.now.Add(-time.Minute))
		entry := func(eid, result string) string {
			return `{"entry_id":"` + eid + `","content":{"content":{"__typename":"TimelineTweet","tweet_results":{"result":` + result + `}}}}`
		}
		_, _ = w.Write([]byte(`{"data":{"viewer_v2":{"user_results":{"result":{"tweet_notification_timeline":{"timeline":{"instructions":[{"entries":[` +
			entry("tweet-bad", `{"__typename":"Tweet"}`) + `,` +
			entry("tweet-good", `{"__typename":"Tweet","rest_id":"`+id+`","details":{"full_text":"ok","created_at_ms":1790000000000}}`) +
			`]}]}}}}}}}`))
	}, "d2_acc")

	tweets, err := f.c.GetTweetNotifications(context.Background(), 20)
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, map[string]int64{"no_id": 1}, parseDrops(t, f, "tweetNotificationsQuery"))
}
