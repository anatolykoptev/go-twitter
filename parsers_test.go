package twitter

import (
	"encoding/json"
	"strings"
	"testing"
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
	if err == nil {
		t.Fatal("expected error for unavailable user")
	}
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
