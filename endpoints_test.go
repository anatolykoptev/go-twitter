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
