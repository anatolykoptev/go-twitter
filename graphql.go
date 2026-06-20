package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// GetUserByScreenName fetches a user profile by Twitter handle.
func (c *Client) GetUserByScreenName(ctx context.Context, handle string) (*TwitterUser, error) {
	variables := map[string]any{
		"screen_name":              handle,
		"withSafetyModeUserFields": true,
	}
	url, err := EndpointURL("UserByScreenName")
	if err != nil {
		return nil, err
	}
	url = addGraphQLParams(url, variables, Endpoints["UserByScreenName"].Features)

	body, _, err := c.doGET(ctx, "UserByScreenName", url)
	if err != nil {
		return nil, fmt.Errorf("UserByScreenName: %w", err)
	}
	return parseUserByScreenName(body)
}

// GetFollowers fetches followers for a user (paginated).
func (c *Client) GetFollowers(ctx context.Context, userID string, maxCount int) ([]*TwitterUser, error) {
	return c.fetchUserList(ctx, "Followers", userID, maxCount)
}

// GetFollowing fetches accounts a user follows (paginated).
func (c *Client) GetFollowing(ctx context.Context, userID string, maxCount int) ([]*TwitterUser, error) {
	return c.fetchUserList(ctx, "Following", userID, maxCount)
}

// fetchUserList is a generic paginated user list fetcher.
func (c *Client) fetchUserList(ctx context.Context, operation, userID string, maxCount int) ([]*TwitterUser, error) {
	var users []*TwitterUser
	var cursor string

	for {
		select {
		case <-ctx.Done():
			return users, ctx.Err()
		default:
		}

		variables := map[string]any{
			"userId":                 userID,
			"count":                  min(100, maxCount-len(users)),
			"includePromotedContent": false,
			// withGrokTranslatedBio is sent by the live x.com Followers/Following
			// request (captured 2026-06-20). Omitting it together with the legacy
			// feature set returned HTTP 404.
			"withGrokTranslatedBio": true,
		}
		if cursor != "" {
			variables["cursor"] = cursor
		}

		url, err := EndpointURL(operation)
		if err != nil {
			return users, err
		}
		url = addGraphQLParams(url, variables, Endpoints[operation].Features)

		body, _, err := c.doGET(ctx, operation, url)
		if err != nil {
			return users, fmt.Errorf("%s: %w", operation, err)
		}

		batch, nextCursor, err := parseUserList(body)
		if err != nil {
			return users, fmt.Errorf("parse %s: %w", operation, err)
		}
		users = append(users, batch...)

		if nextCursor == "" || len(users) >= maxCount {
			break
		}
		cursor = nextCursor
	}
	return users, nil
}

// GetRetweeters fetches users who retweeted a tweet (paginated).
func (c *Client) GetRetweeters(ctx context.Context, tweetID string, maxCount int) ([]*TwitterUser, error) {
	return c.fetchTweetUserList(ctx, "Retweeters", tweetID, maxCount)
}

// fetchTweetUserList is a paginated user list fetcher for tweet-centric endpoints.
func (c *Client) fetchTweetUserList(ctx context.Context, operation, tweetID string, maxCount int) ([]*TwitterUser, error) {
	var users []*TwitterUser
	var cursor string

	for {
		select {
		case <-ctx.Done():
			return users, ctx.Err()
		default:
		}

		variables := map[string]any{
			"tweetId":                     tweetID,
			"count":                       min(20, maxCount-len(users)),
			"includePromotedContent":      true,
			"withDownvotePerspective":     false,
			"withReactionsMetadata":       false,
			"withReactionsPerspective":    false,
			"withSuperFollowsTweetFields": true,
			"withSuperFollowsUserFields":  true,
			"withVoice":                   true,
			"withBirdwatchNotes":          true,
			"withCommunity":               true,
		}
		if cursor != "" {
			variables["cursor"] = cursor
		}

		url, err := EndpointURL(operation)
		if err != nil {
			return users, err
		}
		url = addGraphQLParams(url, variables, Endpoints[operation].Features)

		body, _, err := c.doGET(ctx, operation, url)
		if err != nil {
			return users, fmt.Errorf("%s: %w", operation, err)
		}

		batch, nextCursor, err := parseRetweeterList(body)
		if err != nil {
			return users, fmt.Errorf("parse %s: %w", operation, err)
		}
		users = append(users, batch...)

		if nextCursor == "" || len(users) >= maxCount {
			break
		}
		cursor = nextCursor
	}
	return users, nil
}

// GetTweetByID fetches a single tweet by its ID.
func (c *Client) GetTweetByID(ctx context.Context, tweetID string) (*Tweet, error) {
	variables := map[string]any{
		"focalTweetId":                           tweetID,
		"with_rux_injections":                    false,
		"rankingMode":                            "Relevance",
		"includePromotedContent":                 true,
		"withCommunity":                          true,
		"withQuickPromoteEligibilityTweetFields": true,
		"withBirdwatchNotes":                     true,
		"withVoice":                              true,
		"withDownvotePerspective":                false,
		"withReactionsMetadata":                  false,
		"withReactionsPerspective":               false,
		"withSuperFollowsTweetFields":            true,
		"withSuperFollowsUserFields":             true,
	}
	url, err := EndpointURL("TweetDetail")
	if err != nil {
		return nil, err
	}
	url = addGraphQLParams(url, variables, Endpoints["TweetDetail"].Features)

	body, _, err := c.doGET(ctx, "TweetDetail", url)
	if err != nil {
		return nil, fmt.Errorf("TweetDetail: %w", err)
	}
	tweets, err := parseTweetDetail(body)
	if err != nil {
		// If parsing fails, log the raw response for debugging
		slog.Debug("TweetDetail parse failed", slog.String("body_prefix", string(body[:min(500, len(body))])))
		return nil, fmt.Errorf("parse TweetDetail: %w", err)
	}
	slog.Debug("TweetDetail parsed", slog.Int("count", len(tweets)), slog.String("target", tweetID))
	for _, t := range tweets {
		slog.Debug("TweetDetail tweet", slog.String("id", t.ID), slog.String("text_prefix", t.Text[:min(50, len(t.Text))]))
		if t.ID == tweetID {
			return t, nil
		}
	}
	if len(tweets) > 0 {
		return tweets[0], nil
	}
	// Log raw body prefix to understand why parsing returned empty
	slog.Warn("TweetDetail no tweets", slog.String("body_prefix", string(body[:min(1000, len(body))])))
	return nil, fmt.Errorf("tweet %s not found in response", tweetID)
}

// GetUserTweets fetches recent tweets for a user.
func (c *Client) GetUserTweets(ctx context.Context, userID string, count int) ([]*Tweet, error) {
	variables := map[string]any{
		"userId":                                 userID,
		"count":                                  count,
		"includePromotedContent":                 false,
		"withQuickPromoteEligibilityTweetFields": true,
		"withVoice":                              true,
		"withV2Timeline":                         true,
	}
	url, err := EndpointURL("UserTweets")
	if err != nil {
		return nil, err
	}
	url = addGraphQLParams(url, variables, Endpoints["UserTweets"].Features)

	body, _, err := c.doGET(ctx, "UserTweets", url)
	if err != nil {
		return nil, fmt.Errorf("UserTweets: %w", err)
	}
	return parseTweetTimeline(body, userID)
}

// SearchTimeline searches for tweets matching a query.
// Uses POST (Twitter migrated this endpoint from GET in March 2026).
func (c *Client) SearchTimeline(ctx context.Context, query string, count int) ([]*Tweet, error) {
	variables := map[string]any{
		"rawQuery":    query,
		"count":       count,
		"querySource": "typed_query",
		"product":     "Latest",
	}
	fieldToggles := map[string]any{
		"withArticleRichContentState": false,
	}
	url, err := EndpointURL("SearchTimeline")
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"variables":    variables,
		"features":     Endpoints["SearchTimeline"].Features,
		"fieldToggles": fieldToggles,
	})
	if err != nil {
		return nil, fmt.Errorf("SearchTimeline: marshal payload: %w", err)
	}

	body, _, err := c.doPoolPOST(ctx, "SearchTimeline", url, payload)
	if err != nil {
		return nil, fmt.Errorf("SearchTimeline: %w", err)
	}
	return parseSearchTimeline(body)
}

// CreateTweet posts a tweet from a specific account.
// Returns the tweet ID on success.
func (c *Client) CreateTweet(ctx context.Context, acc *Account, text string) (string, error) {
	variables := map[string]any{
		"tweet_text":              text,
		"dark_request":            false,
		"media":                   map[string]any{"media_entities": []any{}, "possibly_sensitive": false},
		"semantic_annotation_ids": []any{},
	}

	ep := Endpoints["CreateTweet"]
	payload, err := json.Marshal(map[string]any{
		"variables": variables,
		"features":  ep.Features,
		"queryId":   ep.ID,
	})
	if err != nil {
		return "", fmt.Errorf("marshal CreateTweet payload: %w", err)
	}

	body, err := c.doPOST(ctx, acc, "CreateTweet", ep.URL(), payload)
	if err != nil {
		return "", fmt.Errorf("CreateTweet: %w", err)
	}
	return parseCreateTweet(body)
}

// --- T5.5 engagement mutations (account-pinned POST, mirror CreateTweet) ---

// postMutation marshals {variables,features,queryId} for op and issues an
// account-pinned POST via doPOST (NOT doPoolPOST — these mutate a specific
// account's state). It mirrors CreateTweet's payload shape; the engagement
// methods below share it.
func (c *Client) postMutation(ctx context.Context, acc *Account, op string, variables map[string]any) ([]byte, error) {
	ep := Endpoints[op]
	payload, err := json.Marshal(map[string]any{
		"variables": variables,
		"features":  ep.Features,
		"queryId":   ep.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal %s payload: %w", op, err)
	}
	body, err := c.doPOST(ctx, acc, op, ep.URL(), payload)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return body, nil
}

// LikeTweet favorites a tweet from a specific account. Returns nil on the
// "Done" ack, else an error.
func (c *Client) LikeTweet(ctx context.Context, acc *Account, tweetID string) error {
	body, err := c.postMutation(ctx, acc, "FavoriteTweet", map[string]any{"tweet_id": tweetID})
	if err != nil {
		return err
	}
	return parseAck(body, "FavoriteTweet", "favorite_tweet")
}

// UnlikeTweet un-favorites a tweet from a specific account. Returns nil on the
// "Done" ack, else an error.
func (c *Client) UnlikeTweet(ctx context.Context, acc *Account, tweetID string) error {
	body, err := c.postMutation(ctx, acc, "UnfavoriteTweet", map[string]any{"tweet_id": tweetID})
	if err != nil {
		return err
	}
	return parseAck(body, "UnfavoriteTweet", "unfavorite_tweet")
}

// Retweet retweets a tweet from a specific account. Returns the new retweet ID.
func (c *Client) Retweet(ctx context.Context, acc *Account, tweetID string) (string, error) {
	body, err := c.postMutation(ctx, acc, "CreateRetweet", map[string]any{
		"tweet_id":     tweetID,
		"dark_request": false,
	})
	if err != nil {
		return "", err
	}
	return parseCreateRetweet(body)
}

// Unretweet removes a retweet from a specific account. Returns the source tweet ID.
// The DeleteRetweet op keys the target by "source_tweet_id" (the ID of the
// ORIGINAL tweet, not the retweet's own ID) — confirmed live 2026-06-20: sending
// "tweet_id" yields HTTP 422 GRAPHQL_VALIDATION_FAILED ("source_tweet_id must be
// defined"). Matches trevorhobenshield/twitter-api-client + dimdenGD/OldTweetDeck.
func (c *Client) Unretweet(ctx context.Context, acc *Account, tweetID string) (string, error) {
	body, err := c.postMutation(ctx, acc, "DeleteRetweet", map[string]any{
		"source_tweet_id": tweetID,
		"dark_request":    false,
	})
	if err != nil {
		return "", err
	}
	return parseDeleteRetweet(body)
}

// ReplyTweet posts a reply to tweetID from a specific account. Returns the reply
// tweet ID. It routes through the CreateTweet op (reply.in_reply_to_tweet_id set)
// and reuses parseCreateTweet for the result.
func (c *Client) ReplyTweet(ctx context.Context, acc *Account, tweetID, text string) (string, error) {
	body, err := c.postMutation(ctx, acc, "CreateTweet", replyTweetVariables(text, tweetID, nil))
	if err != nil {
		return "", err
	}
	return parseCreateTweet(body)
}

// replyTweetVariables builds CreateTweet variables for a reply: the same
// media-pluggable base as createTweetVariables plus the reply slot pointing at
// the parent tweet. mediaIDs flows into media.media_entities, so a future
// ReplyWithMedia can attach media without changing this shape (plan acceptance #5).
func replyTweetVariables(text, inReplyToTweetID string, mediaIDs []string) map[string]any {
	v := createTweetVariables(text, mediaIDs)
	v["reply"] = map[string]any{
		"in_reply_to_tweet_id":   inReplyToTweetID,
		"exclude_reply_user_ids": []any{},
	}
	return v
}

// tweetPageSize bounds the per-request count for paginated tweet-timeline reads.
// Twitter caps these server-side; 20 mirrors fetchTweetUserList's page size.
const tweetPageSize = 20

// fetchTweetTimeline is the generic cursor-paginated GET fetcher for tweet
// timelines (bookmarks, list, community). It mirrors fetchUserList: build
// variables per page, GET via addGraphQLParams+doGET, parse a batch + bottom
// cursor, loop until the cursor is empty, a page is empty, or maxCount is hit.
// varsFn receives the per-page count and current cursor and returns the variables
// + feature map for that request.
func (c *Client) fetchTweetTimeline(
	ctx context.Context,
	operation string,
	maxCount int,
	varsFn func(count int, cursor string) (variables, features map[string]any),
	parseFn func(body []byte) ([]*Tweet, string, error),
) ([]*Tweet, error) {
	var tweets []*Tweet
	var cursor string

	for {
		select {
		case <-ctx.Done():
			return tweets, ctx.Err()
		default:
		}

		variables, features := varsFn(min(tweetPageSize, maxCount-len(tweets)), cursor)

		url, err := EndpointURL(operation)
		if err != nil {
			return tweets, err
		}
		url = addGraphQLParams(url, variables, features)

		body, _, err := c.doGET(ctx, operation, url)
		if err != nil {
			return tweets, fmt.Errorf("%s: %w", operation, err)
		}

		batch, nextCursor, err := parseFn(body)
		if err != nil {
			return tweets, fmt.Errorf("parse %s: %w", operation, err)
		}
		tweets = append(tweets, batch...)

		// Stop on no cursor, an empty page (defends against a stuck cursor), or
		// reaching the requested ceiling.
		if nextCursor == "" || len(batch) == 0 || len(tweets) >= maxCount {
			break
		}
		cursor = nextCursor
	}
	return tweets, nil
}

// GetBookmarks fetches the authenticated pool account's bookmarked tweets
// (paginated up to maxCount).
func (c *Client) GetBookmarks(ctx context.Context, maxCount int) ([]*Tweet, error) {
	return c.fetchTweetTimeline(ctx, "Bookmarks", maxCount,
		func(count int, cursor string) (map[string]any, map[string]any) {
			variables := map[string]any{
				"count":                  count,
				"includePromotedContent": false,
			}
			if cursor != "" {
				variables["cursor"] = cursor
			}
			return variables, bookmarkFeatures()
		}, parseBookmarks)
}

// GetListTweets fetches the latest tweets in a list (paginated up to maxCount).
func (c *Client) GetListTweets(ctx context.Context, listID string, maxCount int) ([]*Tweet, error) {
	return c.fetchTweetTimeline(ctx, "ListLatestTweetsTimeline", maxCount,
		func(count int, cursor string) (map[string]any, map[string]any) {
			variables := map[string]any{
				"listId": listID,
				"count":  count,
			}
			if cursor != "" {
				variables["cursor"] = cursor
			}
			return variables, Endpoints["ListLatestTweetsTimeline"].Features
		}, parseListTweets)
}

// GetCommunityTweets fetches the latest tweets in a community (paginated up to maxCount).
func (c *Client) GetCommunityTweets(ctx context.Context, communityID string, maxCount int) ([]*Tweet, error) {
	return c.fetchTweetTimeline(ctx, "CommunityTweetsTimeline", maxCount,
		func(count int, cursor string) (map[string]any, map[string]any) {
			variables := map[string]any{
				"communityId":   communityID,
				"count":         count,
				"withCommunity": true,
				"rankingMode":   "Relevance",
			}
			if cursor != "" {
				variables["cursor"] = cursor
			}
			return variables, Endpoints["CommunityTweetsTimeline"].Features
		}, parseCommunityTweets)
}

// GetVerifiedFollowers fetches a user's blue-verified followers (paginated up to
// maxCount). The response shares the followers shape
// (data.user.result.timeline.timeline), so it reuses the existing fetchUserList
// helper + parseUserList parser.
func (c *Client) GetVerifiedFollowers(ctx context.Context, userID string, maxCount int) ([]*TwitterUser, error) {
	return c.fetchUserList(ctx, "BlueVerifiedFollowers", userID, maxCount)
}

// GetHomeTimeline fetches the algorithmic "For you" home timeline.
// POST (pool), mirroring SearchTimeline.
func (c *Client) GetHomeTimeline(ctx context.Context, count int) ([]*Tweet, error) {
	return c.fetchHomeTimeline(ctx, "HomeTimeline", count)
}

// GetHomeLatestTimeline fetches the reverse-chronological "Following" home timeline.
// POST (pool), mirroring SearchTimeline.
func (c *Client) GetHomeLatestTimeline(ctx context.Context, count int) ([]*Tweet, error) {
	return c.fetchHomeTimeline(ctx, "HomeLatestTimeline", count)
}

// fetchHomeTimeline issues a single POST for a home timeline op and parses the
// result. Both home ops share the data.home.home_timeline_urt root.
func (c *Client) fetchHomeTimeline(ctx context.Context, operation string, count int) ([]*Tweet, error) {
	variables := map[string]any{
		"count":                  count,
		"includePromotedContent": false,
		"latestControlAvailable": true,
		"withCommunity":          true,
		"seenTweetIds":           []any{},
	}
	if operation == "HomeTimeline" {
		variables["requestContext"] = "launch"
	}

	ep := Endpoints[operation]
	// queryId omitted: the op is resolved by the URL path, mirroring SearchTimeline.
	payload, err := json.Marshal(map[string]any{
		"variables": variables,
		"features":  ep.Features,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: marshal payload: %w", operation, err)
	}

	body, _, err := c.doPoolPOST(ctx, operation, ep.URL(), payload)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	tweets, _, err := parseHomeTimeline(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", operation, err)
	}
	return tweets, nil
}

// PostWithAccount posts a tweet from a named account (by username).
// Returns the tweet ID on success.
func (c *Client) PostWithAccount(ctx context.Context, username, text string) (string, error) {
	acc := c.AccountByUsername(username)
	if acc == nil {
		return "", fmt.Errorf("account %q not found in pool", username)
	}
	if !acc.IsActive() {
		return "", fmt.Errorf("account %q is not active", username)
	}
	return c.CreateTweet(ctx, acc, text)
}
