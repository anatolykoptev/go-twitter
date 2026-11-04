package twitter

import "strings"

// cookieJarReader is what the single-source rule reads a client's cookie jar
// through; *stealth.BrowserClient implements it.
type cookieJarReader interface {
	GetCookieValue(rawURL, name string) string
}

// sessionCookieNames are the cookies twitterHeaders writes into the explicit
// Cookie header: the ones the client's own cookie jar can also come to hold (a
// login through the client, a ct0 the server rotated).
var sessionCookieNames = []string{"auth_token", "ct0"}

// singleSourceCookies makes the cookie jar the one source of every cookie a
// current identity sends. fhttp appends the jar's cookies to whatever Cookie
// header the request already carries, so a session cookie present in both the
// explicit header and the jar goes out twice (`auth_token=T; ct0=NEW; guest_id=g;
// ct0=PAGE`), which a browser never does. For each session cookie the jar holds
// for rawURL, the explicit pair is dropped and the jar's value is what goes out;
// the explicit header only seeds names the jar does not hold yet.
//
// The jar is also the newest source for ct0: every response's Set-Cookie lands
// in it, and so does the ct0 the Account adopts from that response (SetCT0), so
// when they differ the jar saw the later one. x-csrf-token is set to the ct0
// that goes out as the cookie, since X rejects a pair that disagrees.
//
// What this cannot do, without go-stealth writing to the jar: make an Account's
// auth_token win over a different one in the jar, or a ct0 set on the Account
// after the jar took its own (SetCredentials) win over the jar's.
//
// Used for wireChrome only; the legacy wire is frozen, cookies included.
func singleSourceCookies(jar cookieJarReader, rawURL string, h map[string]string) {
	cookie := h["cookie"]
	if cookie == "" {
		return
	}
	pairs := strings.Split(cookie, "; ")
	kept := pairs[:0:0]
	for _, pair := range pairs {
		name, _, _ := strings.Cut(pair, "=")
		if !isSessionCookie(name) {
			kept = append(kept, pair)
			continue
		}
		v := jar.GetCookieValue(rawURL, name)
		if v == "" || (name == "ct0" && !ct0Re.MatchString(v)) {
			kept = append(kept, pair)
			continue
		}
		if name == "ct0" {
			h["x-csrf-token"] = v
		}
	}
	if len(kept) == 0 {
		delete(h, "cookie")
		return
	}
	h["cookie"] = strings.Join(kept, "; ")
}

func isSessionCookie(name string) bool {
	for _, n := range sessionCookieNames {
		if n == name {
			return true
		}
	}
	return false
}
