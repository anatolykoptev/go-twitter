package twitter

import (
	"fmt"
	"net/url"
	"os"
)

const (
	twitterBase   = "https://x.com/i/api/graphql"
	twitterAPIURL = "https://api.twitter.com"

	// accountSettingsURL is the authenticated REST endpoint used by
	// ValidateAccount to check whether an account's credentials are still
	// alive. Returns 200 on success, 401 on expired auth, 403 on suspension.
	accountSettingsURL = "https://api.twitter.com/1.1/account/settings.json"

	// T5.6 DM cluster — 1.1 REST endpoints (same host the lib already uses for
	// auth/media/settings). DMs are NOT GraphQL ops here, so they live as plain
	// URL consts (like accountSettingsURL), not in the Endpoints map.
	//
	// dmInboxURL: GET inbox metadata. CONFIRMED against
	// trevorhobenshield/twitter-api-client Account.dm_inbox (v1_api =
	// "https://api.twitter.com/1.1", path dm/inbox_initial_state.json).
	dmInboxURL = "https://api.twitter.com/1.1/dm/inbox_initial_state.json"
	// dmNewURL: POST a new DM into an existing conversation. CONFIRMED against
	// d60/twikit v11.dm_new (Endpoint.DM_NEW = .../1.1/dm/new2.json), flat JSON
	// body {conversation_id,text,...}. (NOT the public events/new.json shape.)
	dmNewURL = "https://api.twitter.com/1.1/dm/new2.json"
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

// URL returns the full URL for this endpoint. The variable ID segment is
// path-escaped as defense-in-depth: a poisoned queryID must never break out of
// its path segment. e.Name is a hardcoded const, so it is left verbatim.
func (e Endpoint) URL() string {
	return fmt.Sprintf("%s/%s/%s", twitterBase, url.PathEscape(e.ID), e.Name)
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
	"UserByScreenName": {ID: "IGgvgiOx4QZndDHuD3x9TQ", Name: "UserByScreenName", Features: gqlFeatures()},
	"UserByRestId":     {ID: "VQfQ9wwYdk6j_u2O4vt64Q", Name: "UserByRestId", Features: gqlFeatures()},
	"Followers":        {ID: "9jsVJ9l2uXUIKslHvJqIhw", Name: "Followers", Features: followersFeatures()}, // live-captured 2026-06-20; requires followersFeatures() not gqlFeatures()
	"Following":        {ID: "OLm4oHZBfqWx8jbcEhWoFw", Name: "Following", Features: followersFeatures()}, // live-captured 2026-06-20; same feature set as Followers
	"UserTweets":       {ID: "FOlovQsiHGDls3c0Q_HaSQ", Name: "UserTweets", Features: gqlFeatures()},
	"SearchTimeline":   {ID: "GcXk9vN_d1jUfHNqLacXQA", Name: "SearchTimeline", Features: gqlFeatures()},
	"TweetDetail":      {ID: "VWFGPVAGkZMGRKGe3GFFnA", Name: "TweetDetail", Features: gqlFeatures()},
	"Retweeters":       {ID: "FeoLYPQ-q4bmjGLTZTGs0g", Name: "Retweeters", Features: gqlFeatures()}, // live-verified 2026-06-20
	"CreateTweet":      {ID: "7TKRKCPuAGsmYde0CudbVg", Name: "CreateTweet", Features: gqlFeatures()},

	// T5 read cluster. queryIDs below are SEEDS — auto-maintained by gql-sync once
	// the weekly walk covers them (route-split chunks for the home/list/community
	// ops are not reachable from the unauthenticated x.com/home warm page, so the
	// extractor cannot seed them today). Sources cited per op; env override
	// (TWITTER_QID_*) is the always-on operator hotfix.
	"Bookmarks": {ID: "i8QZ1qqy36ffA3bxfTaf7w", Name: "Bookmarks", Features: gqlFeatures()}, // gql-sync live 2026-06-19 + vladkens/twscrape
	// HomeTimeline / HomeLatestTimeline seeds from trevorhobenshield/twitter-api-client.
	"HomeTimeline":       {ID: "HCosKfLNW1AcOo3la3mMgg", Name: "HomeTimeline", Features: gqlFeatures()},       // seed — auto-maintained by gql-sync
	"HomeLatestTimeline": {ID: "zhX91JE87mWvfprhYE97xA", Name: "HomeLatestTimeline", Features: gqlFeatures()}, // seed — auto-maintained by gql-sync
	// List / Community / verified-followers seeds from vladkens/twscrape.
	"ListLatestTweetsTimeline": {ID: "27HKUy8ulrflZ9Tole038g", Name: "ListLatestTweetsTimeline", Features: gqlFeatures()}, // seed — auto-maintained by gql-sync
	"CommunityTweetsTimeline":  {ID: "Mvs5UOOEkpXVMDZtUcxR-Q", Name: "CommunityTweetsTimeline", Features: gqlFeatures()},  // seed — auto-maintained by gql-sync
	"BlueVerifiedFollowers":    {ID: "OBBd6Dw-4qEYbsu3hGkyxg", Name: "BlueVerifiedFollowers", Features: gqlFeatures()},    // seed — auto-maintained by gql-sync

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
	"FavoriteTweet":   {ID: "lI07N6Otwv1PhnEgXILM7A", Name: "FavoriteTweet", Features: gqlFeatures()},   // live-verified 2026-06-20; NOT bundle-extractable — env/manual refresh only
	"UnfavoriteTweet": {ID: "ZYKSe-w7KEslx3JhSIk5LA", Name: "UnfavoriteTweet", Features: gqlFeatures()}, // live-verified 2026-06-20; NOT bundle-extractable — env/manual refresh only
	"CreateRetweet":   {ID: "mbRO74GrOvSfRcJnlMapnQ", Name: "CreateRetweet", Features: gqlFeatures()},   // live-verified 2026-06-20; NOT bundle-extractable (see mutation note) — env/manual refresh only
	"DeleteRetweet":   {ID: "ZyZigVsNiFO6v1dEks1eWg", Name: "DeleteRetweet", Features: gqlFeatures()},   // live-verified 2026-06-20; variable is source_tweet_id; NOT bundle-extractable — env/manual refresh only
}

// envOverrides maps endpoint names to their env var names for queryId overrides.
var envOverrides = map[string]string{
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

	// T5.5 engagement mutations — the public bundles carry these only as Relay
	// persisted-query hashes, which are a different identifier and return HTTP 422
	// when used as GraphQL-path queryIDs.
	"FavoriteTweet":   "requires authenticated session; public bundles carry the Relay hash, not the GraphQL-path queryID",
	"UnfavoriteTweet": "requires authenticated session; public bundles carry the Relay hash, not the GraphQL-path queryID",
	"CreateRetweet":   "requires authenticated session; public bundles carry the Relay hash, not the GraphQL-path queryID",
	"DeleteRetweet":   "requires authenticated session; public bundles carry the Relay hash, not the GraphQL-path queryID",
}

// ApplyEnvOverrides reads TWITTER_QID_* env vars and overrides queryIds in Endpoints.
// Called automatically by init(); can also be called manually in tests.
func ApplyEnvOverrides() {
	for name, envKey := range envOverrides {
		if qid := os.Getenv(envKey); qid != "" {
			if ep, ok := Endpoints[name]; ok {
				ep.ID = qid
				Endpoints[name] = ep
			}
		}
	}
}

// applyGeneratedOverrides applies the cmd/gql-sync-generated queryIDs (in
// queryids_gen.go) over the committed literals in Endpoints. It runs before
// ApplyEnvOverrides so the final priority is env > generated > committed. Ops
// absent from generatedQueryIDs keep their committed literal; empty generated
// IDs are skipped (a partial/blank sync never blanks an endpoint).
//
// It only UPDATES the ID of ops already present in Endpoints; a generated op
// with no committed Endpoints entry is intentionally NOT added. Endpoints is the
// authoritative set of operations this client issues — sync refreshes IDs for
// known ops, it never introduces new endpoints.
func applyGeneratedOverrides() {
	for name, id := range generatedQueryIDs {
		if id == "" {
			continue
		}
		if ep, ok := Endpoints[name]; ok { // intentional: update known ops only, never add
			ep.ID = id
			Endpoints[name] = ep
		}
	}
}

func init() {
	applyGeneratedOverrides() // generated overrides committed literal
	ApplyEnvOverrides()       // env overrides generated (highest priority)
}

// gqlFeatures returns the canonical Twitter GraphQL feature flags. It prefers the
// cmd/gql-sync-generated baseline (features_gen.go) when present, falling back to
// the committed literal when generation has not run (or produced nothing). The
// returned map is always a fresh COPY so a caller mutating per-op features can
// never corrupt the shared package-level source.
func gqlFeatures() map[string]any {
	if len(generatedFeatures) > 0 {
		out := make(map[string]any, len(generatedFeatures))
		for k, v := range generatedFeatures {
			out[k] = v
		}
		return out
	}
	return committedFeatures()
}

// bookmarkFeatures returns the shared baseline feature set plus the
// graphql_timeline_v2_bookmark_timeline flag the Bookmarks op requires (without
// it x.com returns the legacy bookmark shape / a 400). gqlFeatures() already
// returns a fresh copy, so mutating it here never corrupts the shared baseline.
func bookmarkFeatures() map[string]any {
	f := gqlFeatures()
	f["graphql_timeline_v2_bookmark_timeline"] = true
	return f
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
