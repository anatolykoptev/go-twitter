package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const (
	followURL            = "https://x.com/i/api/1.1/friendships/create.json"
	friendshipsUpdateURL = "https://x.com/i/api/1.1/friendships/update.json"
	friendshipsShowURL   = "https://x.com/i/api/1.1/friendships/show.json"
)

// FollowUser follows userID from acc through the REST 1.1 friendships/create
// endpoint. It is a write: one attempt, pinned to acc, no retry and no
// relogin, so a failure surfaces to the caller instead of repeating.
func (c *Client) FollowUser(ctx context.Context, acc *Account, userID string) error {
	bc, err := c.clientForAccount(acc)
	if err != nil {
		return err
	}
	authTok, ct0, ua := acc.Credentials()
	headers := twitterHeaders(authTok, ct0, ua)
	headers["content-type"] = "application/x-www-form-urlencoded"
	form := url.Values{"user_id": {userID}, "skip_status": {"1"}}.Encode()
	body, respHdrs, status, err := c.observedRequestWithBody(ctx, "FollowUser", bc, "POST", followURL, headers, strings.NewReader(form))
	if err != nil {
		return fmt.Errorf("FollowUser: %w", err)
	}
	c.observeRateLimit(acc, "FollowUser", respHdrs, false)
	return parseFollowResponse(status, body)
}

// parseFollowResponse maps a friendships/create response to an error. 200 with
// the followed user's object is success; anything else reports X's error entry.
func parseFollowResponse(status int, body []byte) error {
	var raw struct {
		IDStr  string `json:"id_str"`
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(body, &raw)
	if len(raw.Errors) > 0 {
		return fmt.Errorf("FollowUser: HTTP %d: X error %d: %s", status, raw.Errors[0].Code, raw.Errors[0].Message)
	}
	if status != 200 || raw.IDStr == "" {
		return fmt.Errorf("FollowUser: HTTP %d: %s", status, truncateBytes(body, 200))
	}
	return nil
}

// FriendshipState reports acc's relationship to userID — whether acc follows
// it and whether device notifications are on — read from one
// friendships/show.json call (target_id only; the source is the authenticating
// account). Same one-shot pinned-account pattern as FollowUser: no retry, no
// relogin. It is the per-target alternative to the GetFollowing list when that
// GraphQL op's rate window is contended by co-tenants.
func (c *Client) FriendshipState(ctx context.Context, acc *Account, userID string) (following, notificationsEnabled bool, err error) {
	bc, err := c.clientForAccount(acc)
	if err != nil {
		return false, false, err
	}
	authTok, ct0, ua := acc.Credentials()
	headers := twitterHeaders(authTok, ct0, ua)
	u := friendshipsShowURL + "?target_id=" + url.QueryEscape(userID)
	body, respHdrs, status, err := c.observedRequest(ctx, "FriendshipState", bc, "GET", u, headers)
	if err != nil {
		return false, false, fmt.Errorf("FriendshipState: %w", err)
	}
	c.observeRateLimit(acc, "FriendshipState", respHdrs, false)
	src, err := parseRelationship(status, body, "FriendshipState")
	return src.Following, src.NotificationsEnabled, err
}

// SetDeviceNotifications turns acc's device notifications for userID on or
// off via friendships/update.json (the "bell" toggle) and returns the
// notifications_enabled flag it reads back from the response. It is a write:
// one attempt, pinned to acc, no retry and no relogin, so a failure surfaces
// to the caller instead of repeating. The readback is advisory only — X has
// answered notifications_enabled=false on a write that did land; verify the
// effect with FriendshipState when it matters.
func (c *Client) SetDeviceNotifications(ctx context.Context, acc *Account, userID string, on bool) (bool, error) {
	bc, err := c.clientForAccount(acc)
	if err != nil {
		return false, err
	}
	authTok, ct0, ua := acc.Credentials()
	headers := twitterHeaders(authTok, ct0, ua)
	headers["content-type"] = "application/x-www-form-urlencoded"
	device := "false"
	if on {
		device = "true"
	}
	form := url.Values{"id": {userID}, "device": {device}}.Encode()
	body, respHdrs, status, err := c.observedRequestWithBody(ctx, "SetDeviceNotifications", bc, "POST", friendshipsUpdateURL, headers, strings.NewReader(form))
	if err != nil {
		return false, fmt.Errorf("SetDeviceNotifications: %w", err)
	}
	c.observeRateLimit(acc, "SetDeviceNotifications", respHdrs, false)
	src, err := parseRelationship(status, body, "SetDeviceNotifications")
	return src.NotificationsEnabled, err
}

// relationshipSource is the source side of a friendships/{show,update}
// response relationship.
type relationshipSource struct {
	Following            bool `json:"following"`
	NotificationsEnabled bool `json:"notifications_enabled"`
}

// parseRelationship reads the source relationship out of a friendships/show
// or friendships/update response (same shape for both). Errors[] wins, then a
// 200 must carry the relationship; anything else is an error.
func parseRelationship(status int, body []byte, op string) (relationshipSource, error) {
	var raw struct {
		Relationship *struct {
			Source relationshipSource `json:"source"`
		} `json:"relationship"`
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(body, &raw)
	if len(raw.Errors) > 0 {
		return relationshipSource{}, fmt.Errorf("%s: HTTP %d: X error %d: %s", op, status, raw.Errors[0].Code, raw.Errors[0].Message)
	}
	if status != 200 || raw.Relationship == nil {
		return relationshipSource{}, fmt.Errorf("%s: HTTP %d: %s", op, status, truncateBytes(body, 200))
	}
	return raw.Relationship.Source, nil
}
