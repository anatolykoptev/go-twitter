package twitter

import (
	"os"
	"strings"
	"testing"
)

// requestPathFiles are the source files carrying the authenticated request hot
// paths (pool GET/POST, single-account POST, media upload) plus the recovery
// helpers. The client-random ct0 rotation that killed the pool ~4h after boot
// lived here; these guards source-grep the shipped tree so a reintroduction
// fails the suite rather than silently returning in production.
var requestPathFiles = []string{"request.go", "media.go"}

// TestRegression_NoClientRandomCT0OnRequestPath asserts no request-path source
// file calls acc.RotateCT0() — the client-random ct0 generator. On an
// ESTABLISHED session a client-generated ct0 can never validate server-side, so
// any such call guarantees CSRF-353 and the doomed relogin cascade. The
// legitimate refresh is server-driven (SetCT0 from extractCT0FromHeaders); the
// only legitimate client generation is the cookie-absent fallback at session
// establishment (auth.go login()), which is NOT on these paths.
func TestRegression_NoClientRandomCT0OnRequestPath(t *testing.T) {
	for _, f := range requestPathFiles {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(src), "RotateCT0()") {
			t.Errorf("%s calls RotateCT0() on a request path — client-random ct0 against an "+
				"established session forces CSRF-353. Adopt the server ct0 via "+
				"csrfRecoveryCT0/SetCT0 instead.", f)
		}
	}
}

// TestRegression_NoProactiveAgeRotation asserts no request-path file pairs the
// ct0 staleness check with a rotation — the proactive block (CT0Age()>ct0MaxAge
// → RotateCT0) was the trigger that fired exactly ct0MaxAge after boot. The fix
// removed the block entirely; ct0 freshness is maintained only by adopting the
// server's set-cookie on success.
func TestRegression_NoProactiveAgeRotation(t *testing.T) {
	for _, f := range requestPathFiles {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(src), "CT0Age()") {
			t.Errorf("%s references CT0Age() on a request path — the proactive "+
				"age-based rotation was the boot+ct0MaxAge killer and must stay deleted.", f)
		}
	}
}
