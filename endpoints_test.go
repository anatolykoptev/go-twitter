package twitter

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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

// TestGqlFeatures_GeneratedPreferred proves gqlFeatures() sources the generated
// baseline when generatedFeatures is non-empty, and falls back to the committed
// literal when it is empty.
func TestGqlFeatures_GeneratedPreferred(t *testing.T) {
	orig := generatedFeatures
	t.Cleanup(func() { generatedFeatures = orig })

	t.Run("generated baseline used when present", func(t *testing.T) {
		generatedFeatures = map[string]any{"only_generated_flag": true}
		got := gqlFeatures()
		if _, ok := got["only_generated_flag"]; !ok {
			t.Fatalf("expected generated flag, got %v", got)
		}
		if len(got) != 1 {
			t.Fatalf("expected only the generated set, got %d flags", len(got))
		}
	})

	t.Run("falls back to committed literal when generated empty", func(t *testing.T) {
		generatedFeatures = map[string]any{}
		got := gqlFeatures()
		want := committedFeatures()
		if len(got) != len(want) {
			t.Fatalf("fallback len = %d, want committed len %d", len(got), len(want))
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("fallback[%q] = %v, want %v", k, got[k], v)
			}
		}
	})
}

// TestGeneratedFeatures_InitialNoOp proves the committed features_gen.go baseline
// equals committedFeatures() verbatim, so the first ship of the generated layer is
// a behavioural no-op (gqlFeatures() returns the same set it did before T4).
func TestGeneratedFeatures_InitialNoOp(t *testing.T) {
	committed := committedFeatures()
	if len(generatedFeatures) != len(committed) {
		t.Fatalf("generatedFeatures has %d flags, committed has %d", len(generatedFeatures), len(committed))
	}
	for k, v := range committed {
		if generatedFeatures[k] != v {
			t.Errorf("generatedFeatures[%q] = %v, want committed %v", k, generatedFeatures[k], v)
		}
	}
}

// TestGqlFeatures_ReturnsCopy proves the returned map is a fresh copy: mutating it
// (as a per-op feature override might) never corrupts the shared package var.
func TestGqlFeatures_ReturnsCopy(t *testing.T) {
	orig := generatedFeatures
	t.Cleanup(func() { generatedFeatures = orig })
	generatedFeatures = map[string]any{"flag_a": true}

	got := gqlFeatures()
	got["flag_a"] = false
	got["injected"] = true

	if generatedFeatures["flag_a"] != true {
		t.Error("mutating the returned map corrupted generatedFeatures[flag_a]")
	}
	if _, ok := generatedFeatures["injected"]; ok {
		t.Error("inserting into the returned map leaked into generatedFeatures")
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
	origGen, origGenOK := generatedQueryIDs[op]
	origEp := Endpoints[op]
	t.Cleanup(func() {
		if origGenOK {
			generatedQueryIDs[op] = origGen
		} else {
			delete(generatedQueryIDs, op)
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
		generatedQueryIDs[op] = genID
		t.Setenv(envKey, envID)
		resetAndApply()
		assert.Equal(t, envID, Endpoints[op].ID)
	})

	t.Run("generated beats committed", func(t *testing.T) {
		generatedQueryIDs[op] = genID
		t.Setenv(envKey, "")
		resetAndApply()
		assert.Equal(t, genID, Endpoints[op].ID)
	})

	t.Run("committed when neither generated nor env present", func(t *testing.T) {
		delete(generatedQueryIDs, op)
		t.Setenv(envKey, "")
		resetAndApply()
		assert.Equal(t, committed, Endpoints[op].ID)
	})
}
