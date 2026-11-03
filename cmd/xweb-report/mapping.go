package main

// opSpec describes how one operation go-twitter issues (a key of
// twitter.Endpoints) is expected to appear in the x-web stack.
//
// The legacy side (variables, response root, parser) is transcribed from the
// call sites in graphql.go / devicefollow.go and the parsers in internal/parse; the
// completeness test fails when an Endpoints key has no row here, so a newly
// added operation cannot silently escape the report.
type opSpec struct {
	// Legacy is the twitter.Endpoints key.
	Legacy string
	// XWeb lists candidate Relay operation names, best first. The first one
	// found in the crawl is compared; later ones are reported as alternatives.
	XWeb []string
	// Parser is the go-twitter function that decodes the legacy response.
	Parser string
	// LegacyVars are the GraphQL variables go-twitter sends today (cursor, when
	// paginated, is listed too).
	LegacyVars []string
	// LegacyRoot is the response path under `data` the parser reads, dotted.
	LegacyRoot string
	// Note is a free-form caveat shown in the details.
	Note string
}

// legacyUserList / legacyTweetTimeline keep the table readable.
var (
	userListVars   = []string{"userId", "count", "includePromotedContent", "cursor"}
	userListRoot   = "user.result.timeline.timeline"
	tweetTimelineV = []string{"count", "includePromotedContent", "cursor"}
)

var specs = []opSpec{
	{Legacy: "UserByScreenName", XWeb: []string{"UserByScreenName"}, Parser: "parse.UserByScreenName",
		LegacyVars: []string{"screen_name", "withSafetyModeUserFields"}, LegacyRoot: "user.result"},
	{Legacy: "UserByRestId", XWeb: []string{"UserByRestId", "UserResultByRestId"}, Parser: "(declared; no call site in v0.7.0)",
		LegacyVars: nil, LegacyRoot: "",
		Note: "Endpoints lists it but nothing calls it. The closest x-web artifacts are UsersByRestIds (batch) and userIdRedirectQuery; neither is a drop-in."},
	{Legacy: "Followers", XWeb: []string{"followersQuery", "userFollowersQuery"}, Parser: "parse.UserList",
		LegacyVars: append([]string{"withGrokTranslatedBio"}, userListVars...), LegacyRoot: userListRoot,
		Note: "x-web takes the handle (screenName), not the numeric userId."},
	{Legacy: "Following", XWeb: []string{"followingQuery"}, Parser: "parse.UserList",
		LegacyVars: append([]string{"withGrokTranslatedBio"}, userListVars...), LegacyRoot: userListRoot,
		Note: "x-web takes the handle (screenName), not the numeric userId."},
	{Legacy: "UserTweets", XWeb: []string{"UserTweets"}, Parser: "parse.TweetTimeline",
		LegacyVars: []string{"userId", "count", "includePromotedContent", "withQuickPromoteEligibilityTweetFields", "withVoice", "withV2Timeline", "cursor"},
		LegacyRoot: "user.result.timeline.timeline",
		Note:       "The x-web artifact is the replies tab (profile_with_replies_timeline_v2) and takes screenName; sibling UserOriginalsTimeline / UserRepliesTimeline / UserPhotoTimeline / UserVideoTimeline exist."},
	{Legacy: "SearchTimeline", XWeb: []string{"SearchByRawQuery"}, Parser: "parse.SearchTimeline",
		LegacyVars: []string{"rawQuery", "count", "querySource", "product", "cursor"}, LegacyRoot: "search_by_raw_query.search_timeline.timeline",
		Note: "No operation literally named SearchTimeline; SearchByRawQuery is the same root field name."},
	{Legacy: "TweetDetail", XWeb: []string{"TweetDetail"}, Parser: "parse.TweetDetail",
		LegacyVars: []string{"focalTweetId", "with_rux_injections", "rankingMode", "includePromotedContent", "withCommunity", "withQuickPromoteEligibilityTweetFields", "withBirdwatchNotes", "withVoice", "withDownvotePerspective", "withReactionsMetadata", "withReactionsPerspective", "withSuperFollowsTweetFields", "withSuperFollowsUserFields", "cursor"},
		LegacyRoot: "threaded_conversation_with_injections_v2"},
	{Legacy: "Retweeters", XWeb: []string{"tweetRetweetersQuery", "Retweeters"}, Parser: "parse.RetweeterList",
		LegacyVars: []string{"tweetId", "count", "includePromotedContent", "withDownvotePerspective", "withReactionsMetadata", "withReactionsPerspective", "withSuperFollowsTweetFields", "withSuperFollowsUserFields", "withVoice", "withBirdwatchNotes", "withCommunity", "cursor"},
		LegacyRoot: "retweeters_timeline.timeline"},
	{Legacy: "CreateTweet", XWeb: []string{"createTweetMutation", "CreateTweet"}, Parser: "parse.CreateTweet",
		LegacyVars: []string{"tweet_text", "dark_request", "media", "semantic_annotation_ids", "reply"}, LegacyRoot: "create_tweet.tweet_results.result"},
	{Legacy: "Bookmarks", XWeb: []string{"bookmarksQuery", "Bookmarks"}, Parser: "parse.Bookmarks",
		LegacyVars: tweetTimelineV, LegacyRoot: "bookmark_timeline_v2.timeline"},
	{Legacy: "HomeTimeline", XWeb: []string{"HomeTimelineQuery", "HomeTimeline"}, Parser: "parse.HomeTimeline",
		LegacyVars: []string{"count", "includePromotedContent", "latestControlAvailable", "withCommunity", "seenTweetIds", "requestContext"}, LegacyRoot: "home.home_timeline_urt"},
	{Legacy: "HomeLatestTimeline", XWeb: []string{"HomeLatestTimelineQuery", "HomeLatestTimeline"}, Parser: "parse.HomeTimeline",
		LegacyVars: []string{"count", "includePromotedContent", "latestControlAvailable", "withCommunity", "seenTweetIds"}, LegacyRoot: "home.home_timeline_urt"},
	{Legacy: "ListLatestTweetsTimeline", XWeb: []string{"listTweetsQuery", "ListLatestTweetsTimeline"}, Parser: "parse.ListTweets",
		LegacyVars: []string{"listId", "count", "cursor"}, LegacyRoot: "list.tweets_timeline.timeline"},
	{Legacy: "CommunityTweetsTimeline", XWeb: []string{"communityTimelineQuery", "CommunityTweetsTimeline"}, Parser: "parse.CommunityTweets",
		LegacyVars: []string{"communityId", "count", "withCommunity", "rankingMode", "cursor"}, LegacyRoot: "communityResults.result.ranked_community_timeline.timeline"},
	{Legacy: "BlueVerifiedFollowers", XWeb: []string{"verifiedFollowersQuery", "BlueVerifiedFollowers"}, Parser: "parse.UserList (via fetchUserList)",
		LegacyVars: userListVars, LegacyRoot: userListRoot},
	{Legacy: "tweetNotificationsQuery", XWeb: []string{"tweetNotificationsQuery"}, Parser: "parse.TweetNotifications",
		LegacyVars: []string{"count"}, LegacyRoot: "viewer_v2.user_results.result.tweet_notification_timeline.timeline",
		Note: "Already an x-web operation in v0.7.0 (hand-extracted id, x-web parser); the request also sends six __relay_internal__pv__ provided variables."},
	{Legacy: "FavoriteTweet", XWeb: []string{"likeButtonFavoriteMutation", "FavoriteTweet"}, Parser: "parse.Ack(favorite_tweet)",
		LegacyVars: []string{"tweet_id"}, LegacyRoot: "favorite_tweet"},
	{Legacy: "UnfavoriteTweet", XWeb: []string{"likeButtonUnfavoriteMutation", "UnfavoriteTweet"}, Parser: "parse.Ack(unfavorite_tweet)",
		LegacyVars: []string{"tweet_id"}, LegacyRoot: "unfavorite_tweet"},
	{Legacy: "CreateRetweet", XWeb: []string{"retweetButtonRetweetMutation", "useRetweetMutation", "CreateRetweet"}, Parser: "parse.CreateRetweet",
		LegacyVars: []string{"tweet_id", "dark_request"}, LegacyRoot: "create_retweet.retweet_results.result"},
	{Legacy: "DeleteRetweet", XWeb: []string{"retweetButtonUnretweetMutation", "DeleteRetweet"}, Parser: "parse.DeleteRetweet",
		LegacyVars: []string{"source_tweet_id", "dark_request"}, LegacyRoot: "unretweet.source_tweet_results.result"},
}
