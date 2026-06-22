package twitter

import (
	"testing"
	"time"
)

// --- Fix 1: NO proactive mid-session ct0 rotation ---------------------------

// TestNoProactiveRandomRotation_AgedSessionKeepsServerCT0 reproduces the
// production killer: an account loaded with a valid, server-issued ct0 whose
// ct0 has aged past ct0MaxAge MUST NOT have that ct0 replaced by a client-random
// value. For an ESTABLISHED session x.com validates ct0 server-side, so a
// self-generated ct0 can never match and yields CSRF error 353 forever.
//
// This drives the exact pre-request decision request.go makes (the
// CT0Age()>ct0MaxAge check) and asserts the credential the next request would
// send is the still-valid SERVER ct0, not a fresh random one. Before the fix the
// proactive block called acc.RotateCT0() here and the assertion fails (the ct0
// is a random GenerateCT0 value); after the fix the block is gone and the server
// ct0 survives.
func TestNoProactiveRandomRotation_AgedSessionKeepsServerCT0(t *testing.T) {
	const serverCT0 = "serverissuedct0abcdef0123456789"
	acc := &Account{Username: "u", AuthToken: "auth", CT0: serverCT0}
	// Age the ct0 well past the rotation threshold, exactly as a 4h+ uptime does.
	acc.mu.Lock()
	acc.ct0RefreshedAt = time.Now().Add(-2 * ct0MaxAge)
	acc.mu.Unlock()

	// shouldProactivelyRotate is the gate request.go consults before each request.
	// The fix makes it always false: an established session is never client-rotated.
	if shouldProactivelyRotate(acc) {
		t.Fatal("aged established session must NOT be proactively rotated to a random ct0 (CSRF-353 killer)")
	}

	// The credential the next request would carry must still be the server ct0.
	_, ct0, _ := acc.Credentials()
	if ct0 != serverCT0 {
		t.Fatalf("aged session ct0 mutated to %q; must keep server ct0 %q", ct0, serverCT0)
	}
}

// --- Fix 2: CSRF-353 recovery adopts SERVER ct0, never random ---------------

// TestCSRFRecoveryCT0_AdoptsServerCookie proves the 353-recovery decision adopts
// the ct0 x.com offers in the 353 response set-cookie header, rather than
// generating a random one. Rotating to a random ct0 on a 353 is proven futile
// (the controller reproduced it live).
func TestCSRFRecoveryCT0_AdoptsServerCookie(t *testing.T) {
	const serverCT0 = "freshservercT0deadbeef9876543210"
	hdrs := map[string]string{"set-cookie": "ct0=" + serverCT0 + "; Path=/; Secure"}

	got, ok := csrfRecoveryCT0(hdrs)
	if !ok {
		t.Fatal("expected to adopt server ct0 from 353 set-cookie")
	}
	if got != serverCT0 {
		t.Fatalf("adopted ct0 = %q, want server ct0 %q", got, serverCT0)
	}
}

// TestCSRFRecoveryCT0_NoServerCookieFallsThrough proves that when x.com offers no
// ct0 to adopt, the recovery reports (_, false) so the caller falls through to
// relogin instead of inventing a random ct0 that can never validate.
func TestCSRFRecoveryCT0_NoServerCookieFallsThrough(t *testing.T) {
	for _, name := range []string{"absent", "no-ct0", "malformed"} {
		t.Run(name, func(t *testing.T) {
			var hdrs map[string]string
			switch name {
			case "absent":
				hdrs = map[string]string{}
			case "no-ct0":
				hdrs = map[string]string{"set-cookie": "guest_id=v1%3A123; Path=/"}
			case "malformed":
				hdrs = map[string]string{"set-cookie": "ct0=bad\r\nvalue; Path=/"}
			}
			if got, ok := csrfRecoveryCT0(hdrs); ok {
				t.Fatalf("expected fall-through (no adoptable server ct0), got %q", got)
			}
		})
	}
}
