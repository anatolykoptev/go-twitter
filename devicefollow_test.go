package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
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
	_, err := parseDeviceFollow([]byte(`{"errors":[{"code":88,"message":"Rate limit exceeded"}]}`))
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
	tweets, _, err := parseTweetNotifications([]byte(tweetNotificationsFixture))
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
	body, err := os.ReadFile("testdata/tweet_notifications.json")
	require.NoError(t, err)

	tweets, _, err := parseTweetNotifications(body)
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
	gone := func(status int) error { return newAPIError(status, []byte(`{"errors":[{"message":"gone"}]}`), nil) }

	src := func(ts []*Tweet, err error, calls *int) feedFetcher {
		return func(context.Context, int) ([]*Tweet, error) { *calls++; return ts, err }
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name        string
		ctx         context.Context
		rest, gql   func(*int) feedFetcher
		want        []*Tweet
		wantErr     bool
		restN, gqlN int
	}{
		{"rest has tweets", context.Background(), func(n *int) feedFetcher { return src(tw, nil, n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, tw, false, 1, 0},
		{"rest empty is a quiet poll: no fallback", context.Background(), func(n *int) feedFetcher { return src(nil, nil, n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, nil, false, 1, 0},
		{"rest 404, graphql ok", context.Background(), func(n *int) feedFetcher { return src(nil, gone(404), n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, gq, false, 1, 1},
		{"rest 410, graphql ok", context.Background(), func(n *int) feedFetcher { return src(nil, gone(410), n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, gq, false, 1, 1},
		{"rest 503 endpoint fault, graphql ok", context.Background(), func(n *int) feedFetcher { return src(nil, gone(503), n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, gq, false, 1, 1},
		{"rest 404, graphql fails: error", context.Background(), func(n *int) feedFetcher { return src(nil, gone(404), n) }, func(n *int) feedFetcher { return src(nil, boom, n) }, nil, true, 1, 1},
		{"rest other error: no fallback", context.Background(), func(n *int) feedFetcher { return src(nil, boom, n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, nil, true, 1, 0},
		{"cancelled ctx: error, no fallback", cancelled, func(n *int) feedFetcher { return src(nil, gone(404), n) }, func(n *int) feedFetcher { return src(gq, nil, n) }, nil, true, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var restN, gqlN int
			got, err := deviceNotifications(tc.ctx, 20, tc.rest(&restN), tc.gql(&gqlN))
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

	t.Run("cancelled ctx surfaces context.Canceled", func(t *testing.T) {
		var n int
		_, err := deviceNotifications(cancelled, 20, func(ctx context.Context, _ int) ([]*Tweet, error) { return nil, ctx.Err() }, src(gq, nil, &n))
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, n)
	})
	t.Run("both sources broken keeps both errors", func(t *testing.T) {
		var n int
		_, err := deviceNotifications(context.Background(), 20, src(nil, gone(404), &n), src(nil, boom, &n))
		require.ErrorIs(t, err, ErrEndpoint)
		require.ErrorIs(t, err, boom)
	})
}

// GetDeviceNotifications end to end against a fake x.com: it must serve the
// REST feed when REST answers (GraphQL untouched) and fall back to GraphQL only
// when REST is gone.
//
// Mutation: devicefollow.go GetDeviceNotifications, swap the two arguments of
// the deviceNotifications call (c.GetTweetNotifications, c.GetDeviceFollowTweets)
// => TestGetDeviceNotifications_EndToEnd/rest_serves_and_graphql_untouched goes
// RED (the GraphQL tweets come back and the GraphQL endpoint is hit).
func TestGetDeviceNotifications_EndToEnd(t *testing.T) {
	restBody, err := os.ReadFile("testdata/device_follow.json")
	require.NoError(t, err)
	gqlBody, err := os.ReadFile("testdata/tweet_notifications.json")
	require.NoError(t, err)

	for _, tc := range []struct {
		name       string
		restStatus int
		wantFirst  string
		wantRest   int32
		wantGQL    int32
	}{
		{"rest serves and graphql untouched", http.StatusOK, "1985000000000000003", 1, 0},
		{"rest gone falls back to graphql", http.StatusNotFound, "1985000000000000001", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var restHits, gqlHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "device_follow"):
					restHits.Add(1)
					w.WriteHeader(tc.restStatus)
					if tc.restStatus == http.StatusOK {
						_, _ = w.Write(restBody)
					} else {
						_, _ = w.Write([]byte(`{"errors":[{"message":"Gone"}]}`))
					}
				case strings.Contains(r.URL.Path, "tweetNotificationsQuery"):
					gqlHits.Add(1)
					_, _ = w.Write(gqlBody)
				default:
					w.WriteHeader(http.StatusTeapot)
				}
			}))
			t.Cleanup(srv.Close)
			origBase, origREST := twitterBase, deviceFollowURL
			twitterBase = srv.URL + "/i/api/graphql"
			deviceFollowURL = srv.URL + "/i/api/2/notifications/device_follow.json"
			t.Cleanup(func() { twitterBase, deviceFollowURL = origBase, origREST })

			c, _ := newPoolWaitClient(t, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			tweets, err := c.GetDeviceNotifications(ctx, 20)
			require.NoError(t, err)
			require.NotEmpty(t, tweets)
			assert.Equal(t, tc.wantFirst, tweets[0].ID)
			assert.Equal(t, tc.wantRest, restHits.Load(), "REST hits")
			assert.Equal(t, tc.wantGQL, gqlHits.Load(), "GraphQL hits")
		})
	}
}

// Both device-notification sources are per-account feeds: no guest fallback.
//
// Mutation: request.go requiresAuth, remove "DeviceFollow", "tweetNotificationsQuery"
// from the case list => TestRequiresAuth_DeviceNotificationOps goes RED
// (`op "DeviceFollow" must require auth`).
func TestRequiresAuth_DeviceNotificationOps(t *testing.T) {
	for _, op := range []string{deviceFollowEndpoint, tweetNotificationsOp} {
		assert.True(t, requiresAuth(op), "op %q must require auth (no guest fallback)", op)
	}
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
	tweets, err := parseDeviceFollow(body)
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
	body, err := os.ReadFile("testdata/tweet_notifications.json")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	ents := doc["data"].(map[string]any)["viewer_v2"].(map[string]any)["user_results"].(map[string]any)["result"].(map[string]any)["tweet_notification_timeline"].(map[string]any)["timeline"].(map[string]any)["instructions"].([]any)[0].(map[string]any)["entries"].([]any)
	ents[1].(map[string]any)["entry_id"] = "promoted-tweet-1985000000000000001"
	out, err := json.Marshal(doc)
	require.NoError(t, err)

	tweets, _, err := parseTweetNotifications(out)
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, "1985000000000000002", tweets[0].ID)
}
