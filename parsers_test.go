package twitter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	user, err := parseUserByScreenName([]byte(body))
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

	_, err := parseUserByScreenName([]byte(body))
	require.ErrorIs(t, err, ErrNotFound, "unavailable user is a deterministic miss")
}

// TestParseUserByScreenName_ErrorsWithData: a partial response carrying both
// errors[] and a typed UserUnavailable result must resolve via the data — the
// typed result is a deterministic ErrNotFound, not a retryable generic error.
func TestParseUserByScreenName_ErrorsWithData(t *testing.T) {
	body := `{
		"data": {"user": {"result": {"__typename": "UserUnavailable", "rest_id": ""}}},
		"errors": [{"message": "UserUnavailable"}]
	}`
	_, err := parseUserByScreenName([]byte(body))
	require.ErrorIs(t, err, ErrNotFound)
}

func TestParseUserByScreenName_ErrorsOnly(t *testing.T) {
	body := `{"data": {}, "errors": [{"message": "bad query"}]}`
	_, err := parseUserByScreenName([]byte(body))
	require.Error(t, err)
	assert.ErrorContains(t, err, "bad query")
	assert.NotErrorIs(t, err, ErrNotFound)
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
	users, err := parseSearchUsersTimeline(body)
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
	users, err := parseSearchUsersTimeline(body)
	require.NoError(t, err)
	require.Len(t, users, 1)
	assert.Equal(t, "ann", users[0].Handle)
}

// TestSplitConversation covers the focal/replies split used by
// GetTweetConversation: focal is the ID match (else first, defensive —
// production callers go through parseTweetDetail's ErrNotFound gate),
// replies keep entry order minus every copy of the focal ID.
func TestSplitConversation(t *testing.T) {
	focal := &Tweet{ID: "10"}
	reply1 := &Tweet{ID: "11"}
	reply2 := &Tweet{ID: "12"}

	main, replies := splitConversation([]*Tweet{focal, reply1, reply2}, "10")
	assert.Equal(t, "10", main.ID)
	require.Len(t, replies, 2)
	assert.Equal(t, "11", replies[0].ID)

	// The focal reappears as the anchor item of its own conversationthread-*
	// module — every copy must stay out of replies.
	dup := &Tweet{ID: "10"}
	main, replies = splitConversation([]*Tweet{focal, dup, reply1}, "10")
	assert.Equal(t, "10", main.ID)
	require.Len(t, replies, 1)
	assert.Equal(t, "11", replies[0].ID)

	// Defensive fallback only: unreachable through getTweetDetail, which
	// returns ErrNotFound when the focal is absent.
	main, replies = splitConversation([]*Tweet{reply1, reply2}, "10")
	assert.Equal(t, "11", main.ID)
	require.Len(t, replies, 1)
	assert.Equal(t, "12", replies[0].ID)

	main, _ = splitConversation([]*Tweet{focal}, "10")
	assert.Equal(t, "10", main.ID)

	main, replies = splitConversation(nil, "10")
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

	tweets, err := parseTweetDetail(body, "100")
	require.NoError(t, err)
	require.Len(t, tweets, 4, "pin + focal module anchor + 2 replies; both promoted items excluded")
	focal, replies := splitConversation(tweets, "100")
	require.NotNil(t, focal)
	assert.Equal(t, "100", focal.ID)
	assert.Equal(t, "focal tweet", focal.Text)
	require.Len(t, replies, 2)
	assert.Equal(t, "101", replies[0].ID)
	assert.Equal(t, "102", replies[1].ID)
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
	_, err := parseTweetDetail([]byte(body), "100")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestParseTweetDetail_Errors: a 200 response carrying GraphQL errors[] must
// surface as an error, not an empty conversation.
func TestParseTweetDetail_Errors(t *testing.T) {
	body := `{"data":{},"errors":[{"message":"Rate limit exceeded"}]}`
	_, err := parseTweetDetail([]byte(body), "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "Rate limit exceeded")
}

func TestParseSearchTimeline_Errors(t *testing.T) {
	body := `{"data":{},"errors":[{"message":"query too long"}]}`
	_, err := parseSearchTimeline([]byte(body))
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

	tweets, err := parseSearchTimeline([]byte(body))
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
func assertOneTweetWithCursor(t *testing.T, tweets []*Tweet, cursor string, err error) {
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
	tweets, cursor, err := parseBookmarks([]byte(body))
	assertOneTweetWithCursor(t, tweets, cursor, err)
}

func TestParseHomeTimeline(t *testing.T) {
	body := `{"data":{"home":{"home_timeline_urt":{` + tweetInstructions + `}}}}`
	tweets, cursor, err := parseHomeTimeline([]byte(body))
	assertOneTweetWithCursor(t, tweets, cursor, err)
}

func TestParseListTweets(t *testing.T) {
	body := `{"data":{"list":{"tweets_timeline":{"timeline":{` + tweetInstructions + `}}}}}`
	tweets, cursor, err := parseListTweets([]byte(body))
	assertOneTweetWithCursor(t, tweets, cursor, err)
}

func TestParseCommunityTweets(t *testing.T) {
	body := `{"data":{"communityResults":{"result":{"ranked_community_timeline":{"timeline":{` + tweetInstructions + `}}}}}}`
	tweets, cursor, err := parseCommunityTweets([]byte(body))
	assertOneTweetWithCursor(t, tweets, cursor, err)
}

// TestParseCommunityTweets_RecursiveFallback proves the recursive-fallback path:
// even if the ranked_community_timeline wrapper key (UNVERIFIED) is wrong at
// runtime, instructions found anywhere under data still parse. This is the
// resilience guarantee documented in timelineTweetParse.
func TestParseCommunityTweets_RecursiveFallback(t *testing.T) {
	body := `{"data":{"communityResults":{"result":{"some_other_wrapper_key":{"timeline":{` + tweetInstructions + `}}}}}}`
	tweets, cursor, err := parseCommunityTweets([]byte(body))
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
func assertTypedTweet(t *testing.T, tweets []*Tweet, err error) {
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

func tweetIDs(tweets []*Tweet) []string {
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
	tweets, _, err := parseBookmarks([]byte(body))
	assertTypedTweet(t, tweets, err)
}

// TestParseHomeTimeline_DiscriminatesRoot pins data.home.home_timeline_urt.
func TestParseHomeTimeline_DiscriminatesRoot(t *testing.T) {
	body := `{"data":{"home":{"home_timeline_urt":{` + instrBlockJSON(tweetEntryJSON("123")) + `}},` + decoyBlock + `}}`
	tweets, _, err := parseHomeTimeline([]byte(body))
	assertTypedTweet(t, tweets, err)
}

// TestParseListTweets_DiscriminatesRoot pins data.list.tweets_timeline.timeline.
func TestParseListTweets_DiscriminatesRoot(t *testing.T) {
	body := `{"data":{"list":{"tweets_timeline":{"timeline":{` + instrBlockJSON(tweetEntryJSON("123")) + `}}},` + decoyBlock + `}}`
	tweets, _, err := parseListTweets([]byte(body))
	assertTypedTweet(t, tweets, err)
}

// TestParseCommunityTweets_DiscriminatesRoot pins the UNVERIFIED root
// data.communityResults.result.ranked_community_timeline.timeline. This test
// pins the CURRENT assumption: if a live smoke test shows the real wrapper key
// differs, this test fails and the code + assertion update together.
func TestParseCommunityTweets_DiscriminatesRoot(t *testing.T) {
	body := `{"data":{"communityResults":{"result":{"ranked_community_timeline":{"timeline":{` + instrBlockJSON(tweetEntryJSON("123")) + `}}}},` + decoyBlock + `}}`
	tweets, _, err := parseCommunityTweets([]byte(body))
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
	users, cursor, err := parseUserList([]byte(body))
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
	if err := parseAck([]byte(`{"data":{"favorite_tweet":"Done"}}`), "FavoriteTweet", "favorite_tweet"); err != nil {
		t.Fatalf("expected nil on Done ack, got %v", err)
	}
	// Success: unfavorite key.
	if err := parseAck([]byte(`{"data":{"unfavorite_tweet":"Done"}}`), "UnfavoriteTweet", "unfavorite_tweet"); err != nil {
		t.Fatalf("expected nil on Done ack, got %v", err)
	}
	// API error surfaced.
	err := parseAck([]byte(`{"errors":[{"message":"already favorited"}]}`), "FavoriteTweet", "favorite_tweet")
	if err == nil {
		t.Fatal("expected error from errors[]")
	}
	if !strings.Contains(err.Error(), "already favorited") {
		t.Fatalf("expected API message surfaced, got %v", err)
	}
	// Missing / non-"Done" ack -> error (not silent success).
	if err := parseAck([]byte(`{"data":{}}`), "FavoriteTweet", "favorite_tweet"); err == nil {
		t.Fatal("expected error on missing ack")
	}
	if err := parseAck([]byte(`{"data":{"favorite_tweet":"Nope"}}`), "FavoriteTweet", "favorite_tweet"); err == nil {
		t.Fatal("expected error on non-Done ack")
	}
}

func TestParseCreateRetweet(t *testing.T) {
	// Success-shape -> retweet rest_id.
	body := `{"data":{"create_retweet":{"retweet_results":{"result":{"rest_id":"1700000000000000001"}}}}}`
	id, err := parseCreateRetweet([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if id != "1700000000000000001" {
		t.Fatalf("expected retweet id, got %q", id)
	}
	// errors[] surfaced.
	_, err = parseCreateRetweet([]byte(`{"errors":[{"message":"rate limited"}]}`))
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected API error surfaced, got %v", err)
	}
	// Empty/missing result -> error.
	if _, err := parseCreateRetweet([]byte(`{"data":{"create_retweet":{}}}`)); err == nil {
		t.Fatal("expected error on empty retweet result")
	}
}

func TestParseDeleteRetweet(t *testing.T) {
	// Success-shape -> source tweet rest_id (op key "unretweet").
	body := `{"data":{"unretweet":{"source_tweet_results":{"result":{"rest_id":"1600000000000000002"}}}}}`
	id, err := parseDeleteRetweet([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if id != "1600000000000000002" {
		t.Fatalf("expected source tweet id, got %q", id)
	}
	// errors[] surfaced.
	_, err = parseDeleteRetweet([]byte(`{"errors":[{"message":"not found"}]}`))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected API error surfaced, got %v", err)
	}
	// Empty/missing result -> error.
	if _, err := parseDeleteRetweet([]byte(`{"data":{"unretweet":{}}}`)); err == nil {
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

func TestCT0(t *testing.T) {
	ct0 := GenerateCT0()
	if len(ct0) != 64 {
		t.Fatalf("expected 64 char hex, got %d chars", len(ct0))
	}
	// Should be different each time
	ct02 := GenerateCT0()
	if ct0 == ct02 {
		t.Fatal("expected different ct0 values")
	}
}

func TestParseCreateTweet(t *testing.T) {
	// Success-shape -> tweet rest_id (ReplyTweet reuses this parser).
	body := `{"data":{"create_tweet":{"tweet_results":{"result":{"rest_id":"1800000000000000001"}}}}}`
	id, err := parseCreateTweet([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if id != "1800000000000000001" {
		t.Fatalf("expected tweet id, got %q", id)
	}
	// errors[] surfaced.
	_, err = parseCreateTweet([]byte(`{"errors":[{"message":"duplicate"}]}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected API error surfaced, got %v", err)
	}
	// Empty/missing result -> error (the silent-no-op trap for the reply path).
	if _, err := parseCreateTweet([]byte(`{"data":{"create_tweet":{}}}`)); err == nil {
		t.Fatal("expected error on empty tweet result")
	}
}
