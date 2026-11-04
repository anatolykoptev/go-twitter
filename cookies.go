package twitter

import (
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/publicsuffix"
)

// sessionCookieNames are the cookies the Account owns: they are written into
// the Account's cookie jar from the Account's values.
var sessionCookieNames = []string{"auth_token", "ct0"}

// defaultSessionOrigins are the sites a current identity's session cookies are
// scoped to. x.com serves the web client (x.com and api.x.com); twitter.com is
// the legacy host a current identity still reaches (upload.twitter.com, the
// media upload). Both used to receive the explicit Cookie header.
var defaultSessionOrigins = []string{"https://x.com/", "https://twitter.com/"}

// sessionJar is the one cookie store of a current-identity Account, shared by
// the Account's XHR client and its warm-navigation client (stealth.WithCookieJar),
// as the tabs of one browser share theirs. The Account stays the source of the
// session values: sync writes auth_token and ct0 into the jar whenever the
// Account's value differs from the one last written, and everything the server
// sets (Set-Cookie on any response of either client) lands in the jar by itself.
//
// A request therefore carries each cookie once, from the jar, in the jar's
// RFC 6265 order; there is no explicit Cookie header to double it.
//
// Used for wireChrome accounts only: the legacy wire is frozen, jar behaviour
// included (tls-client's default jar keys by the last two host labels and
// ignores Domain and Path).
type sessionJar struct {
	jar    *cookiejar.Jar
	scopes []jarScope

	mu         sync.Mutex
	seededAuth string // the Account's auth_token last written (or found) in the jar
	seededCT0  string
}

// jarScope is one site the session cookies are seeded for.
type jarScope struct {
	origin *url.URL
	domain string // Domain attribute; "" = host-only (an IP, as a test server has)
}

func newSessionJar(origins []string) (*sessionJar, error) {
	// The public suffix list keeps a response from setting a cookie for a whole
	// TLD (Domain=com); without it the jar would accept one.
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	if len(origins) == 0 {
		origins = defaultSessionOrigins
	}
	s := &sessionJar{jar: jar}
	for _, o := range origins {
		u, err := url.Parse(o)
		if err != nil || u.Hostname() == "" {
			return nil, err
		}
		sc := jarScope{origin: u}
		if net.ParseIP(u.Hostname()) == nil {
			sc.domain = u.Hostname()
		}
		s.scopes = append(s.scopes, sc)
	}
	return s, nil
}

// sync writes the Account's values into the jar where they differ from what
// was last written. It compares with the last write, never with the jar: a
// ct0 the server rotated into the jar must not be overwritten by an Account
// that has not changed. Where the jar already holds the Account's value (the
// Account adopted it from a Set-Cookie the jar had seen) nothing is written,
// so the server's attributes stay.
func (s *sessionJar) sync(authToken, ct0 string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if authToken != s.seededAuth {
		s.seed("auth_token", authToken, true)
		s.seededAuth = authToken
	}
	if ct0 != s.seededCT0 {
		s.seed("ct0", ct0, false)
		s.seededCT0 = ct0
	}
}

// seed makes name=value the only cookie of that name the jar sends to each
// scope; an empty value removes it. Attributes are those of the cookies X sets
// for a session: Domain the site, Path /, Secure, HttpOnly on auth_token. (They
// are not pinned by a capture in this repo; the jar acts only on Domain, Path
// and Secure, and HttpOnly is inert in a Go client.)
func (s *sessionJar) seed(name, value string, httpOnly bool) {
	for _, sc := range s.scopes {
		if value != "" && s.holdsOnly(sc.origin, name, value) {
			continue
		}
		if sc.domain != "" {
			// A host-only twin (a cookie a response set without Domain) is another
			// jar entry from the Domain cookie and would go out beside it.
			for _, h := range []string{sc.domain, "api." + sc.domain} {
				twin := *sc.origin
				twin.Host = h
				s.jar.SetCookies(&twin, []*http.Cookie{{Name: name, Path: "/", MaxAge: -1}})
			}
		}
		if value == "" {
			s.jar.SetCookies(sc.origin, []*http.Cookie{{Name: name, Path: "/", Domain: sc.domain, MaxAge: -1}})
			continue
		}
		s.jar.SetCookies(sc.origin, []*http.Cookie{{
			Name: name, Value: value, Path: "/", Domain: sc.domain, Secure: true, HttpOnly: httpOnly,
		}})
	}
}

// holdsOnly reports whether the jar sends exactly one cookie called name to u
// and its value is value.
func (s *sessionJar) holdsOnly(u *url.URL, name, value string) bool {
	n, v := 0, ""
	for _, c := range s.jar.Cookies(u) {
		if c.Name == name {
			n, v = n+1, c.Value
		}
	}
	return n == 1 && v == value
}

// value is the cookie name the jar sends to rawURL, "" when it sends none.
func (s *sessionJar) value(u *url.URL, name string) string {
	for _, c := range s.jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// forRequest makes the jar the one source of the session cookies of a request to
// rawURL: an explicit pair the jar will send is dropped from the Cookie header
// (fhttp appends the jar to whatever header the request carries, so a name in
// both would go out twice), and x-csrf-token becomes the ct0 the jar sends.
// The jar is read here, as late as the request is ours; go-stealth reads it
// again when it writes the request, after its URL guard and the connection
// acquire, so a rotation landing between the two reads still costs one 353
// (recovery adopts the server's value).
//
// A name the jar will not send to that host (a site outside the seeded scopes)
// stays in the explicit header, which then is its only source.
func (s *sessionJar) forRequest(rawURL string, h map[string]string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	ct0 := s.value(u, "ct0")
	if !ct0Re.MatchString(ct0) {
		ct0 = ""
	}
	if cookie := h["cookie"]; cookie != "" {
		kept := []string(nil)
		for _, pair := range strings.Split(cookie, "; ") {
			name, _, _ := strings.Cut(pair, "=")
			held := (name == "ct0" && ct0 != "") || (name == "auth_token" && s.value(u, name) != "")
			if !held {
				kept = append(kept, pair)
			}
		}
		if len(kept) == 0 {
			delete(h, "cookie")
		} else {
			h["cookie"] = strings.Join(kept, "; ")
		}
	}
	// Only where the request already has the header: a document navigation
	// carries none.
	if _, ok := h["x-csrf-token"]; ok && ct0 != "" {
		h["x-csrf-token"] = ct0
	}
}

// sessionJarFor returns the Account's cookie jar, creating it on first use. The
// jar is created once (atomically: this runs with the Account's mutex held from
// clientForAccount and navClientFor, and without it from NewClient). The new
// jar is seeded from the Account's fields, read directly like the rest of the
// client build.
func (a *Account) sessionJarFor(cfg ClientConfig) (*sessionJar, error) {
	if sj := a.jar.Load(); sj != nil {
		return sj, nil
	}
	sj, err := newSessionJar(cfg.sessionOrigins)
	if err != nil {
		return nil, err
	}
	if !a.jar.CompareAndSwap(nil, sj) {
		sj = a.jar.Load()
	}
	sj.sync(a.AuthToken, a.CT0)
	return sj, nil
}

// syncedJar is the Account's jar brought up to date with the Account's current
// values, nil for an Account without one (a legacy identity, or no client yet).
// It runs on every request, under the Account's mutex so the values it writes
// are the ones the Account holds now, not a snapshot an older request took:
// that is what keeps a field written directly (AuthToken and CT0 are exported)
// from being sent stale, and what keeps an out-of-order request from writing an
// old ct0 over a newer one.
func (a *Account) syncedJar() *sessionJar {
	if a == nil {
		return nil
	}
	sj := a.jar.Load()
	if sj == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	sj.sync(a.AuthToken, a.CT0)
	return sj
}
