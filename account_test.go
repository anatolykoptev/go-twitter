package twitter

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestParseAccounts_MalformedEntryNeverLogsSecret proves a malformed account
// entry (one with too few fields to be valid) is skipped WITHOUT its raw
// content reaching the logs — the entry may be, or contain, a password /
// auth_token / ct0 / TOTP seed.
func TestParseAccounts_MalformedEntryNeverLogsSecret(t *testing.T) {
	const secret = "SUPERSECRETtotpSEED9999"

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A single-field entry: len(parts)==1, so the whole thing IS the secret and
	// must be dropped.
	accounts := ParseAccounts(secret + ",gooduser:goodpass")

	logged := buf.String()
	if strings.Contains(logged, secret) {
		t.Fatalf("secret leaked to logs: %q", logged)
	}
	if !strings.Contains(logged, "invalid account entry") {
		t.Fatalf("expected a skip warning, got: %q", logged)
	}

	// The malformed line is skipped; only the valid account survives.
	if len(accounts) != 1 {
		t.Fatalf("len(accounts) = %d, want 1 (bad line skipped)", len(accounts))
	}
	if accounts[0].Username != "gooduser" {
		t.Fatalf("surviving account = %q, want gooduser", accounts[0].Username)
	}
}
