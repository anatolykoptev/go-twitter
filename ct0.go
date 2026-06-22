package twitter

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

// ct0Re bounds an extracted ct0 to the alphanumeric token alphabet x.com uses.
// A ct0 is reused verbatim in a request header, so a value carrying CR/LF or
// other control bytes from a hostile set-cookie must be rejected before storage
// (removes reliance on go-stealth for CRLF rejection).
var ct0Re = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// GenerateCT0 generates a random 32-byte hex string for use as a ct0 CSRF token.
func GenerateCT0() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000000000000000000000000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// ct0MaxAge is the staleness horizon for a ct0 token. It no longer drives any
// client-side rotation (that was the production killer — see
// shouldProactivelyRotate); it is retained as the named horizon used in tests
// and as documentation of the window over which a session's ct0 is expected to
// be refreshed by the SERVER via set-cookie on each successful response.
const ct0MaxAge = 4 * time.Hour

// shouldProactivelyRotate reports whether the client should replace an account's
// ct0 with a freshly GENERATED (client-random) value before its next request.
//
// It is ALWAYS false. For an established session x.com validates ct0
// server-side: a client-generated ct0 can never match, so any proactive
// client-side rotation guarantees a CSRF error 353 (and the doomed recovery →
// relogin → guest-token cascade that takes the whole account down ~4h after
// boot — the live-reproduced failure). The legitimate refresh is server-driven:
// each successful response's set-cookie ct0 is adopted via SetCT0
// (extractCT0FromHeaders), keeping the session's ct0 current without ever
// inventing one. A client-generated ct0 is only valid at session
// ESTABLISHMENT, which is the login() cookie-absent fallback, not mid-session.
//
// Kept as a named predicate (rather than deleting the call sites inline) so the
// invariant "never client-rotate an established session's ct0" is asserted by a
// test and is greppable, guarding against a future reintroduction of the bug.
func shouldProactivelyRotate(_ *Account) bool {
	return false
}

// csrfRecoveryCT0 returns the ct0 x.com offered in a CSRF-353 response's
// set-cookie header, when present and well-formed. The CSRF-353 recovery path
// adopts THIS value rather than generating a random one: rotating to a
// client-random ct0 on a 353 is proven futile against an established session.
// Returns (_, false) when x.com offered no adoptable ct0, signalling the caller
// to fall through to relogin (the session is genuinely expired).
func csrfRecoveryCT0(respHdrs map[string]string) (string, bool) {
	if ct0 := extractCT0FromHeaders(respHdrs); ct0 != "" {
		return ct0, true
	}
	return "", false
}

// extractCT0FromHeaders parses ct0 value from a set-cookie response header.
func extractCT0FromHeaders(headers map[string]string) string {
	cookie := headers["set-cookie"]
	if cookie == "" {
		return ""
	}
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "ct0=") {
			val := strings.TrimPrefix(part, "ct0=")
			if val != "" && ct0Re.MatchString(val) {
				return val
			}
		}
	}
	return ""
}
