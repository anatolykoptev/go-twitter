package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"
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
	convs, err := parseDMInbox(body)
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
// "sent". Mirrors parseCreateTweet's empty-result guard.
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
	return parseSendDM(body)
}

// --- DM parsers (bespoke, NOT timeline-shaped) ---

// parseDMInbox walks the 1.1 inbox_initial_state JSON: it builds one
// DMConversation per entry in inbox_initial_state.conversations (with participant
// IDs), then attaches each inbox message (entries[].message.message_data) to its
// conversation by conversation_id. Conversations are returned sorted by ID for
// determinism. Surfaces any errors[] field. An empty inbox (no conversations) is
// a valid, non-error result; a malformed response (no inbox_initial_state and no
// error) is an error.
func parseDMInbox(body []byte) ([]*DMConversation, error) {
	var raw struct {
		InboxInitialState json.RawMessage `json:"inbox_initial_state"`
		Errors            []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal DM inbox: %w", err)
	}
	if len(raw.Errors) > 0 {
		return nil, fmt.Errorf("twitter API error: %s", raw.Errors[0].Message)
	}
	if len(raw.InboxInitialState) == 0 || string(raw.InboxInitialState) == "null" {
		return nil, fmt.Errorf("DM inbox missing inbox_initial_state: %s", truncateBytes(body, 300))
	}

	var state struct {
		Conversations map[string]struct {
			ConversationID string `json:"conversation_id"`
			Participants   []struct {
				UserID string `json:"user_id"`
			} `json:"participants"`
		} `json:"conversations"`
		Entries []struct {
			Message struct {
				MessageData struct {
					ID             string `json:"id"`
					ConversationID string `json:"conversation_id"`
					SenderID       string `json:"sender_id"`
					Text           string `json:"text"`
					Time           string `json:"time"`
				} `json:"message_data"`
			} `json:"message"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw.InboxInitialState, &state); err != nil {
		return nil, fmt.Errorf("unmarshal inbox_initial_state: %w", err)
	}

	byID := make(map[string]*DMConversation, len(state.Conversations))
	for key, conv := range state.Conversations {
		id := conv.ConversationID
		if id == "" {
			id = key // the map key is the conversation id
		}
		participants := make([]string, 0, len(conv.Participants))
		for _, p := range conv.Participants {
			if p.UserID != "" {
				participants = append(participants, p.UserID)
			}
		}
		byID[id] = &DMConversation{ConversationID: id, Participants: participants}
	}

	// Attach messages to their conversation; create a bare conversation if an
	// entry references one not present in the conversations map (defensive).
	for _, e := range state.Entries {
		md := e.Message.MessageData
		if md.ID == "" {
			continue // not a message entry (e.g. conversation_read marker)
		}
		if md.ConversationID == "" {
			continue // orphan message, no conversation id — skip (avoid a ""-keyed bare conversation)
		}
		conv, ok := byID[md.ConversationID]
		if !ok {
			conv = &DMConversation{ConversationID: md.ConversationID}
			byID[md.ConversationID] = conv
		}
		conv.Messages = append(conv.Messages, DMMessage{
			ID:             md.ID,
			ConversationID: md.ConversationID,
			SenderID:       md.SenderID,
			Text:           md.Text,
			CreatedAt:      parseDMTime(md.Time),
		})
	}

	out := make([]*DMConversation, 0, len(byID))
	for _, conv := range byID {
		out = append(out, conv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConversationID < out[j].ConversationID })
	return out, nil
}

// parseSendDM extracts the created message id from a new2.json response. The
// response is the inbox-event shape: entries[].message.message_data.id (twikit
// reads message_data here); an event envelope is also tolerated. FAIL-CLOSED:
// surfaces errors[], a dm_validation_failure_type, or an empty id as an error.
func parseSendDM(body []byte) (string, error) {
	var raw struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Entries []struct {
			Message struct {
				ID          string `json:"id"`
				MessageData struct {
					ID string `json:"id"`
				} `json:"message_data"`
			} `json:"message"`
		} `json:"entries"`
		Event struct {
			ID            string `json:"id"`
			MessageCreate struct {
				MessageData struct {
					ID string `json:"id"`
				} `json:"message_data"`
			} `json:"message_create"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("unmarshal SendDM: %w", err)
	}
	// Load-bearing: classifyError whitelists known codes, so DM-specific error
	// codes (e.g. 349) reach here as errNone — this errors[] check is the only
	// thing that catches them. Do NOT remove as "redundant with transport".
	if len(raw.Errors) > 0 {
		return "", fmt.Errorf("SendDM API error: %s", raw.Errors[0].Message)
	}
	if ft := dmValidationFailure(body); ft != "" {
		return "", fmt.Errorf("SendDM rejected: dm_validation_failure_type=%s", ft)
	}

	// Prefer the event envelope id, then the inbox-entry message_data id.
	if id := raw.Event.MessageCreate.MessageData.ID; id != "" {
		return id, nil
	}
	if id := raw.Event.ID; id != "" {
		return id, nil
	}
	for _, e := range raw.Entries {
		if e.Message.MessageData.ID != "" {
			return e.Message.MessageData.ID, nil
		}
		if e.Message.ID != "" {
			return e.Message.ID, nil
		}
	}
	return "", fmt.Errorf("SendDM returned no message id: %s", truncateBytes(body, 300))
}

// parseDMTime parses the 1.1 inbox time field (unix milliseconds as a string)
// into a time.Time. An empty or unparseable value yields the zero time, matching
// the tolerant time handling in parseTweetResult.
func parseDMTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// dmValidationFailure recursively searches a decoded JSON body for a non-empty
// dm_validation_failure_type value (twitter signals a rejected DM with this
// key). Returns the failure type, or "" when absent. Mirrors the find_key check
// trevorhobenshield/twitter-api-client uses after a send.
func dmValidationFailure(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return findStringKey(v, "dm_validation_failure_type")
}

func findStringKey(v any, key string) string {
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t[key].(string); ok && s != "" {
			return s
		}
		for _, child := range t {
			if r := findStringKey(child, key); r != "" {
				return r
			}
		}
	case []any:
		for _, child := range t {
			if r := findStringKey(child, key); r != "" {
				return r
			}
		}
	}
	return ""
}
