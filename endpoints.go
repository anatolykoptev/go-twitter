package twitter

import "fmt"

const (
	twitterBase   = "https://x.com/i/api/graphql"
	twitterAPIURL = "https://api.twitter.com"
)

// bearerTokens is the list of known Twitter web-app bearer tokens.
var bearerTokens = []string{
	"AAAAAAAAAAAAAAAAAAAAANRILgAAAAAAnNwIzUejRCOuH5E6I8xnZz4puTs%3D1Zv7ttfk8LF81IUq16cHjhLTvJu4FA33AGWWjCpTnA",
	"AAAAAAAAAAAAAAAAAAAAAFQODgEAAAAAVHTp76lzh3rFzcHbmHVvQxYYpTw%3DckAlMINMjmCwxUcaXbAN4XqJVdgMJaHqNOFgPMK0zN1qLqLQCF",
}

// BearerToken is the active bearer token (first in list).
var BearerToken = bearerTokens[0]

// Endpoint holds the operation ID, path template, and per-operation feature flags.
type Endpoint struct {
	ID       string
	Name     string
	Features map[string]any
}

// URL returns the full URL for this endpoint.
func (e Endpoint) URL() string {
	return fmt.Sprintf("%s/%s/%s", twitterBase, e.ID, e.Name)
}

// EndpointURL returns the URL for a named operation, or an error if unknown.
func EndpointURL(operation string) (string, error) {
	ep, ok := Endpoints[operation]
	if !ok {
		return "", fmt.Errorf("unknown operation: %s", operation)
	}
	return ep.URL(), nil
}

// Endpoints maps operation names to their current GraphQL IDs and feature flags.
var Endpoints = map[string]Endpoint{
	"UserByScreenName": {ID: "sLVLhk0bGj3MVFEKTdax1w", Name: "UserByScreenName", Features: gqlFeatures()},
	"UserByRestId":     {ID: "GazOglcBvgLigl3ywt6b3Q", Name: "UserByRestId", Features: gqlFeatures()},
	"Followers":        {ID: "pd8Tt1qUz1YWrICegqZ8cw", Name: "Followers", Features: gqlFeatures()},
	"Following":        {ID: "wjvx62Hye2dGVvnvVco0xA", Name: "Following", Features: gqlFeatures()},
	"UserTweets":       {ID: "HuTx74BxAnezK1gWvYY7zg", Name: "UserTweets", Features: gqlFeatures()},
	"SearchTimeline":   {ID: "nK1dw4oV3k4w5TdtcAdSww", Name: "SearchTimeline", Features: gqlFeatures()},
	"TweetDetail":      {ID: "zXaXQgfyR4GxE21uwYQSyA", Name: "TweetDetail", Features: gqlFeatures()},
	"Retweeters":       {ID: "0BoJlKAxoNPQUHRftlwZ2w", Name: "Retweeters", Features: gqlFeatures()},
	"CreateTweet":      {ID: "7TKRKCPuAGsmYde0CudbVg", Name: "CreateTweet", Features: gqlFeatures()},
}

// gqlFeatures returns the canonical Twitter GraphQL feature flags.
func gqlFeatures() map[string]any {
	return map[string]any{
		"articles_preview_enabled":                                                false,
		"c9s_tweet_anatomy_moderator_badge_enabled":                               true,
		"communities_web_enable_tweet_community_results_fetch":                    true,
		"creator_subscriptions_quote_tweet_preview_enabled":                       false,
		"creator_subscriptions_tweet_preview_api_enabled":                         true,
		"freedom_of_speech_not_reach_fetch_enabled":                               true,
		"graphql_is_translatable_rweb_tweet_is_translatable_enabled":              true,
		"longform_notetweets_consumption_enabled":                                 true,
		"longform_notetweets_inline_media_enabled":                                true,
		"longform_notetweets_rich_text_read_enabled":                              true,
		"premium_content_api_read_enabled":                                        false,
		"profile_label_improvements_pcf_label_in_post_enabled":                   false,
		"responsive_web_edit_tweet_api_enabled":                                   true,
		"responsive_web_enhance_cards_enabled":                                    false,
		"responsive_web_graphql_exclude_directive_enabled":                        true,
		"responsive_web_graphql_skip_user_profile_image_extensions_enabled":       false,
		"responsive_web_graphql_timeline_navigation_enabled":                      true,
		"responsive_web_grok_analyze_button_fetch_trends_enabled":                 false,
		"responsive_web_grok_analyze_post_followups_enabled":                      false,
		"responsive_web_grok_image_annotation_enabled":                            false,
		"responsive_web_grok_share_attachment_enabled":                            false,
		"responsive_web_media_download_video_enabled":                             false,
		"responsive_web_twitter_article_tweet_consumption_enabled":                true,
		"rweb_tipjar_consumption_enabled":                                         true,
		"rweb_video_timestamps_enabled":                                           true,
		"standardized_nudges_misinfo":                                             true,
		"tweet_awards_web_tipping_enabled":                                        false,
		"tweet_with_visibility_results_prefer_gql_limited_actions_policy_enabled": true,
		"tweet_with_visibility_results_prefer_gql_media_interstitial_enabled":     false,
		"tweetypie_unmention_optimization_enabled":                                true,
		"verified_phone_label_enabled":                                            false,
		"view_counts_everywhere_api_enabled":                                      true,
	}
}
