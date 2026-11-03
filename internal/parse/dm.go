package parse

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/anatolykoptev/go-twitter/model"
)

// --- DM parsers (bespoke, NOT timeline-shaped) ---

// DMInbox walks the 1.1 inbox_initial_state JSON: it builds one
// DMConversation per entry in inbox_initial_state.conversations (with participant
// IDs), then attaches each inbox message (entries[].message.message_data) to its
// conversation by conversation_id. Conversations are returned sorted by ID for
// determinism. Surfaces any errors[] field. An empty inbox (no conversations) is
// a valid, non-error result; a malformed response (no inbox_initial_state and no
// error) is an error.
func DMInbox(body []byte) ([]*model.DMConversation, error) {
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
		return nil, fmt.Errorf("DM inbox missing inbox_initial_state: %s", TruncateBytes(body, 300))
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

	byID := make(map[string]*model.DMConversation, len(state.Conversations))
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
		byID[id] = &model.DMConversation{ConversationID: id, Participants: participants}
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
			conv = &model.DMConversation{ConversationID: md.ConversationID}
			byID[md.ConversationID] = conv
		}
		conv.Messages = append(conv.Messages, model.DMMessage{
			ID:             md.ID,
			ConversationID: md.ConversationID,
			SenderID:       md.SenderID,
			Text:           md.Text,
			CreatedAt:      parseDMTime(md.Time),
		})
	}

	out := make([]*model.DMConversation, 0, len(byID))
	for _, conv := range byID {
		out = append(out, conv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConversationID < out[j].ConversationID })
	return out, nil
}

// SendDM extracts the created message id from a new2.json response. The
// response is the inbox-event shape: entries[].message.message_data.id (twikit
// reads message_data here); an event envelope is also tolerated. FAIL-CLOSED:
// surfaces errors[], a dm_validation_failure_type, or an empty id as an error.
func SendDM(body []byte) (string, error) {
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
	return "", fmt.Errorf("SendDM returned no message id: %s", TruncateBytes(body, 300))
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
		// Sorted keys: the first hit must not depend on map iteration order.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if r := findStringKey(t[k], key); r != "" {
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
