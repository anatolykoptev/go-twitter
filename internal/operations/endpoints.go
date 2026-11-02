// Package operations holds the compiled GraphQL operation data: the committed
// queryIDs and per-operation feature flags, the cmd/gql-sync generated layer
// (queryids_gen.go, features_gen.go) and the TWITTER_QID_* variable names. It
// carries no client state; the layering over it (generated, env, runtime
// refresh) lives in the root package.
package operations

// Operation is one row of the table: the operation ID, its name and the
// per-operation feature flags. The root package's Endpoint has the same fields.
type Operation struct {
	ID       string
	Name     string
	Features map[string]any
}

// Table returns the committed operation table: operation name -> queryID and
// per-operation feature flags, before the generated and env layers are applied
// (the root package layers them, see its init). A fresh map with fresh feature
// maps on every call.
func Table() map[string]Operation {
	return map[string]Operation{
		"UserByScreenName": {ID: "IGgvgiOx4QZndDHuD3x9TQ", Name: "UserByScreenName", Features: CompiledGQLFeatures()},
		"UserByRestId":     {ID: "VQfQ9wwYdk6j_u2O4vt64Q", Name: "UserByRestId", Features: CompiledGQLFeatures()},
		"Followers":        {ID: "9jsVJ9l2uXUIKslHvJqIhw", Name: "Followers", Features: followersFeatures()}, // live-captured 2026-06-20; requires followersFeatures() not gqlFeatures()
		"Following":        {ID: "OLm4oHZBfqWx8jbcEhWoFw", Name: "Following", Features: followersFeatures()}, // live-captured 2026-06-20; same feature set as Followers
		"UserTweets":       {ID: "FOlovQsiHGDls3c0Q_HaSQ", Name: "UserTweets", Features: CompiledGQLFeatures()},
		"SearchTimeline":   {ID: "GcXk9vN_d1jUfHNqLacXQA", Name: "SearchTimeline", Features: CompiledGQLFeatures()},
		"TweetDetail":      {ID: "VWFGPVAGkZMGRKGe3GFFnA", Name: "TweetDetail", Features: CompiledGQLFeatures()},
		"Retweeters":       {ID: "FeoLYPQ-q4bmjGLTZTGs0g", Name: "Retweeters", Features: CompiledGQLFeatures()}, // live-verified 2026-06-20
		"CreateTweet":      {ID: "7TKRKCPuAGsmYde0CudbVg", Name: "CreateTweet", Features: CompiledGQLFeatures()},

		// T5 read cluster. queryIDs below are SEEDS — auto-maintained by gql-sync once
		// the weekly walk covers them (route-split chunks for the home/list/community
		// ops are not reachable from the unauthenticated x.com/home warm page, so the
		// extractor cannot seed them today). Sources cited per op; env override
		// (TWITTER_QID_*) is the always-on operator hotfix.
		"Bookmarks": {ID: "i8QZ1qqy36ffA3bxfTaf7w", Name: "Bookmarks", Features: CompiledGQLFeatures()}, // gql-sync live 2026-06-19 + vladkens/twscrape
		// HomeTimeline / HomeLatestTimeline seeds from trevorhobenshield/twitter-api-client.
		"HomeTimeline":       {ID: "HCosKfLNW1AcOo3la3mMgg", Name: "HomeTimeline", Features: CompiledGQLFeatures()},       // seed — auto-maintained by gql-sync
		"HomeLatestTimeline": {ID: "zhX91JE87mWvfprhYE97xA", Name: "HomeLatestTimeline", Features: CompiledGQLFeatures()}, // seed — auto-maintained by gql-sync
		// List / Community / verified-followers seeds from vladkens/twscrape.
		"ListLatestTweetsTimeline": {ID: "27HKUy8ulrflZ9Tole038g", Name: "ListLatestTweetsTimeline", Features: CompiledGQLFeatures()}, // seed — auto-maintained by gql-sync
		"CommunityTweetsTimeline":  {ID: "Mvs5UOOEkpXVMDZtUcxR-Q", Name: "CommunityTweetsTimeline", Features: CompiledGQLFeatures()},  // seed — auto-maintained by gql-sync
		"BlueVerifiedFollowers":    {ID: "OBBd6Dw-4qEYbsu3hGkyxg", Name: "BlueVerifiedFollowers", Features: CompiledGQLFeatures()},    // seed — auto-maintained by gql-sync
		// Device-notification timeline: fallback source for the device_follow.json
		// feed (tweets from accounts the viewer belled). x-web (Vite/Relay) stack:
		// the ID is params.id of the chunk tweet-notifications-*.js (variables:
		// tweetNotificationsProviders). gql-sync cannot extract it yet (#119); env
		// override is the hotfix path.
		"tweetNotificationsQuery": {ID: "tNwvgqJyhttOF1-7q_v3qA", Name: "tweetNotificationsQuery"},

		// T5.5 engagement mutations (account-pinned POST, mirror CreateTweet).
		//
		// queryIDs here are MANUALLY MAINTAINED. The 2026-06-20 v0.6.1 live arc found
		// the CreateRetweet/DeleteRetweet GraphQL-path queryIDs are NOT exposed in the
		// CURRENT unauthenticated x.com warm-page chunks that gql-sync walks: a full
		// authed-bundle scan surfaced these ops only as Relay persisted-query hashes
		// (e.g. a CreateRetweet Relay hash), which are a DIFFERENT identifier than the
		// GraphQL-path queryID and return HTTP 422 GRAPHQL_VALIDATION_FAILED when used.
		// So gql-sync does not seed these today — NOT by design but by absence from its
		// source. CAUTION: gql-sync has no read-vs-mutation filter (emit.go writes the
		// whole extracted map; CreateTweet, a mutation, is already in queryids_gen.go),
		// and per applyGeneratedOverrides the precedence is env > generated > committed.
		// If a future bundle ever exposes a params:{id,name,operationKind:"mutation"}
		// shape for these ops, gql-sync WILL emit it and override the committed literal
		// below — so verify any generated mutation queryID against a live round-trip.
		// To refresh manually: capture the live GraphQL-path id (network capture or
		// fa0311/TwitterInternalAPIDocument docs/json/API.json — its FavoriteTweet/
		// UnfavoriteTweet IDs match these working seeds, the cross-check), hotfix via
		// the TWITTER_QID_* env override (always-on), and land the new literal here.
		//
		// Features mirror CreateTweet (gqlFeatures()): the reply op IS CreateTweet so it
		// needs the full set; like/retweet tolerate it (twitter-api-client sends the
		// default feature set on these too).
		"FavoriteTweet":   {ID: "lI07N6Otwv1PhnEgXILM7A", Name: "FavoriteTweet", Features: CompiledGQLFeatures()},   // live-verified 2026-06-20; NOT bundle-extractable — env/manual refresh only
		"UnfavoriteTweet": {ID: "ZYKSe-w7KEslx3JhSIk5LA", Name: "UnfavoriteTweet", Features: CompiledGQLFeatures()}, // live-verified 2026-06-20; NOT bundle-extractable — env/manual refresh only
		"CreateRetweet":   {ID: "mbRO74GrOvSfRcJnlMapnQ", Name: "CreateRetweet", Features: CompiledGQLFeatures()},   // live-verified 2026-06-20; NOT bundle-extractable (see mutation note) — env/manual refresh only
		"DeleteRetweet":   {ID: "ZyZigVsNiFO6v1dEks1eWg", Name: "DeleteRetweet", Features: CompiledGQLFeatures()},   // live-verified 2026-06-20; variable is source_tweet_id; NOT bundle-extractable — env/manual refresh only
	}
}

// EnvOverrideKeys maps endpoint names to their env var names for queryId overrides.
var EnvOverrideKeys = map[string]string{
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
	"tweetNotificationsQuery":  "TWITTER_QID_TWEET_NOTIFICATIONS_QUERY",

	// T5.5 engagement mutations.
	"FavoriteTweet":   "TWITTER_QID_FAVORITE_TWEET",
	"UnfavoriteTweet": "TWITTER_QID_UNFAVORITE_TWEET",
	"CreateRetweet":   "TWITTER_QID_CREATE_RETWEET",
	"DeleteRetweet":   "TWITTER_QID_DELETE_RETWEET",
}

// knownManualQueryIDs is the explicit allowlist of Endpoints operations whose
// queryID is NOT extracted by cmd/gql-sync from the public unauthenticated x.com
// bundles. Every entry carries a one-line reason. A new endpoint added to
// Endpoints without either a generated queryID or an entry here fails
// TestQueryIDCompleteness.
var knownManualQueryIDs = map[string]string{
	// T5 read cluster — route-split chunks for these ops are not reachable from
	// the unauthenticated x.com/home warm page, so gql-sync cannot seed them.
	"Bookmarks":                "requires authenticated session; not in the unauthenticated x.com bundles gql-sync walks",
	"HomeTimeline":             "requires authenticated session; not in the unauthenticated x.com bundles gql-sync walks",
	"HomeLatestTimeline":       "requires authenticated session; not in the unauthenticated x.com bundles gql-sync walks",
	"ListLatestTweetsTimeline": "requires authenticated session; not in the unauthenticated x.com bundles gql-sync walks",
	"CommunityTweetsTimeline":  "requires authenticated session; not in the unauthenticated x.com bundles gql-sync walks",
	"BlueVerifiedFollowers":    "requires authenticated session; not in the unauthenticated x.com bundles gql-sync walks",
	"tweetNotificationsQuery":  "requires authenticated session; not in the unauthenticated x.com bundles gql-sync walks",

	// T5.5 engagement mutations — the public bundles carry these only as Relay
	// persisted-query hashes, which are a different identifier and return HTTP 422
	// when used as GraphQL-path queryIDs.
	"FavoriteTweet":   "requires authenticated session; public bundles carry the Relay hash, not the GraphQL-path queryID",
	"UnfavoriteTweet": "requires authenticated session; public bundles carry the Relay hash, not the GraphQL-path queryID",
	"CreateRetweet":   "requires authenticated session; public bundles carry the Relay hash, not the GraphQL-path queryID",
	"DeleteRetweet":   "requires authenticated session; public bundles carry the Relay hash, not the GraphQL-path queryID",
}

// CompiledGQLFeatures is the compiled baseline: the generated set when present,
// else the committed literal. Fresh copy on every call.
func CompiledGQLFeatures() map[string]any {
	if len(generatedFeatures) > 0 {
		out := make(map[string]any, len(generatedFeatures))
		for k, v := range generatedFeatures {
			out[k] = v
		}
		return out
	}
	return committedFeatures()
}

// followersFeatures returns the feature set x.com requires for the Followers and
// Following GraphQL ops as of the 2026-06-20 live capture. These two profile
// list ops validate a DIFFERENT feature set than the shared gqlFeatures()
// baseline: the live browser request OMITS responsive_web_graphql_exclude_directive_enabled
// and INCLUDES rweb_cashtags_enabled / rweb_cashtags_composer_attachment_enabled /
// rweb_conversational_replies_downvote_enabled. Sending the shared baseline
// returns HTTP 404 (feature-set mismatch, NOT a stale queryId alone). Captured
// live via an authenticated /<user>/followers network request. Refresh by
// re-capturing the live Followers request URL.
func followersFeatures() map[string]any {
	return map[string]any{
		"articles_preview_enabled":                                                true,
		"c9s_tweet_anatomy_moderator_badge_enabled":                               true,
		"communities_web_enable_tweet_community_results_fetch":                    true,
		"content_disclosure_ai_generated_indicator_enabled":                       true,
		"content_disclosure_indicator_enabled":                                    true,
		"creator_subscriptions_tweet_preview_api_enabled":                         true,
		"freedom_of_speech_not_reach_fetch_enabled":                               true,
		"graphql_is_translatable_rweb_tweet_is_translatable_enabled":              true,
		"longform_notetweets_consumption_enabled":                                 true,
		"longform_notetweets_inline_media_enabled":                                false,
		"longform_notetweets_rich_text_read_enabled":                              true,
		"post_ctas_fetch_enabled":                                                 false,
		"premium_content_api_read_enabled":                                        false,
		"profile_label_improvements_pcf_label_in_post_enabled":                    true,
		"responsive_web_edit_tweet_api_enabled":                                   true,
		"responsive_web_enhance_cards_enabled":                                    false,
		"responsive_web_graphql_skip_user_profile_image_extensions_enabled":       false,
		"responsive_web_graphql_timeline_navigation_enabled":                      true,
		"responsive_web_grok_analysis_button_from_backend":                        true,
		"responsive_web_grok_analyze_button_fetch_trends_enabled":                 false,
		"responsive_web_grok_analyze_post_followups_enabled":                      true,
		"responsive_web_grok_annotations_enabled":                                 true,
		"responsive_web_grok_community_note_auto_translation_is_enabled":          true,
		"responsive_web_grok_image_annotation_enabled":                            true,
		"responsive_web_grok_imagine_annotation_enabled":                          true,
		"responsive_web_grok_share_attachment_enabled":                            true,
		"responsive_web_grok_show_grok_translated_post":                           true,
		"responsive_web_jetfuel_frame":                                            true,
		"responsive_web_profile_redirect_enabled":                                 false,
		"responsive_web_twitter_article_tweet_consumption_enabled":                true,
		"rweb_cashtags_composer_attachment_enabled":                               true,
		"rweb_cashtags_enabled":                                                   true,
		"rweb_conversational_replies_downvote_enabled":                            false,
		"rweb_tipjar_consumption_enabled":                                         false,
		"rweb_video_screen_enabled":                                               false,
		"standardized_nudges_misinfo":                                             true,
		"tweet_with_visibility_results_prefer_gql_limited_actions_policy_enabled": true,
		"verified_phone_label_enabled":                                            false,
		"view_counts_everywhere_api_enabled":                                      true,
	}
}

// committedFeatures is the hand-maintained baseline feature set, used when no
// generated baseline is present. Extracted from x.com — keep roughly in sync, but
// gql-sync's features_gen.go is the authoritative refresh path.
func committedFeatures() map[string]any {
	return map[string]any{
		"articles_preview_enabled":                                                true,
		"c9s_tweet_anatomy_moderator_badge_enabled":                               true,
		"communities_web_enable_tweet_community_results_fetch":                    true,
		"content_disclosure_ai_generated_indicator_enabled":                       true,
		"content_disclosure_indicator_enabled":                                    true,
		"creator_subscriptions_tweet_preview_api_enabled":                         true,
		"freedom_of_speech_not_reach_fetch_enabled":                               true,
		"graphql_is_translatable_rweb_tweet_is_translatable_enabled":              true,
		"longform_notetweets_consumption_enabled":                                 true,
		"longform_notetweets_inline_media_enabled":                                true,
		"longform_notetweets_rich_text_read_enabled":                              true,
		"post_ctas_fetch_enabled":                                                 true,
		"premium_content_api_read_enabled":                                        false,
		"profile_label_improvements_pcf_label_in_post_enabled":                    true,
		"responsive_web_edit_tweet_api_enabled":                                   true,
		"responsive_web_enhance_cards_enabled":                                    false,
		"responsive_web_graphql_exclude_directive_enabled":                        true,
		"responsive_web_graphql_skip_user_profile_image_extensions_enabled":       false,
		"responsive_web_graphql_timeline_navigation_enabled":                      true,
		"responsive_web_grok_analyze_button_fetch_trends_enabled":                 true,
		"responsive_web_grok_analyze_post_followups_enabled":                      true,
		"responsive_web_grok_analysis_button_from_backend":                        true,
		"responsive_web_grok_annotations_enabled":                                 true,
		"responsive_web_grok_community_note_auto_translation_is_enabled":          true,
		"responsive_web_grok_image_annotation_enabled":                            true,
		"responsive_web_grok_imagine_annotation_enabled":                          true,
		"responsive_web_grok_share_attachment_enabled":                            true,
		"responsive_web_grok_show_grok_translated_post":                           true,
		"responsive_web_jetfuel_frame":                                            true,
		"responsive_web_profile_redirect_enabled":                                 true,
		"responsive_web_twitter_article_tweet_consumption_enabled":                true,
		"rweb_tipjar_consumption_enabled":                                         true,
		"rweb_video_screen_enabled":                                               true,
		"standardized_nudges_misinfo":                                             true,
		"tweet_with_visibility_results_prefer_gql_limited_actions_policy_enabled": true,
		"verified_phone_label_enabled":                                            false,
		"view_counts_everywhere_api_enabled":                                      true,
	}
}
