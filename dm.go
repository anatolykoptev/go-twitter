package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/anatolykoptev/go-twitter/internal/parse"
)

// dmInboxParams is the documented query-param set for inbox_initial_state.json.
// Copied verbatim from trevorhobenshield/twitter-api-client constants.dm_params
// (the cited reference) so the request shape matches a proven client. Values are
// the literal strings the web client sends.
var dmInboxParams = map[string]string{
	"context":                            "FETCH_DM_CONVERSATION",
	"include_profile_interstitial_type":  "1",
	"include_blocking":                   "1",
	"include_blocked_by":                 "1",
	"include_followed_by":                "1",
	"include_want_retweets":              "1",
	"include_mute_edge":                  "1",
	"include_can_dm":                     "1",
	"include_can_media_tag":              "1",
	"include_ext_has_nft_avatar":         "1",
	"include_ext_is_blue_verified":       "1",
	"include_ext_verified_type":          "1",
	"include_ext_profile_image_shape":    "1",
	"skip_status":                        "1",
	"dm_secret_conversations_enabled":    "false",
	"krs_registration_enabled":           "true",
	"cards_platform":                     "Web-12",
	"include_cards":                      "1",
	"include_ext_alt_text":               "true",
	"include_ext_limited_action_results": "false",
	"include_quote_count":                "true",
	"include_reply_count":                "1",
	"tweet_mode":                         "extended",
	"include_ext_views":                  "true",
	"dm_users":                           "true",
	"include_groups":                     "true",
	"include_inbox_timelines":            "true",
	"include_ext_media_color":            "true",
	"supports_reactions":                 "true",
	"include_conversation_info":          "true",
}

// GetDMInbox fetches the authenticated pool account's DM inbox. The inbox is
// per-account state, so doGET pins a real account from the pool (DMInbox is in
// requiresAuth — no guest fallback). Returns the conversations, each with its
// participants and the messages present in the initial inbox state. An empty
// inbox (0 conversations) is a valid, non-error result.
func (c *Client) GetDMInbox(ctx context.Context) ([]*DMConversation, error) {
	params := url.Values{}
	for k, v := range dmInboxParams {
		params.Set(k, v)
	}
	reqURL := dmInboxURL + "?" + params.Encode()

	body, _, err := c.doGET(ctx, "DMInbox", reqURL)
	if err != nil {
		return nil, fmt.Errorf("DMInbox: %w", err)
	}
	convs, err := parse.DMInbox(body)
	if err != nil {
		return nil, fmt.Errorf("parse DMInbox: %w", err)
	}
	return convs, nil
}

// SendDM sends a text direct message into an existing conversation from a
// specific account (account-pinned write, mirroring CreateTweet/engagement
// mutations). Returns the created message id.
//
// FAIL-CLOSED: a 200 response with no usable message id (empty body, a DM
// validation failure, or a surfaced API error) returns an error — never a silent
// "sent". Mirrors parse.CreateTweet's empty-result guard.
// conversationID is a CONVERSATION id (1:1 form "<recipientID>-<selfID>", per
// twikit), NOT a user id; there is no derive helper — the caller forms it.
func (c *Client) SendDM(ctx context.Context, acc *Account, conversationID, text string) (string, error) {
	if conversationID == "" {
		return "", fmt.Errorf("SendDM: empty conversation_id")
	}
	// Flat new2.json body — CONFIRMED against d60/twikit v11.dm_new.
	payload, err := json.Marshal(map[string]any{
		"cards_platform":      "Web-12",
		"conversation_id":     conversationID,
		"dm_users":            false,
		"include_cards":       1,
		"include_quote_count": true,
		"recipient_ids":       false,
		"text":                text,
	})
	if err != nil {
		return "", fmt.Errorf("marshal SendDM payload: %w", err)
	}

	body, err := c.doPOST(ctx, acc, "SendDM", dmNewURL, payload)
	if err != nil {
		return "", fmt.Errorf("SendDM: %w", err)
	}
	return parse.SendDM(body)
}
