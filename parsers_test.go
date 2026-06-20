package twitter

import "testing"

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
