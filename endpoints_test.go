package twitter

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
