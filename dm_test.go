package twitter

import (
	"testing"
)

func TestRequiresAuth_DM(t *testing.T) {
	if !requiresAuth("DMInbox") {
		t.Fatal("DMInbox must require auth (no guest fallback)")
	}
	if !requiresAuth("SendDM") {
		t.Fatal("SendDM must require auth (no guest fallback)")
	}
}
