package twitter

import (
	"os"
	"testing"
)

// TestParseUserByScreenName_CoreShape grounds the parser against a REAL,
// live-captured UserByScreenName response body (cz_binance, 2026-06-23), after
// X moved name / screen_name / created_at out of `legacy` into a new `core`
// object and the avatar to `avatar.image_url`. With the pre-migration parser
// (which reads only legacy.*) Handle/DisplayName/CreatedAt/HasAvatar come back
// empty even though numeric stats parse fine — the exact production symptom that
// produced empty handle/display_name from /v1/account/cz_binance.
//
// Fixture provenance: testdata/ubsn_cz_binance.json is the verbatim body
// returned by GetUserByScreenName via the deployed go-hully credentials.
func TestParseUserByScreenName_CoreShape(t *testing.T) {
	body, err := os.ReadFile("testdata/ubsn_cz_binance.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	user, err := parseUserByScreenName(body)
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

	user, err := parseUserByScreenName(body)
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
	body, err := os.ReadFile("testdata/usertweets_vitalik.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	tweets, err := parseTweetTimeline(body, "295218901")
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
