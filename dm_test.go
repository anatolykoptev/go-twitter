package twitter

import (
	"strings"
	"testing"
)

// NOTE: These are inline-JSON parser/shape tests only. Live DM read/send is
// DEFERRED — it reads and mutates real account state and needs a throwaway
// account, so it is not exercised here (see PR body). Do not add live DM calls.

func TestParseDMInbox_Success(t *testing.T) {
	body := `{
		"inbox_initial_state": {
			"conversations": {
				"111-222": {
					"conversation_id": "111-222",
					"type": "ONE_TO_ONE",
					"participants": [
						{"user_id": "111"},
						{"user_id": "222"}
					]
				}
			},
			"entries": [
				{
					"message": {
						"id": "900001",
						"conversation_id": "111-222",
						"message_data": {
							"id": "900001",
							"conversation_id": "111-222",
							"sender_id": "111",
							"text": "hello there",
							"time": "1700000000000"
						}
					}
				},
				{
					"conversation_read": {"conversation_id": "111-222", "last_read_event_id": "900001"}
				}
			]
		}
	}`

	convs, err := parseDMInbox([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(convs))
	}
	c := convs[0]
	if c.ConversationID != "111-222" {
		t.Fatalf("expected conversation id 111-222, got %s", c.ConversationID)
	}
	if len(c.Participants) != 2 || c.Participants[0] != "111" || c.Participants[1] != "222" {
		t.Fatalf("unexpected participants: %#v", c.Participants)
	}
	if len(c.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(c.Messages))
	}
	m := c.Messages[0]
	if m.ID != "900001" {
		t.Fatalf("expected message id 900001, got %s", m.ID)
	}
	if m.Text != "hello there" {
		t.Fatalf("expected text 'hello there', got %q", m.Text)
	}
	if m.SenderID != "111" {
		t.Fatalf("expected sender 111, got %s", m.SenderID)
	}
	if m.CreatedAt.IsZero() {
		t.Fatal("expected non-zero CreatedAt from unix-millis time")
	}
}

func TestParseDMInbox_Error(t *testing.T) {
	body := `{"errors": [{"code": 32, "message": "Could not authenticate you"}]}`
	_, err := parseDMInbox([]byte(body))
	if err == nil {
		t.Fatal("expected error for error-shaped inbox response")
	}
	if !strings.Contains(err.Error(), "Could not authenticate") {
		t.Fatalf("expected surfaced API error, got %v", err)
	}
}

func TestParseDMInbox_Empty(t *testing.T) {
	body := `{"inbox_initial_state": {"conversations": {}, "entries": []}}`
	convs, err := parseDMInbox([]byte(body))
	if err != nil {
		t.Fatalf("empty inbox must NOT error, got %v", err)
	}
	if len(convs) != 0 {
		t.Fatalf("expected 0 conversations for empty inbox, got %d", len(convs))
	}
}

func TestParseSendDM_Success(t *testing.T) {
	// new2.json success shape — inbox-event entries (mirrors twikit v11.dm_new).
	body := `{
		"entries": [
			{
				"message": {
					"id": "955500",
					"conversation_id": "111-222",
					"message_data": {
						"id": "955500",
						"conversation_id": "111-222",
						"sender_id": "222",
						"text": "sent it"
					}
				}
			}
		],
		"users": {"222": {"id_str": "222"}}
	}`
	id, err := parseSendDM([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if id != "955500" {
		t.Fatalf("expected created message id 955500, got %s", id)
	}
}

func TestParseSendDM_Error(t *testing.T) {
	body := `{"errors": [{"code": 349, "message": "You cannot send messages to this user"}]}`
	_, err := parseSendDM([]byte(body))
	if err == nil {
		t.Fatal("expected error for error-shaped send response")
	}
	if !strings.Contains(err.Error(), "cannot send messages") {
		t.Fatalf("expected surfaced API error, got %v", err)
	}
}

func TestParseSendDM_ValidationFailure(t *testing.T) {
	// A DM rejected for policy reasons carries dm_validation_failure_type — must
	// fail closed even if the envelope is otherwise 200-shaped.
	body := `{"entries": [{"message": {"message_data": {"dm_validation_failure_type": "CANNOT_SEND_TO_NON_FOLLOWER"}}}]}`
	_, err := parseSendDM([]byte(body))
	if err == nil {
		t.Fatal("expected error on dm_validation_failure_type")
	}
	if !strings.Contains(err.Error(), "dm_validation_failure_type") {
		t.Fatalf("expected validation-failure error, got %v", err)
	}
}

func TestParseSendDM_EmptyEvent(t *testing.T) {
	// The silent-no-op trap for a WRITE: a 200 with no message id must error,
	// never return a silent success.
	for _, body := range []string{
		`{}`,
		`{"entries": []}`,
		`{"event": {}}`,
	} {
		if id, err := parseSendDM([]byte(body)); err == nil {
			t.Fatalf("expected error for empty send response %q, got id %q", body, id)
		}
	}
}

func TestRequiresAuth_DM(t *testing.T) {
	if !requiresAuth("DMInbox") {
		t.Fatal("DMInbox must require auth (no guest fallback)")
	}
	if !requiresAuth("SendDM") {
		t.Fatal("SendDM must require auth (no guest fallback)")
	}
}
