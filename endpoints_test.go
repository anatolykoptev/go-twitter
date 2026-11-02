package twitter

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/anatolykoptev/go-twitter/internal/operations"
)

// TestEndpointURL_NormalID proves URL() yields the expected path for a normal
// queryID (PathEscape is a no-op on the safe alphabet).
func TestEndpointURL_NormalID(t *testing.T) {
	e := Endpoint{ID: "IGgvgiOx4QZndDHuD3x9TQ", Name: "UserByScreenName"}
	want := "https://x.com/i/api/graphql/IGgvgiOx4QZndDHuD3x9TQ/UserByScreenName"
	assert.Equal(t, want, e.URL())
}

// TestEndpointURL_EscapesHostileID proves an ID containing a `/` or a space is
// percent-escaped so it cannot break out of its path segment.
func TestEndpointURL_EscapesHostileID(t *testing.T) {
	for _, id := range []string{"AAA/../../evil", "has space", "a/b"} {
		got := Endpoint{ID: id, Name: "Op"}.URL()
		assert.NotContains(t, got, id, "raw hostile ID must not appear verbatim")
		assert.True(t, strings.HasPrefix(got, "https://x.com/i/api/graphql/"),
			"escaped URL keeps the fixed base prefix")
		// The variable segment must contain no raw slash or space.
		seg := strings.TrimPrefix(got, "https://x.com/i/api/graphql/")
		seg = strings.TrimSuffix(seg, "/Op")
		assert.NotContains(t, seg, "/")
		assert.NotContains(t, seg, " ")
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	orig := Endpoints["TweetDetail"].ID

	t.Setenv("TWITTER_QID_TWEET_DETAIL", "test_override_id")
	ApplyEnvOverrides()

	assert.Equal(t, "test_override_id", Endpoints["TweetDetail"].ID)

	// Restore original value.
	ep := Endpoints["TweetDetail"]
	ep.ID = orig
	Endpoints["TweetDetail"] = ep
}

func TestApplyEnvOverrides_AllEndpoints(t *testing.T) {
	cases := map[string]string{
		"TweetDetail":      "TWITTER_QID_TWEET_DETAIL",
		"UserByScreenName": "TWITTER_QID_USER_BY_SCREEN_NAME",
		"UserTweets":       "TWITTER_QID_USER_TWEETS",
		"SearchTimeline":   "TWITTER_QID_SEARCH_TIMELINE",
		"Followers":        "TWITTER_QID_FOLLOWERS",
		"Following":        "TWITTER_QID_FOLLOWING",
		"Retweeters":       "TWITTER_QID_RETWEETERS",
		"CreateTweet":      "TWITTER_QID_CREATE_TWEET",
		// T5 read cluster.
		"Bookmarks":                "TWITTER_QID_BOOKMARKS",
		"HomeTimeline":             "TWITTER_QID_HOME_TIMELINE",
		"HomeLatestTimeline":       "TWITTER_QID_HOME_LATEST_TIMELINE",
		"ListLatestTweetsTimeline": "TWITTER_QID_LIST_LATEST_TWEETS_TIMELINE",
		"CommunityTweetsTimeline":  "TWITTER_QID_COMMUNITY_TWEETS_TIMELINE",
		"BlueVerifiedFollowers":    "TWITTER_QID_BLUE_VERIFIED_FOLLOWERS",
		// T5.5 engagement mutations.
		"FavoriteTweet":   "TWITTER_QID_FAVORITE_TWEET",
		"UnfavoriteTweet": "TWITTER_QID_UNFAVORITE_TWEET",
		"CreateRetweet":   "TWITTER_QID_CREATE_RETWEET",
		"DeleteRetweet":   "TWITTER_QID_DELETE_RETWEET",
	}

	origIDs := make(map[string]string, len(cases))
	for name := range cases {
		origIDs[name] = Endpoints[name].ID
	}

	for name, envKey := range cases {
		t.Setenv(envKey, "override_"+name)
	}
	ApplyEnvOverrides()

	for name := range cases {
		assert.Equal(t, "override_"+name, Endpoints[name].ID)
	}

	// Restore originals.
	for name, id := range origIDs {
		ep := Endpoints[name]
		ep.ID = id
		Endpoints[name] = ep
	}
}

// TestRequiresAuth_ReadCluster proves every T5 read-cluster op requires a real
// authenticated account (no guest fallback — the Mar-2026 lesson).
func TestRequiresAuth_ReadCluster(t *testing.T) {
	for _, op := range []string{
		"Bookmarks", "HomeTimeline", "HomeLatestTimeline",
		"ListLatestTweetsTimeline", "CommunityTweetsTimeline", "BlueVerifiedFollowers",
	} {
		assert.True(t, requiresAuth(op), "op %q must require auth (no guest fallback)", op)
	}
}

// TestReadClusterEndpointsRegistered proves each new op has an Endpoints entry
// (non-empty ID + matching Name) so EndpointURL resolves it.
func TestReadClusterEndpointsRegistered(t *testing.T) {
	for _, op := range []string{
		"Bookmarks", "HomeTimeline", "HomeLatestTimeline",
		"ListLatestTweetsTimeline", "CommunityTweetsTimeline", "BlueVerifiedFollowers",
	} {
		ep, ok := Endpoints[op]
		assert.True(t, ok, "op %q must be registered in Endpoints", op)
		assert.NotEmpty(t, ep.ID, "op %q must have a seed queryID", op)
		assert.Equal(t, op, ep.Name, "op %q Name must match its key", op)
	}
}

// TestRequiresAuth_EngagementMutations proves every T5.5 engagement mutation
// requires a real authenticated account (writes never fall back to a guest
// token). ReplyTweet routes through the CreateTweet op, already covered.
func TestRequiresAuth_EngagementMutations(t *testing.T) {
	for _, op := range []string{
		"FavoriteTweet", "UnfavoriteTweet", "CreateRetweet", "DeleteRetweet", "CreateTweet",
	} {
		assert.True(t, requiresAuth(op), "engagement op %q must require auth (no guest fallback)", op)
	}
}

// TestEngagementEndpointsRegistered proves each new engagement op has an
// Endpoints entry (non-empty seed ID + matching Name) so EndpointURL resolves it.
func TestEngagementEndpointsRegistered(t *testing.T) {
	for _, op := range []string{
		"FavoriteTweet", "UnfavoriteTweet", "CreateRetweet", "DeleteRetweet",
	} {
		ep, ok := Endpoints[op]
		assert.True(t, ok, "op %q must be registered in Endpoints", op)
		assert.NotEmpty(t, ep.ID, "op %q must have a seed queryID", op)
		assert.Equal(t, op, ep.Name, "op %q Name must match its key", op)
	}
}

func TestApplyEnvOverrides_EmptyEnv(t *testing.T) {
	orig := Endpoints["TweetDetail"].ID
	// Ensure env var is unset.
	t.Setenv("TWITTER_QID_TWEET_DETAIL", "")
	ApplyEnvOverrides()

	// Should remain unchanged when env var is empty.
	assert.Equal(t, orig, Endpoints["TweetDetail"].ID)
}

// TestGeneratedOverrideChain proves the runtime priority env > generated >
// committed holds across applyGeneratedOverrides + ApplyEnvOverrides, for the
// same operation under each of the three layers.
func TestGeneratedOverrideChain(t *testing.T) {
	const (
		op        = "TweetDetail"
		envKey    = "TWITTER_QID_TWEET_DETAIL"
		committed = "committed_id"
		genID     = "generated_id"
		envID     = "env_id"
	)

	// Snapshot the package state we mutate, restore it after.
	origGen, origGenOK := operations.GeneratedQueryIDs[op]
	origEp := Endpoints[op]
	t.Cleanup(func() {
		if origGenOK {
			operations.GeneratedQueryIDs[op] = origGen
		} else {
			delete(operations.GeneratedQueryIDs, op)
		}
		Endpoints[op] = origEp
		applyGeneratedOverrides()
		ApplyEnvOverrides()
	})

	// setCommitted resets the endpoint's ID to the committed literal, then runs
	// the override chain in production order.
	resetAndApply := func() {
		ep := Endpoints[op]
		ep.ID = committed
		Endpoints[op] = ep
		applyGeneratedOverrides()
		ApplyEnvOverrides()
	}

	t.Run("env beats generated", func(t *testing.T) {
		operations.GeneratedQueryIDs[op] = genID
		t.Setenv(envKey, envID)
		resetAndApply()
		assert.Equal(t, envID, Endpoints[op].ID)
	})

	t.Run("generated beats committed", func(t *testing.T) {
		operations.GeneratedQueryIDs[op] = genID
		t.Setenv(envKey, "")
		resetAndApply()
		assert.Equal(t, genID, Endpoints[op].ID)
	})

	t.Run("committed when neither generated nor env present", func(t *testing.T) {
		delete(operations.GeneratedQueryIDs, op)
		t.Setenv(envKey, "")
		resetAndApply()
		assert.Equal(t, committed, Endpoints[op].ID)
	})
}

// TestMutationQueryIDs_LiveVerified pins the 2026-06-20 live-verified CreateRetweet
// and DeleteRetweet queryIDs. A change here is a deliberate refresh and must be
// re-verified with a live round-trip (see project memory / PR notes).
func TestMutationQueryIDs_LiveVerified(t *testing.T) {
	want := map[string]string{
		"CreateRetweet": "mbRO74GrOvSfRcJnlMapnQ",
		"DeleteRetweet": "ZyZigVsNiFO6v1dEks1eWg",
	}
	for op, id := range want {
		assert.Equal(t, id, Endpoints[op].ID,
			"op %q queryID drifted from the live-verified value; re-verify with a live round-trip before changing", op)
	}
}

// TestUnretweetUsesSourceTweetIDVariable source-greps graphql.go to guard the
// DeleteRetweet variable name. x.com's DeleteRetweet op keys the target by
// "source_tweet_id" (the original tweet's ID); sending "tweet_id" returns HTTP
// 422 GRAPHQL_VALIDATION_FAILED (confirmed live 2026-06-20). This guards against
// a regression back to the bare "tweet_id" key.
func TestUnretweetUsesSourceTweetIDVariable(t *testing.T) {
	src, err := os.ReadFile("graphql.go")
	if err != nil {
		t.Fatalf("read graphql.go: %v", err)
	}
	// Locate the Unretweet body and assert it posts source_tweet_id.
	text := string(src)
	const marker = "func (c *Client) Unretweet("
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatal("Unretweet method not found in graphql.go")
	}
	// Bound the scan to the function body (up to the next func decl).
	rest := text[i:]
	if j := strings.Index(rest[len(marker):], "\nfunc "); j >= 0 {
		rest = rest[:len(marker)+j]
	}
	if !strings.Contains(rest, `"source_tweet_id"`) {
		t.Error("Unretweet must POST the DeleteRetweet variable as \"source_tweet_id\" (x.com 422s on bare tweet_id)")
	}
}

// TestReadOpQueryIDs_LiveVerified pins the 2026-06-20 live-captured Followers,
// Following and Retweeters queryIDs. Each was confirmed with a live round-trip
// (GetFollowers/GetFollowing/GetRetweeters returning real users, see PR notes).
// A change here is a deliberate refresh and must be re-verified live.
func TestReadOpQueryIDs_LiveVerified(t *testing.T) {
	want := map[string]string{
		"Followers":  "9jsVJ9l2uXUIKslHvJqIhw",
		"Following":  "OLm4oHZBfqWx8jbcEhWoFw",
		"Retweeters": "FeoLYPQ-q4bmjGLTZTGs0g",
	}
	for op, id := range want {
		assert.Equal(t, id, Endpoints[op].ID,
			"op %q queryID drifted from the live-verified value; re-verify with a live round-trip before changing", op)
	}
}

// TestFollowersFeatureSet guards the v0.6.2 finding that the Followers/Following
// ops validate a DIFFERENT GraphQL feature set than the shared gqlFeatures()
// baseline. Sending the baseline returns HTTP 404 (feature-set mismatch, not a
// stale queryId alone). The live x.com request INCLUDES rweb_cashtags_enabled
// and OMITS responsive_web_graphql_exclude_directive_enabled. These two flags
// are the discriminator; if a refactor ever points Followers back at
// gqlFeatures(), this fails the build.
func TestFollowersFeatureSet(t *testing.T) {
	for _, op := range []string{"Followers", "Following"} {
		f := Endpoints[op].Features
		if v, ok := f["rweb_cashtags_enabled"]; !ok || v != true {
			t.Errorf("%s must send rweb_cashtags_enabled=true (live x.com requirement); got ok=%v v=%v", op, ok, v)
		}
		if _, ok := f["responsive_web_graphql_exclude_directive_enabled"]; ok {
			t.Errorf("%s must NOT send responsive_web_graphql_exclude_directive_enabled (live x.com 404s with it present)", op)
		}
	}
}

// TestFetchUserListSendsGrokTranslatedBio guards the withGrokTranslatedBio
// variable that the live Followers/Following request sends. Omitting it together
// with the legacy feature set was part of the 404 failure mode.
func TestFetchUserListSendsGrokTranslatedBio(t *testing.T) {
	src, err := os.ReadFile("graphql.go")
	if err != nil {
		t.Fatalf("read graphql.go: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, `"withGrokTranslatedBio"`) {
		t.Error("fetchUserList must send the withGrokTranslatedBio variable (live x.com Followers/Following request includes it)")
	}
	// The variable must be SCOPED to Followers/Following — BlueVerifiedFollowers
	// rides the same fetchUserList helper but was verified live WITHOUT it. Guard
	// the op-scoping condition so a refactor cannot silently re-globalize it.
	if !strings.Contains(text, `operation == "Followers" || operation == "Following"`) {
		t.Error("withGrokTranslatedBio must be scoped to Followers/Following only (BlueVerifiedFollowers must keep its verified request shape)")
	}
}

// TestFetchUserListFollowersFollowingUsePOST guards the GET→POST migration: X
// migrated the Followers/Following social-graph reads to POST (2026-06-20; same
// pattern as SearchTimeline, docs/2026-03-31-twitter-searchtimeline-404.md). A
// revert to doGET passes every other unit test and only surfaces as a live
// bare-404, so assert the POST branch stays in source.
func TestFetchUserListFollowersFollowingUsePOST(t *testing.T) {
	src, err := os.ReadFile("graphql.go")
	if err != nil {
		t.Fatalf("read graphql.go: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, "doPoolPOST(ctx, operation, url, payload)") {
		t.Error("Followers/Following must POST via doPoolPOST — the GET→POST migration appears reverted (live x.com returns bare 404 on GET)")
	}
}
