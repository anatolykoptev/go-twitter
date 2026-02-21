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
