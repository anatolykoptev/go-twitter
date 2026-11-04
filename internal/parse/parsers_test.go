package parse

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anatolykoptev/go-twitter/model"
)

func TestParseUserByScreenName(t *testing.T) {
	body := `{
		"data": {
			"user": {
				"result": {
					"__typename": "User",
					"id": "UXNlcjoxMjM0NQ==",
					"rest_id": "12345",
					"legacy": {
						"name": "Test User",
						"screen_name": "testuser",
						"followers_count": 100,
						"friends_count": 50,
						"statuses_count": 200,
						"listed_count": 5,
						"created_at": "Mon Jan 02 15:04:05 +0000 2020",
						"verified": false,
						"description": "Hello world",
						"profile_image_url_https": "https://pbs.twimg.com/profile_images/123/photo.jpg"
					},
					"is_blue_verified": true
				}
			}
		}
	}`

	user, err := UserByScreenName([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != "12345" {
		t.Fatalf("expected ID 12345, got %s", user.ID)
	}
	if user.Handle != "testuser" {
		t.Fatalf("expected handle testuser, got %s", user.Handle)
	}
	if user.DisplayName != "Test User" {
		t.Fatalf("expected name Test User, got %s", user.DisplayName)
	}
	if user.Followers != 100 {
		t.Fatalf("expected 100 followers, got %d", user.Followers)
	}
	if !user.IsVerified {
		t.Fatal("expected verified (blue)")
	}
	if !user.HasAvatar {
		t.Fatal("expected has avatar")
	}
	if !user.HasBio {
		t.Fatal("expected has bio")
	}
}

func TestParseUserByScreenName_Unavailable(t *testing.T) {
	body := `{
		"data": {
			"user": {
				"result": {
					"__typename": "UserUnavailable",
					"rest_id": ""
				}
			}
		}
	}`

	_, err := UserByScreenName([]byte(body))
	require.ErrorIs(t, err, model.ErrNotFound, "unavailable user is a deterministic miss")
}

// TestParseUserByScreenName_ErrorsWithData: a partial response carrying both
// errors[] and a typed UserUnavailable result must resolve via the data — the
// typed result is a deterministic ErrNotFound, not a retryable generic error.
func TestParseUserByScreenName_ErrorsWithData(t *testing.T) {
	body := `{
		"data": {"user": {"result": {"__typename": "UserUnavailable", "rest_id": ""}}},
		"errors": [{"message": "UserUnavailable"}]
	}`
	_, err := UserByScreenName([]byte(body))
	require.ErrorIs(t, err, model.ErrNotFound)
}

func TestParseUserByScreenName_ErrorsOnly(t *testing.T) {
	body := `{"data": {}, "errors": [{"message": "bad query"}]}`
	_, err := UserByScreenName([]byte(body))
	require.Error(t, err)
	assert.ErrorContains(t, err, "bad query")
	assert.NotErrorIs(t, err, model.ErrNotFound)
}

func TestParseSearchUsersTimeline(t *testing.T) {
	body := []byte(`{
		"data": {"search_by_raw_query": {"search_timeline": {"timeline": {"instructions": [{
			"type": "TimelineAddEntries",
			"entries": [
				{"entryId": "user-12345", "sortIndex": "0", "content": {"entryType": "TimelineTimelineItem", "__typename": "TimelineTimelineItem", "itemContent": {"__typename": "TimelineUser", "user_results": {"result": {"__typename": "User", "rest_id": "12345", "core": {"name": "Jane Dev", "screen_name": "janedev", "created_at": "Wed Jan 15 12:00:00 +0000 2020"}, "is_blue_verified": true, "profile_bio": {"description": "Building things"}, "legacy": {"followers_count": 987, "friends_count": 100, "statuses_count": 500}}}, "user_display_type": "UserDetailed"}}},
				{"entryId": "cursor-bottom", "content": {"entryType": "TimelineTimelineCursor", "cursorType": "Bottom", "value": "CUR1"}}
			]}]}}}}
	}`)
	users, err := SearchUsersTimeline(body)
	require.NoError(t, err)
	require.Len(t, users, 1)
	u := users[0]
	assert.Equal(t, "12345", u.ID)
	assert.Equal(t, "janedev", u.Handle)
	assert.Equal(t, "Jane Dev", u.DisplayName)
	assert.Equal(t, 987, u.Followers)
	assert.True(t, u.IsVerified)
	assert.Equal(t, "Building things", u.Bio)
}

// TestParseSearchUsersTimeline_SkipsTweets guards the TimelineUser filter:
// a People-tab page can still carry non-user entries, and dropping the
// typename check would surface tweets as empty users.
func TestParseSearchUsersTimeline_SkipsTweets(t *testing.T) {
	body := []byte(`{
		"data": {"search_by_raw_query": {"search_timeline": {"timeline": {"instructions": [{
			"type": "TimelineAddEntries",
			"entries": [
				{"entryId": "tweet-9", "content": {"entryType": "TimelineTimelineItem", "itemContent": {"__typename": "TimelineTweet", "tweet_results": {"result": {"__typename": "Tweet", "rest_id": "9", "core": {"user_results": {"result": {"__typename": "User", "rest_id": "7", "core": {"screen_name": "noise"}}}}, "legacy": {"full_text": "not a user"}}}}}},
				{"entryId": "user-8", "content": {"entryType": "TimelineTimelineItem", "itemContent": {"__typename": "TimelineUser", "user_results": {"result": {"__typename": "User", "rest_id": "8", "core": {"name": "Ann", "screen_name": "ann"}, "legacy": {"followers_count": 5}}}}}}
			]}]}}}}
	}`)
	users, err := SearchUsersTimeline(body)
	require.NoError(t, err)
	require.Len(t, users, 1)
	assert.Equal(t, "ann", users[0].Handle)
}

// TestSplitConversation covers the focal/replies split used by
// GetTweetConversation: focal is the ID match (else first, defensive —
// production callers go through parseTweetDetail's ErrNotFound gate),
// replies keep entry order minus every copy of the focal ID.
func TestSplitConversation(t *testing.T) {
	focal := &model.Tweet{ID: "10"}
	reply1 := &model.Tweet{ID: "11"}
	reply2 := &model.Tweet{ID: "12"}

	main, replies := SplitConversation([]*model.Tweet{focal, reply1, reply2}, "10")
	assert.Equal(t, "10", main.ID)
	require.Len(t, replies, 2)
	assert.Equal(t, "11", replies[0].ID)

	// The focal reappears as the anchor item of its own conversationthread-*
	// module — every copy must stay out of replies.
	dup := &model.Tweet{ID: "10"}
	main, replies = SplitConversation([]*model.Tweet{focal, dup, reply1}, "10")
	assert.Equal(t, "10", main.ID)
	require.Len(t, replies, 1)
	assert.Equal(t, "11", replies[0].ID)

	// Defensive fallback only: unreachable through getTweetDetail, which
	// returns ErrNotFound when the focal is absent.
	main, replies = SplitConversation([]*model.Tweet{reply1, reply2}, "10")
	assert.Equal(t, "11", main.ID)
	require.Len(t, replies, 1)
	assert.Equal(t, "12", replies[0].ID)

	main, _ = SplitConversation([]*model.Tweet{focal}, "10")
	assert.Equal(t, "10", main.ID)

	main, replies = SplitConversation(nil, "10")
	assert.Nil(t, main)
	assert.Empty(t, replies)
}

// TestParseTweetDetail_ConversationModules exercises the live TweetDetail
// shape: focal via TimelinePinEntry, replies nested inside
// conversationthread-* module items, a promoted item excluded.
func TestParseTweetDetail_ConversationModules(t *testing.T) {
	tweetIC := func(id, user, text string) map[string]any {
		return map[string]any{
			"__typename": "TimelineTweet",
			"tweet_results": map[string]any{"result": map[string]any{
				"__typename": "Tweet",
				"rest_id":    id,
				"core": map[string]any{"user_results": map[string]any{"result": map[string]any{
					"__typename": "User",
					"rest_id":    "u" + id,
					"core":       map[string]any{"screen_name": user, "name": user},
				}}},
				"legacy": map[string]any{
					"full_text":      text,
					"created_at":     "Wed Jan 15 12:00:00 +0000 2020",
					"favorite_count": 3, "retweet_count": 1, "reply_count": 0,
					"user_id_str": "u" + id,
				},
			}},
		}
	}
	promoted := tweetIC("99", "adco", "AD")
	promoted["promotedMetadata"] = map[string]any{"adId": "x"}
	promotedInModule := tweetIC("98", "adco2", "MODULE AD")
	promotedInModule["promotedMetadata"] = map[string]any{"adId": "y"}
	body, err := json.Marshal(map[string]any{
		"data": map[string]any{"threaded_conversation_with_injections_v2": map[string]any{"instructions": []any{
			map[string]any{"type": "TimelinePinEntry", "entry": map[string]any{
				"entryId": "tweet-100",
				"content": map[string]any{"entryType": "TimelineTimelineItem", "itemContent": tweetIC("100", "alice", "focal tweet")},
			}},
			map[string]any{"type": "TimelineAddEntries", "entries": []any{
				map[string]any{"entryId": "conversationthread-100", "content": map[string]any{
					"entryType": "TimelineTimelineModule",
					"items": []any{
						// Live modules anchor on the focal tweet itself —
						// the focal reappears as items[0] and must not leak
						// into its own replies.
						map[string]any{"entryId": "conversationthread-100-tweet-100", "item": map[string]any{"itemContent": tweetIC("100", "alice", "focal tweet")}},
						map[string]any{"entryId": "conversationthread-100-tweet-101", "item": map[string]any{"itemContent": tweetIC("101", "bob", "reply one")}},
						// Real promoted module items carry a hash-suffixed
						// entryId with no "promoted" substring — only the
						// promotedMetadata probe catches them.
						map[string]any{"entryId": "conversationthread-100-9f8e7d", "item": map[string]any{"itemContent": promotedInModule}},
						map[string]any{"entryId": "conversationthread-100-tweet-102", "item": map[string]any{"itemContent": tweetIC("102", "carol", "reply two")}},
					},
				}},
				map[string]any{"entryId": "promoted-99", "content": map[string]any{"entryType": "TimelineTimelineItem", "itemContent": promoted}},
				map[string]any{"entryId": "cursor-bottom", "content": map[string]any{"entryType": "TimelineTimelineCursor", "cursorType": "Bottom", "value": "CUR"}},
			}},
		}}},
	})
	require.NoError(t, err)

	tweets, err := TweetDetail(body, "100")
	require.NoError(t, err)
	require.Len(t, tweets, 4, "pin + focal module anchor + 2 replies; both promoted items excluded")
	focal, replies := SplitConversation(tweets, "100")
	require.NotNil(t, focal)
	assert.Equal(t, "100", focal.ID)
	assert.Equal(t, "focal tweet", focal.Text)
	require.Len(t, replies, 2)
	assert.Equal(t, "101", replies[0].ID)
	assert.Equal(t, "102", replies[1].ID)
}

// TestParseTweetDetail_VisibilityWrappedFocal: X nests sensitive-tagged
// tweets as {__typename: TweetWithVisibilityResults, tweet: {…Tweet…}} —
// without the unwrap the focal decodes with empty rest_id and reports
// ErrNotFound although the tweet is present and readable.
func TestParseTweetDetail_VisibilityWrappedFocal(t *testing.T) {
	wrapped := map[string]any{
		"__typename": "TimelineTweet",
		"tweet_results": map[string]any{"result": map[string]any{
			"__typename":           "TweetWithVisibilityResults",
			"limitedActionResults": map[string]any{"limited_actions": []any{}},
			"tweet": map[string]any{
				"__typename": "Tweet",
				"rest_id":    "100",
				"core": map[string]any{"user_results": map[string]any{"result": map[string]any{
					"__typename": "User",
					"rest_id":    "u100",
					"core":       map[string]any{"screen_name": "alice", "name": "alice"},
				}}},
				"legacy": map[string]any{
					"full_text":      "sensitive focal",
					"created_at":     "Wed Jan 15 12:00:00 +0000 2020",
					"favorite_count": 3, "retweet_count": 1, "reply_count": 0,
					"user_id_str": "u100",
				},
			},
		}},
	}
	body, err := json.Marshal(map[string]any{
		"data": map[string]any{"threaded_conversation_with_injections_v2": map[string]any{"instructions": []any{
			map[string]any{"type": "TimelinePinEntry", "entry": map[string]any{
				"entryId": "tweet-100",
				"content": map[string]any{"entryType": "TimelineTimelineItem", "itemContent": wrapped},
			}},
		}}},
	})
	require.NoError(t, err)

	tweets, err := TweetDetail(body, "100")
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Equal(t, "100", tweets[0].ID)
	assert.Equal(t, "sensitive focal", tweets[0].Text)
	assert.Equal(t, "alice", tweets[0].AuthorHandle)
}

// TestParseTweetDetail_FocalAbsent: a deleted focal's page still carries
// replies/ancestors — an absent focal must be ErrNotFound, never a silently
// substituted tweets[0]. This is also the production pin for the pool's
// no-retry classification.
func TestParseTweetDetail_FocalAbsent(t *testing.T) {
	body := `{"data":{"threaded_conversation_with_injections_v2":{"instructions":[
		{"type":"TimelineAddEntries","entries":[
			{"entryId":"conversationthread-9","content":{"entryType":"TimelineTimelineModule","items":[
				{"entryId":"conversationthread-9-tweet-9","item":{"itemContent":{"__typename":"TimelineTweet","tweet_results":{"result":{"__typename":"Tweet","rest_id":"9","legacy":{"full_text":"orphan reply","created_at":"Wed Jan 15 12:00:00 +0000 2020","user_id_str":"u9"}}}}}}
			]}}
		]}
	]}}}`
	_, err := TweetDetail([]byte(body), "100")
	require.ErrorIs(t, err, model.ErrNotFound)
}

// TestParseTweetDetail_Errors: a 200 response carrying GraphQL errors[] must
// surface as an error, not an empty conversation.
func TestParseTweetDetail_Errors(t *testing.T) {
	body := `{"data":{},"errors":[{"message":"Rate limit exceeded"}]}`
	_, err := TweetDetail([]byte(body), "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "Rate limit exceeded")
}

func TestParseSearchTimeline_Errors(t *testing.T) {
	body := `{"data":{},"errors":[{"message":"query too long"}]}`
	_, err := SearchTimeline([]byte(body))
	require.Error(t, err)
	assert.ErrorContains(t, err, "query too long")
}

func TestParseSearchTimeline(t *testing.T) {
	body := `{
		"data": {
			"search_by_raw_query": {
				"search_timeline": {
					"timeline": {
						"instructions": [{
							"type": "TimelineAddEntries",
							"entries": [{
								"entryId": "tweet-123",
								"content": {
									"entryType": "TimelineTimelineItem",
									"__typename": "TimelineTimelineItem",
									"itemContent": {
										"__typename": "TimelineTweet",
										"tweet_results": {
											"result": {
												"__typename": "Tweet",
												"rest_id": "123",
												"legacy": {
													"full_text": "Hello $BTC $ETH",
													"created_at": "Mon Jan 02 15:04:05 +0000 2024",
													"favorite_count": 10,
													"retweet_count": 5,
													"quote_count": 2,
													"user_id_str": "999"
												},
												"views": {"count": "1000"}
											}
										}
									}
								}
							}]
						}]
					}
				}
			}
		}
	}`

	tweets, err := SearchTimeline([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(tweets) != 1 {
		t.Fatalf("expected 1 tweet, got %d", len(tweets))
	}
	tw := tweets[0]
	if tw.ID != "123" {
		t.Fatalf("expected ID 123, got %s", tw.ID)
	}
	if tw.AuthorID != "999" {
		t.Fatalf("expected author 999, got %s", tw.AuthorID)
	}
	if tw.Views != 1000 {
		t.Fatalf("expected 1000 views, got %d", tw.Views)
	}
	if tw.Likes != 10 {
		t.Fatalf("expected 10 likes, got %d", tw.Likes)
	}
	if len(tw.TokenMentions) != 2 {
		t.Fatalf("expected 2 token mentions, got %v", tw.TokenMentions)
	}
	if tw.TokenMentions[0] != "BTC" || tw.TokenMentions[1] != "ETH" {
		t.Fatalf("expected [BTC, ETH], got %v", tw.TokenMentions)
	}
}

// tweetInstructions is the shared inner instructions shape (one TimelineTweet +
// one bottom cursor), reused across the T5 read-cluster parser tests. Only the
// per-op root WRAPPER around it differs — these tests validate the parser + the
// documented root-key wiring against that shape, with the same rigor as
// TestParseSearchTimeline. Live queryID/response validation is deferred to the
// planned smoke test.
const tweetInstructions = `"instructions": [{
	"type": "TimelineAddEntries",
	"entries": [
		{
			"entryId": "tweet-123",
			"content": {
				"entryType": "TimelineTimelineItem",
				"__typename": "TimelineTimelineItem",
				"itemContent": {
					"__typename": "TimelineTweet",
					"tweet_results": {
						"result": {
							"__typename": "Tweet",
							"rest_id": "123",
							"legacy": {"full_text": "hello", "user_id_str": "999", "favorite_count": 7}
						}
					}
				}
			}
		},
		{
			"entryId": "cursor-bottom-9",
			"content": {
				"entryType": "TimelineTimelineCursor",
				"__typename": "TimelineTimelineCursor",
				"cursorType": "Bottom",
				"value": "CURSOR_NEXT"
			}
		}
	]
}]`

// assertOneTweetWithCursor checks the (tweets, cursor) result every read-cluster
// parser must produce against the shared tweetInstructions fixture.
func assertOneTweetWithCursor(t *testing.T, tweets []*model.Tweet, cursor string, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if len(tweets) != 1 {
		t.Fatalf("expected 1 tweet, got %d", len(tweets))
	}
	if tweets[0].ID != "123" {
		t.Fatalf("expected ID 123, got %s", tweets[0].ID)
	}
	if tweets[0].Likes != 7 {
		t.Fatalf("expected 7 likes, got %d", tweets[0].Likes)
	}
	if cursor != "CURSOR_NEXT" {
		t.Fatalf("expected bottom cursor CURSOR_NEXT, got %q", cursor)
	}
}

func TestParseBookmarks(t *testing.T) {
	body := `{"data":{"bookmark_timeline_v2":{"timeline":{` + tweetInstructions + `}}}}`
	tweets, cursor, err := Bookmarks([]byte(body))
	assertOneTweetWithCursor(t, tweets, cursor, err)
}

func TestParseHomeTimeline(t *testing.T) {
	body := `{"data":{"home":{"home_timeline_urt":{` + tweetInstructions + `}}}}`
	tweets, cursor, err := HomeTimeline([]byte(body))
	assertOneTweetWithCursor(t, tweets, cursor, err)
}

func TestParseListTweets(t *testing.T) {
	body := `{"data":{"list":{"tweets_timeline":{"timeline":{` + tweetInstructions + `}}}}}`
	tweets, cursor, err := ListTweets([]byte(body))
	assertOneTweetWithCursor(t, tweets, cursor, err)
}

func TestParseCommunityTweets(t *testing.T) {
	body := `{"data":{"communityResults":{"result":{"ranked_community_timeline":{"timeline":{` + tweetInstructions + `}}}}}}`
	tweets, cursor, err := CommunityTweets([]byte(body))
	assertOneTweetWithCursor(t, tweets, cursor, err)
}

// TestParseCommunityTweets_RecursiveFallback proves the recursive-fallback path:
// even if the ranked_community_timeline wrapper key (UNVERIFIED) is wrong at
// runtime, instructions found anywhere under data still parse. This is the
// resilience guarantee documented in timelineTweetParse.
func TestParseCommunityTweets_RecursiveFallback(t *testing.T) {
	body := `{"data":{"communityResults":{"result":{"some_other_wrapper_key":{"timeline":{` + tweetInstructions + `}}}}}}`
	tweets, cursor, err := CommunityTweets([]byte(body))
	assertOneTweetWithCursor(t, tweets, cursor, err)
}

// --- Discriminating fixtures (Fix 3) ---
//
// tweetEntryJSON builds a single TimelineTweet entry carrying rest_id.
func tweetEntryJSON(id string) string {
	return `{"entryId":"tweet-` + id + `","content":{"entryType":"TimelineTimelineItem","__typename":"TimelineTimelineItem","itemContent":{"__typename":"TimelineTweet","tweet_results":{"result":{"__typename":"Tweet","rest_id":"` + id + `","legacy":{"full_text":"x","user_id_str":"1"}}}}}}`
}

// instrBlockJSON wraps the given entries in a single TimelineAddEntries
// instruction, emitting the inner `"instructions": [...]` fragment.
func instrBlockJSON(entries ...string) string {
	out := `"instructions":[{"type":"TimelineAddEntries","entries":[`
	for i, e := range entries {
		if i > 0 {
			out += ","
		}
		out += e
	}
	return out + `]}]`
}

// assertTypedTweet asserts the parser chose the typed path: exactly the right
// tweet ("123") and never the decoy ("999"). It FAILS if the typed root key is
// broken, because the recursive fallback would then surface the larger decoy
// block instead.
func assertTypedTweet(t *testing.T, tweets []*model.Tweet, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	for _, tw := range tweets {
		if tw.ID == "999" {
			t.Fatalf("decoy tweet 999 returned — typed root key broke, fell back to recursive scan")
		}
	}
	if len(tweets) != 1 || tweets[0].ID != "123" {
		t.Fatalf("expected exactly typed tweet 123, got %d tweets %v", len(tweets), tweetIDs(tweets))
	}
}

func tweetIDs(tweets []*model.Tweet) []string {
	ids := make([]string, len(tweets))
	for i, tw := range tweets {
		ids[i] = tw.ID
	}
	return ids
}

// decoyBlock is a 2-entry instructions block (rest_id 999, 998) placed under a
// non-typed key. It out-sizes the 1-entry typed block, so if the typed key were
// broken the recursive fallback (largest-wins) would deterministically pick it —
// making every discriminating test below truly fail on a broken root key.
var decoyBlock = `"decoy_module":{` + instrBlockJSON(tweetEntryJSON("999"), tweetEntryJSON("998")) + `}`

// TestParseBookmarks_DiscriminatesRoot pins data.bookmark_timeline_v2.timeline:
// the typed path must win over a larger decoy block under data.
func TestParseBookmarks_DiscriminatesRoot(t *testing.T) {
	body := `{"data":{"bookmark_timeline_v2":{"timeline":{` + instrBlockJSON(tweetEntryJSON("123")) + `}},` + decoyBlock + `}}`
	tweets, _, err := Bookmarks([]byte(body))
	assertTypedTweet(t, tweets, err)
}

// TestParseHomeTimeline_DiscriminatesRoot pins data.home.home_timeline_urt.
func TestParseHomeTimeline_DiscriminatesRoot(t *testing.T) {
	body := `{"data":{"home":{"home_timeline_urt":{` + instrBlockJSON(tweetEntryJSON("123")) + `}},` + decoyBlock + `}}`
	tweets, _, err := HomeTimeline([]byte(body))
	assertTypedTweet(t, tweets, err)
}

// TestParseListTweets_DiscriminatesRoot pins data.list.tweets_timeline.timeline.
func TestParseListTweets_DiscriminatesRoot(t *testing.T) {
	body := `{"data":{"list":{"tweets_timeline":{"timeline":{` + instrBlockJSON(tweetEntryJSON("123")) + `}}},` + decoyBlock + `}}`
	tweets, _, err := ListTweets([]byte(body))
	assertTypedTweet(t, tweets, err)
}

// TestParseCommunityTweets_DiscriminatesRoot pins the UNVERIFIED root
// data.communityResults.result.ranked_community_timeline.timeline. This test
// pins the CURRENT assumption: if a live smoke test shows the real wrapper key
// differs, this test fails and the code + assertion update together.
func TestParseCommunityTweets_DiscriminatesRoot(t *testing.T) {
	body := `{"data":{"communityResults":{"result":{"ranked_community_timeline":{"timeline":{` + instrBlockJSON(tweetEntryJSON("123")) + `}}}},` + decoyBlock + `}}`
	tweets, _, err := CommunityTweets([]byte(body))
	assertTypedTweet(t, tweets, err)
}

// TestFindTimelineInstructions_LargestWinsDeterministic proves the recursive
// fallback is deterministic and picks the REAL timeline: two sibling
// instructions blocks under data — a 1-entry decoy (999) and a 3-entry real
// block (123,124,125) — must always resolve to the 3-entry block regardless of
// Go's randomized map iteration order.
func TestFindTimelineInstructions_LargestWinsDeterministic(t *testing.T) {
	data := `{"sidebar":{` + instrBlockJSON(tweetEntryJSON("999")) + `},"primary":{` +
		instrBlockJSON(tweetEntryJSON("123"), tweetEntryJSON("124"), tweetEntryJSON("125")) + `}}`
	for i := 0; i < 25; i++ {
		tl := findTimelineInstructions(json.RawMessage(data))
		tweets, err := extractTweetsFromTimeline(tl, "")
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if len(tweets) != 3 {
			t.Fatalf("iter %d: expected 3-entry real block, got %d tweets %v", i, len(tweets), tweetIDs(tweets))
		}
		if tweets[0].ID != "123" {
			t.Fatalf("iter %d: expected first tweet 123, got %s", i, tweets[0].ID)
		}
	}
}

// TestParseBlueVerifiedFollowers proves GetVerifiedFollowers' parser (the reused
// parseUserList) extracts a verified follower + bottom cursor from the
// data.user.result.timeline.timeline shape.
func TestParseBlueVerifiedFollowers(t *testing.T) {
	body := `{
		"data": {"user": {"result": {"timeline": {"timeline": {
			"instructions": [{
				"type": "TimelineAddEntries",
				"entries": [
					{
						"entryId": "user-42",
						"content": {
							"entryType": "TimelineTimelineItem",
							"__typename": "TimelineTimelineItem",
							"itemContent": {
								"__typename": "TimelineUser",
								"user_results": {"result": {
									"__typename": "User",
									"rest_id": "42",
									"legacy": {"screen_name": "verified_user", "name": "Verified"},
									"is_blue_verified": true
								}}
							}
						}
					},
					{
						"entryId": "cursor-bottom-1",
						"content": {
							"entryType": "TimelineTimelineCursor",
							"__typename": "TimelineTimelineCursor",
							"cursorType": "Bottom",
							"value": "USER_CURSOR_NEXT"
						}
					}
				]
			}]
		}}}}}
	}`
	users, cursor, err := UserList([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(users))
	}
	if users[0].Handle != "verified_user" {
		t.Fatalf("expected handle verified_user, got %s", users[0].Handle)
	}
	if !users[0].IsVerified {
		t.Fatal("expected verified follower")
	}
	if cursor != "USER_CURSOR_NEXT" {
		t.Fatalf("expected bottom cursor USER_CURSOR_NEXT, got %q", cursor)
	}
}

// --- T5.5 engagement mutation parsers ---

func TestParseAck_Favorite(t *testing.T) {
	// Success: bare "Done" ack.
	if err := Ack([]byte(`{"data":{"favorite_tweet":"Done"}}`), "FavoriteTweet", "favorite_tweet"); err != nil {
		t.Fatalf("expected nil on Done ack, got %v", err)
	}
	// Success: unfavorite key.
	if err := Ack([]byte(`{"data":{"unfavorite_tweet":"Done"}}`), "UnfavoriteTweet", "unfavorite_tweet"); err != nil {
		t.Fatalf("expected nil on Done ack, got %v", err)
	}
	// API error surfaced.
	err := Ack([]byte(`{"errors":[{"message":"already favorited"}]}`), "FavoriteTweet", "favorite_tweet")
	if err == nil {
		t.Fatal("expected error from errors[]")
	}
	if !strings.Contains(err.Error(), "already favorited") {
		t.Fatalf("expected API message surfaced, got %v", err)
	}
	// Missing / non-"Done" ack -> error (not silent success).
	if err := Ack([]byte(`{"data":{}}`), "FavoriteTweet", "favorite_tweet"); err == nil {
		t.Fatal("expected error on missing ack")
	}
	if err := Ack([]byte(`{"data":{"favorite_tweet":"Nope"}}`), "FavoriteTweet", "favorite_tweet"); err == nil {
		t.Fatal("expected error on non-Done ack")
	}
}

func TestParseCreateRetweet(t *testing.T) {
	// Success-shape -> retweet rest_id.
	body := `{"data":{"create_retweet":{"retweet_results":{"result":{"rest_id":"1700000000000000001"}}}}}`
	id, err := CreateRetweet([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if id != "1700000000000000001" {
		t.Fatalf("expected retweet id, got %q", id)
	}
	// errors[] surfaced.
	_, err = CreateRetweet([]byte(`{"errors":[{"message":"rate limited"}]}`))
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected API error surfaced, got %v", err)
	}
	// Empty/missing result -> error.
	if _, err := CreateRetweet([]byte(`{"data":{"create_retweet":{}}}`)); err == nil {
		t.Fatal("expected error on empty retweet result")
	}
}

func TestParseDeleteRetweet(t *testing.T) {
	// Success-shape -> source tweet rest_id (op key "unretweet").
	body := `{"data":{"unretweet":{"source_tweet_results":{"result":{"rest_id":"1600000000000000002"}}}}}`
	id, err := DeleteRetweet([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if id != "1600000000000000002" {
		t.Fatalf("expected source tweet id, got %q", id)
	}
	// errors[] surfaced.
	_, err = DeleteRetweet([]byte(`{"errors":[{"message":"not found"}]}`))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected API error surfaced, got %v", err)
	}
	// Empty/missing result -> error.
	if _, err := DeleteRetweet([]byte(`{"data":{"unretweet":{}}}`)); err == nil {
		t.Fatal("expected error on empty source result")
	}
}

func TestExtractTokenMentions(t *testing.T) {
	tests := []struct {
		text     string
		expected []string
	}{
		{"Hello $BTC and $ETH", []string{"BTC", "ETH"}},
		{"No mentions here", nil},
		{"$BTC $BTC duplicate", []string{"BTC"}},
		{"$A too short", nil}, // less than 2 chars
	}

	for _, tt := range tests {
		result := extractTokenMentions(tt.text)
		if len(result) != len(tt.expected) {
			t.Fatalf("extractTokenMentions(%q) = %v, want %v", tt.text, result, tt.expected)
		}
	}
}

func TestParseCreateTweet(t *testing.T) {
	// Success-shape -> tweet rest_id (ReplyTweet reuses this parser).
	body := `{"data":{"create_tweet":{"tweet_results":{"result":{"rest_id":"1800000000000000001"}}}}}`
	id, err := CreateTweet([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if id != "1800000000000000001" {
		t.Fatalf("expected tweet id, got %q", id)
	}
	// errors[] surfaced.
	_, err = CreateTweet([]byte(`{"errors":[{"message":"duplicate"}]}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected API error surfaced, got %v", err)
	}
	// Empty/missing result -> error (the silent-no-op trap for the reply path).
	if _, err := CreateTweet([]byte(`{"data":{"create_tweet":{}}}`)); err == nil {
		t.Fatal("expected error on empty tweet result")
	}
}

// TestParseUserByScreenName_CoreShape grounds the parser against a REAL,
// live-captured UserByScreenName response body (cz_binance, 2026-06-23), after
// X moved name / screen_name / created_at out of `legacy` into a new `core`
// object and the avatar to `avatar.image_url`. With the pre-migration parser
// (which reads only legacy.*) Handle/DisplayName/CreatedAt/HasAvatar come back
// empty even though numeric stats parse fine — the exact production symptom that
// produced empty handle/display_name from /v1/account/cz_binance.
//
// Fixture provenance: testdata/ubsn_cz_binance.json is the verbatim body
// returned by GetUserByScreenName via a downstream consumer's deployed credentials.
func TestParseUserByScreenName_CoreShape(t *testing.T) {
	body, err := os.ReadFile("../../testdata/ubsn_cz_binance.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	user, err := UserByScreenName(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// rest_id has never moved — sanity that we parsed the right node.
	if user.ID != "902926941413453824" {
		t.Fatalf("ID: want 902926941413453824, got %q", user.ID)
	}

	// The migrated fields — these are the bug.
	if user.Handle != "cz_binance" {
		t.Fatalf("Handle: want cz_binance, got %q (core.screen_name not read)", user.Handle)
	}
	if user.DisplayName != "CZ 🔶 BNB" {
		t.Fatalf("DisplayName: want %q, got %q (core.name not read)", "CZ 🔶 BNB", user.DisplayName)
	}
	if user.CreatedAt.IsZero() {
		t.Fatal("CreatedAt: want non-zero from core.created_at, got zero")
	}
	if user.CreatedAt.Year() != 2017 {
		t.Fatalf("CreatedAt: want year 2017, got %d", user.CreatedAt.Year())
	}
	if !user.HasAvatar {
		t.Fatal("HasAvatar: want true from avatar.image_url, got false")
	}

	// These never moved — must still parse from legacy.* so the fix does not
	// regress the working fields.
	if user.Followers != 11601459 {
		t.Fatalf("Followers: want 11601459, got %d", user.Followers)
	}
	if user.Following != 1280 {
		t.Fatalf("Following: want 1280, got %d", user.Following)
	}
	if user.TweetCount != 8838 {
		t.Fatalf("TweetCount: want 8838, got %d", user.TweetCount)
	}
	if user.ListedCount != 40383 {
		t.Fatalf("ListedCount: want 40383, got %d", user.ListedCount)
	}
	if !user.IsVerified {
		t.Fatal("IsVerified: want true (is_blue_verified), got false")
	}
	if !user.HasBio {
		t.Fatal("HasBio: want true, got false")
	}
	// Bio is grounded in the REAL captured body. This fixture carries the bio in
	// BOTH the post-migration profile_bio.description and the still-populated
	// legacy.description (identical content) — the migration-in-progress
	// signature. Asserting the exact text proves Bio parses to the captured value
	// regardless of which source bio() reads.
	const wantBio = "Buy the book (proceeds go to charity):\nEnglish: https://t.co/UxgYxYJ3NF\nChinese: https://t.co/ItFd8FEyuK \n\n@binance\n@BNBchain\n@YZiLabs\n@GiggleAcademy"
	if user.Bio != wantBio {
		t.Fatalf("Bio: want %q, got %q", wantBio, user.Bio)
	}
}

// TestParseUserResult_BioProfileBioPath proves Bio reads the post-migration
// profile_bio.description location. Here legacy.description is ABSENT while
// profile_bio.description carries the text — the shape X serves once the bio
// field finishes migrating out of legacy (mirroring the core.* migration this
// PR fixes). With the pre-fix parser (which read only r.Legacy.Description) this
// returns an empty Bio / HasBio=false; the bio() helper makes it read the new
// path. This is the RED-without-the-fix proof for the profile_bio fallback.
func TestParseUserResult_BioProfileBioPath(t *testing.T) {
	body := []byte(`{
		"data": {
			"user": {
				"result": {
					"__typename": "User",
					"id": "VXNlcjoxMDA=",
					"rest_id": "100",
					"core": {
						"name": "Migrated Bio User",
						"screen_name": "migrated_bio",
						"created_at": "Mon Jan 02 15:04:05 +0000 2021"
					},
					"profile_bio": {
						"description": "bio from the new profile_bio location"
					},
					"avatar": {
						"image_url": "https://pbs.twimg.com/profile_images/100/new.jpg"
					},
					"legacy": {
						"followers_count": 5,
						"friends_count": 2,
						"statuses_count": 9,
						"listed_count": 1
					},
					"is_blue_verified": true
				}
			}
		}
	}`)

	user, err := UserByScreenName(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	const wantBio = "bio from the new profile_bio location"
	if user.Bio != wantBio {
		t.Fatalf("Bio: want %q from profile_bio.description, got %q (new path not read)", wantBio, user.Bio)
	}
	if !user.HasBio {
		t.Fatal("HasBio: want true from profile_bio.description, got false")
	}
}

// TestParseUserResult_BioLegacyFallback proves the bio() helper still falls back
// to the pre-migration legacy.description when profile_bio is absent, so the new
// fallback is non-destructive for the old shape X may still serve during rollout.
func TestParseUserResult_BioLegacyFallback(t *testing.T) {
	body := []byte(`{
		"data": {
			"user": {
				"result": {
					"__typename": "User",
					"id": "VXNlcjoxMDE=",
					"rest_id": "101",
					"legacy": {
						"name": "Legacy Bio User",
						"screen_name": "legacy_bio",
						"description": "bio from legacy.description",
						"followers_count": 5
					},
					"is_blue_verified": false
				}
			}
		}
	}`)

	user, err := UserByScreenName(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	const wantBio = "bio from legacy.description"
	if user.Bio != wantBio {
		t.Fatalf("Bio: want %q from legacy.description fallback, got %q", wantBio, user.Bio)
	}
	if !user.HasBio {
		t.Fatal("HasBio: want true from legacy.description fallback, got false")
	}
}

// TestParseUserResult_LegacyFallback proves the fix is non-destructive across
// BOTH response shapes: an OLD, pre-migration body (name/screen_name/created_at
// still under legacy, no core object) must still parse. X may serve either shape
// during a rollout, so the parser must prefer core and fall back to legacy.
//
// This body is synthetic by necessity — X has already migrated, so the old shape
// can no longer be captured live. It mirrors the exact pre-2026-06 legacy layout.
func TestParseUserResult_LegacyFallback(t *testing.T) {
	body := []byte(`{
		"data": {
			"user": {
				"result": {
					"__typename": "User",
					"id": "VXNlcjo5OQ==",
					"rest_id": "99",
					"legacy": {
						"name": "Legacy Name",
						"screen_name": "legacy_handle",
						"created_at": "Mon Jan 02 15:04:05 +0000 2020",
						"followers_count": 7,
						"friends_count": 3,
						"statuses_count": 11,
						"listed_count": 1,
						"verified": true,
						"description": "old shape bio",
						"profile_image_url_https": "https://pbs.twimg.com/profile_images/9/legacy.jpg"
					},
					"is_blue_verified": false
				}
			}
		}
	}`)

	user, err := UserByScreenName(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if user.Handle != "legacy_handle" {
		t.Fatalf("Handle: want legacy_handle (legacy fallback), got %q", user.Handle)
	}
	if user.DisplayName != "Legacy Name" {
		t.Fatalf("DisplayName: want Legacy Name (legacy fallback), got %q", user.DisplayName)
	}
	if user.CreatedAt.IsZero() {
		t.Fatal("CreatedAt: want non-zero from legacy.created_at fallback, got zero")
	}
	if !user.HasAvatar {
		t.Fatal("HasAvatar: want true from legacy.profile_image_url_https fallback, got false")
	}
	if !user.IsVerified {
		t.Fatal("IsVerified: want true from legacy.verified, got false")
	}
}

// TestParseUserTweets_CoreAuthorShape grounds the tweet-author path against a
// REAL, live-captured UserTweets timeline entry (VitalikButerin, 2026-06-23).
// The nested tweet_results.result.core.user_results.result also moved
// name/screen_name into `core`, so the pre-migration parser (which reads
// ...user_results.result.legacy.screen_name) returns an empty AuthorHandle /
// AuthorName.
//
// Fixture provenance: testdata/usertweets_vitalik.json is a single real timeline
// entry extracted verbatim from the deployed UserTweets response.
func TestParseUserTweets_CoreAuthorShape(t *testing.T) {
	body, err := os.ReadFile("../../testdata/usertweets_vitalik.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	tweets, err := TweetTimeline(body, "295218901")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tweets) == 0 {
		t.Fatal("expected at least one tweet")
	}
	tw := tweets[0]
	if tw.AuthorHandle != "VitalikButerin" {
		t.Fatalf("AuthorHandle: want VitalikButerin from core.user_results.result.core.screen_name, got %q", tw.AuthorHandle)
	}
	if tw.AuthorName != "vitalik.eth" {
		t.Fatalf("AuthorName: want vitalik.eth from core.user_results.result.core.name, got %q", tw.AuthorName)
	}
	if tw.AuthorID != "295218901" {
		t.Fatalf("AuthorID: want 295218901, got %q", tw.AuthorID)
	}
}

// TestParseUserByScreenName_NoLegacyShape grounds the parser against a REAL,
// live-captured UserByScreenName response (elonmusk, 2026-10-17) in the
// post-2026-10 schema where X drops the `legacy` key ENTIRELY — a hard cut,
// not the hybrid shape seen during the core.* migration. Follower/following/
// tweet counts move to relationship_counts.* and tweet_counts.tweets; with a
// legacy-only parser all three silently return 0 while name/bio/avatar keep
// working — the exact production symptom (profile counters zeroed through the
// go-social shared read path).
//
// Fixture provenance: testdata/ubsn_elonmusk_nolegacy.json is the verbatim
// body returned by GetUserByScreenName through the deployed go-social shared
// client on 2026-10-17.
func TestParseUserByScreenName_NoLegacyShape(t *testing.T) {
	body, err := os.ReadFile("../../testdata/ubsn_elonmusk_nolegacy.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	user, err := UserByScreenName(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if user.ID != "44196397" {
		t.Fatalf("ID: want 44196397, got %q", user.ID)
	}
	if user.Handle != "elonmusk" {
		t.Fatalf("Handle: want elonmusk, got %q", user.Handle)
	}
	if user.DisplayName != "Elon Musk" {
		t.Fatalf("DisplayName: want Elon Musk, got %q", user.DisplayName)
	}
	// The bug under test: counts live in relationship_counts / tweet_counts,
	// not legacy. A legacy-only read yields exactly 0 — assert the captured
	// real values, not just non-zero.
	if user.Followers != 241501933 {
		t.Fatalf("Followers: want 241501933, got %d", user.Followers)
	}
	if user.Following != 1420 {
		t.Fatalf("Following: want 1420, got %d", user.Following)
	}
	if user.TweetCount != 110396 {
		t.Fatalf("TweetCount: want 110396, got %d", user.TweetCount)
	}
	// listed_count has no observed post-migration location — the field is
	// simply absent in this shape; assert 0 rather than a wrong-source value.
	if user.ListedCount != 0 {
		t.Fatalf("ListedCount: want 0 (no new-shape location), got %d", user.ListedCount)
	}
	if !user.IsVerified {
		t.Fatal("IsVerified: want true (is_blue_verified), got false")
	}
	if !user.HasAvatar {
		t.Fatal("HasAvatar: want true from avatar.image_url, got false")
	}
	if !user.HasBio {
		t.Fatal("HasBio: want true, got false")
	}
}

// TestParseUserResult_RelationshipCountsPreferred proves presence (not value)
// selects the source: when relationship_counts is present alongside legacy,
// the new location is authoritative even if the legacy values differ — a
// migration-in-progress hybrid must not read stale legacy counters.
func TestParseUserResult_RelationshipCountsPreferred(t *testing.T) {
	body := []byte(`{
		"data": {
			"user": {
				"result": {
					"__typename": "User",
					"id": "VXNlcjoxMDI=",
					"rest_id": "102",
					"core": {"name": "Hybrid", "screen_name": "hybrid"},
					"relationship_counts": {"followers": 10, "following": 0},
					"tweet_counts": {"tweets": 3},
					"legacy": {"followers_count": 999, "friends_count": 888, "statuses_count": 777, "listed_count": 5},
					"is_blue_verified": false
				}
			}
		}
	}`)

	user, err := UserByScreenName(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if user.Followers != 10 {
		t.Fatalf("Followers: want 10 from relationship_counts, got %d", user.Followers)
	}
	// A genuine zero in the new location must not fall back to legacy's 888.
	if user.Following != 0 {
		t.Fatalf("Following: want 0 from relationship_counts, got %d", user.Following)
	}
	if user.TweetCount != 3 {
		t.Fatalf("TweetCount: want 3 from tweet_counts, got %d", user.TweetCount)
	}
	// listed_count is legacy-only by design.
	if user.ListedCount != 5 {
		t.Fatalf("ListedCount: want 5 from legacy (only location), got %d", user.ListedCount)
	}
}

// UserListPage reports the user entries a page carried, so a paginating caller
// can tell an empty page from one whose users were all unavailable.
func TestUserListPage_EntriesCount(t *testing.T) {
	wrap := func(entries string) []byte {
		return []byte(`{"data":{"user":{"result":{"timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[` + entries + `]}]}}}}}}`)
	}
	cursor := `{"entryId":"cursor-bottom-1","content":{"entryType":"TimelineTimelineCursor","cursorType":"Bottom","value":"cur"}}`
	user := func(typ string) string {
		return `{"entryId":"user-1","content":{"entryType":"TimelineTimelineItem","itemContent":{"__typename":"TimelineUser","user_results":{"result":{"__typename":"` + typ + `","rest_id":"7","core":{"screen_name":"a"}}}}}}`
	}

	empty, err := UserListPage(wrap(cursor))
	require.NoError(t, err)
	assert.Equal(t, 0, empty.Entries)
	assert.Equal(t, "cur", empty.Cursor, "the cursor of an empty page is still reported")

	unavailable, err := UserListPage(wrap(`{"entryId":"user-2","content":{"entryType":"TimelineTimelineItem","itemContent":{"__typename":"TimelineUser","user_results":{"result":{"__typename":"UserUnavailable"}}}}},` + cursor))
	require.NoError(t, err)
	assert.Equal(t, 1, unavailable.Entries, "an unavailable account is still an entry")
	assert.Empty(t, unavailable.Users)

	one, err := UserListPage(wrap(user("User") + "," + cursor))
	require.NoError(t, err)
	assert.Equal(t, 1, one.Entries)
	assert.Len(t, one.Users, 1)
}
