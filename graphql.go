package twitter

import (
	"context"
	"fmt"
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
			"tweetId":                tweetID,
			"count":                  min(20, maxCount-len(users)),
			"includePromotedContent": true,
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
	url = addGraphQLParams(url, variables, Endpoints["SearchTimeline"].Features, fieldToggles)

	body, _, err := c.doGET(ctx, "SearchTimeline", url)
	if err != nil {
		return nil, fmt.Errorf("SearchTimeline: %w", err)
	}
	return parseSearchTimeline(body)
}
