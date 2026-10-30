package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// deviceFollowURL is the legacy endpoint serving the merged timeline of new
// tweets from every account the viewer enabled device notifications ("bell")
// for: one feed covering all belled authors.
const deviceFollowURL = "https://x.com/i/api/2/notifications/device_follow.json"

// deviceFollowEndpoint is the rate-limit/metrics key for the REST bell source.
const deviceFollowEndpoint = "DeviceFollow"

// GetDeviceFollowTweets fetches the merged device-notifications timeline via
// the legacy REST endpoint.
func (c *Client) GetDeviceFollowTweets(ctx context.Context, count int) ([]*Tweet, error) {
	u := deviceFollowURL + "?count=" + strconv.Itoa(count)
	body, _, err := c.doGET(ctx, deviceFollowEndpoint, u)
	if err != nil {
		return nil, fmt.Errorf("DeviceFollow: %w", err)
	}
	tweets, err := parseDeviceFollow(body)
	if err != nil {
		return nil, fmt.Errorf("parse DeviceFollow: %w", err)
	}
	c.metrics.recordFreshness(deviceFollowEndpoint, tweets)
	return tweets, nil
}

// GetTweetNotifications fetches the same merged feed via GraphQL
// tweetNotificationsQuery — the fallback when device_follow.json is gone or
// empty. It carries its own endpoint key so a fault on one source cannot
// quarantine the account from the other.
func (c *Client) GetTweetNotifications(ctx context.Context, count int) ([]*Tweet, error) {
	const op = "tweetNotificationsQuery"
	url, err := EndpointURL(op)
	if err != nil {
		return nil, err
	}
	url = addGraphQLParams(url, map[string]any{
		"count": count,
		// Relay persisted variable X's web client injects for this op; the
		// server 422s ("must be defined") when it is absent.
		"__relay_internal__pv__xclientsfeaturesappviewerisloggedinrelayprovider": true,
	}, endpointFor(op).Features)
	body, _, err := c.doGET(ctx, op, url)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	tweets, _, err := parseTweetNotifications(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", op, err)
	}
	c.metrics.recordFreshness(op, tweets)
	return tweets, nil
}

// parseDeviceFollow decodes the globalObjects shape: device_follow.json
// answers like the 1.5 API, with all tweets under globalObjects.tweets and
// their authors under globalObjects.users keyed by user id.
func parseDeviceFollow(body []byte) ([]*Tweet, error) {
	var raw struct {
		GlobalObjects struct {
			Tweets map[string]struct {
				IDStr     string `json:"id_str"`
				FullText  string `json:"full_text"`
				Text      string `json:"text"`
				UserIDStr string `json:"user_id_str"`
			} `json:"tweets"`
			Users map[string]struct {
				ScreenName string `json:"screen_name"`
				Name       string `json:"name"`
			} `json:"users"`
		} `json:"globalObjects"`
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal device_follow: %w", err)
	}
	if len(raw.Errors) > 0 {
		return nil, fmt.Errorf("twitter API error %d: %s", raw.Errors[0].Code, raw.Errors[0].Message)
	}
	tweets := make([]*Tweet, 0, len(raw.GlobalObjects.Tweets))
	for _, lt := range raw.GlobalObjects.Tweets {
		if lt.IDStr == "" {
			continue
		}
		text := lt.FullText
		if text == "" {
			text = lt.Text
		}
		t := &Tweet{ID: lt.IDStr, AuthorID: lt.UserIDStr, Text: text}
		if u, ok := raw.GlobalObjects.Users[lt.UserIDStr]; ok {
			t.AuthorHandle = u.ScreenName
			t.AuthorName = u.Name
		}
		tweets = append(tweets, t)
	}
	return tweets, nil
}

// parseTweetNotifications parses the tweetNotificationsQuery response.
// Root: data.viewer_v2.user_results.result.tweet_notification_timeline.timeline
// — a standard URT timeline, so extraction reuses the shared machinery (the
// recursive fallback in timelineTweetParse covers a rotated wrapper key).
func parseTweetNotifications(body []byte) ([]*Tweet, string, error) {
	return timelineTweetParse(body, "tweetNotificationsQuery", func(data json.RawMessage) timelineObj {
		var r struct {
			ViewerV2 struct {
				UserResults struct {
					Result struct {
						TweetNotificationTimeline struct {
							Timeline timelineObj `json:"timeline"`
						} `json:"tweet_notification_timeline"`
					} `json:"result"`
				} `json:"user_results"`
			} `json:"viewer_v2"`
		}
		_ = json.Unmarshal(data, &r)
		return r.ViewerV2.UserResults.Result.TweetNotificationTimeline.Timeline
	})
}
