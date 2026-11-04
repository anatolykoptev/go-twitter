package twitter

import (
	"strings"
	"sync"
)

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
// The Account is the newer source when its values changed since the Client's
// last request for it (SetCredentials on a live Account): see
// cookieState.accountIsNewer. go-stealth v1.27.0 cannot write to the jar, so
// that request still carries the jar's stale value after the Account's.
//
// Used for wireChrome only; the legacy wire is frozen, cookies included.
func singleSourceCookies(jar cookieJarReader, rawURL string, h map[string]string, st *cookieState) {
	cookie := h["cookie"]
	if cookie == "" {
		return
	}
	pairs := strings.Split(cookie, "; ")
	kept := pairs[:0:0]
	for _, pair := range pairs {
		name, explicit, _ := strings.Cut(pair, "=")
		if !isSessionCookie(name) {
			kept = append(kept, pair)
			continue
		}
		v := jar.GetCookieValue(rawURL, name)
		if v == "" || (name == "ct0" && !ct0Re.MatchString(v)) || st.accountIsNewer(name, explicit, v) {
			kept = append(kept, pair)
			continue
		}
		if name == "ct0" {
			// Only where the request already has the header: a document
			// navigation carries none.
			//
			// The jar is appended by fhttp at send time, after go-stealth's URL
			// guard (a DNS lookup), so a rotation landing in that window can still
			// put a csrf token that differs from the ct0 cookie on one request;
			// the 353 recovery adopts the server's value. Closing it takes a
			// caller-owned jar shared by the clients and no explicit header
			// (go-stealth WithCookieJar), not this rule.
			if _, ok := h["x-csrf-token"]; ok {
				h["x-csrf-token"] = v
			}
		}
	}
	if len(kept) == 0 {
		delete(h, "cookie")
		return
	}
	h["cookie"] = strings.Join(kept, "; ")
}

// cookieState is what one Client remembers of one Account's session cookies:
// the value of each the Account sent last, and, for a value found to be newer
// than the jar's, the jar value it superseded.
type cookieState struct {
	mu   sync.Mutex
	last map[string]string // name -> the Account's value at its last request
	pin  map[string]string // name -> the jar's value when the Account's was found newer
}

// accountIsNewer reports whether the Account's value for name (explicit) must be
// sent although the jar holds a different one (jarVal). The Account is newer
// when its value changed since the last request (SetCredentials on a live
// Account; a value adopted from a response also changes it, but then it equals
// the jar's) and stays newer until the jar's value moves, which is the server
// rotating it. fhttp still appends the jar's stale value after it: go-stealth
// v1.27.0 cannot remove or overwrite a jar entry, so that request carries the
// name twice, the Account's value first.
func (st *cookieState) accountIsNewer(name, explicit, jarVal string) bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.last == nil {
		st.last, st.pin = map[string]string{}, map[string]string{}
	}
	prev, seen := st.last[name]
	st.last[name] = explicit
	if explicit == jarVal {
		delete(st.pin, name)
		return false
	}
	if seen && prev != explicit {
		st.pin[name] = jarVal
	}
	if pinned, ok := st.pin[name]; ok {
		if pinned == jarVal {
			return true
		}
		delete(st.pin, name)
	}
	return false
}

// cookieStateFor is the state for acc on this Client; nil for a request that
// carries no account.
func (c *Client) cookieStateFor(acc *Account) *cookieState {
	if acc == nil {
		return nil
	}
	st, _ := c.cookieSeen.LoadOrStore(acc, &cookieState{})
	return st.(*cookieState)
}

func isSessionCookie(name string) bool {
	for _, n := range sessionCookieNames {
		if n == name {
			return true
		}
	}
	return false
}
