package twitter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cookieNames lists the cookie names of a Cookie header value, in order.
func cookieNames(header string) []string {
	var out []string
	for _, p := range strings.Split(header, "; ") {
		if p == "" {
			continue
		}
		n, _, _ := strings.Cut(p, "=")
		out = append(out, n)
	}
	return out
}

// jarEchoReq is what the echo server saw of one request.
type jarEchoReq struct {
	Host, Path string
	Cookie     []string // every Cookie header line
	CSRF       string
}

// names are the cookie names across all Cookie header lines.
func (r jarEchoReq) names() []string { return cookieNames(strings.Join(r.Cookie, "; ")) }

// value is the value of cookie name on the request, "" if absent.
func (r jarEchoReq) value(name string) string {
	for _, p := range strings.Split(strings.Join(r.Cookie, "; "), "; ") {
		if n, v, _ := strings.Cut(p, "="); n == name {
			return v
		}
	}
	return ""
}

// jarRig is a fake X on two hostnames of one TLS listener: x.localhost (the
// site, x.com's stand-in) and api.x.localhost (api.x.com's). *.localhost resolves
// to loopback, so no DNS or proxy is involved and nothing leaves the machine.
// Paths: /page sets a page cookie scoped to the site; /rotate sets ct0 scoped to
// the site; /hostonly sets a ct0 with no Domain; anything else is echoed.
type jarRig struct {
	c    *Client
	acc  *Account
	port string

	mu   sync.Mutex
	reqs []jarEchoReq
}

func newJarRig(t *testing.T, acc *Account) *jarRig {
	t.Helper()
	r := &jarRig{acc: acc}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.reqs = append(r.reqs, jarEchoReq{Host: req.Host, Path: req.URL.Path, Cookie: req.Header.Values("Cookie"), CSRF: req.Header.Get("X-Csrf-Token")})
		r.mu.Unlock()
		switch req.URL.Path {
		case "/page":
			http.SetCookie(w, &http.Cookie{Name: "guest_id", Value: "g1", Path: "/", Domain: "x.localhost", Secure: true})
		case "/rotate":
			http.SetCookie(w, &http.Cookie{Name: "ct0", Value: "ROTATED", Path: "/", Domain: "x.localhost", Secure: true})
		case "/hostonly":
			http.SetCookie(w, &http.Cookie{Name: "ct0", Value: "TWIN", Path: "/", Secure: true})
		}
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	r.port = u.Port()
	r.c = newLoopbackClient(t, ClientConfig{
		Accounts:       []*Account{acc},
		sessionOrigins: []string{"https://x.localhost:" + r.port + "/"},
		stealthOpts:    []stealth.ClientOption{stealth.WithInsecureSkipVerify()},
	})
	return r
}

func (r *jarRig) url(host, path string) string {
	return "https://" + host + ".localhost:" + r.port + path
}

// xhr sends an XHR with headers built from the Account now.
func (r *jarRig) xhr(t *testing.T, host, path string) jarEchoReq {
	t.Helper()
	tok, ct0, ua := r.acc.Credentials()
	return r.send(t, host, path, twitterHeaders(tok, ct0, ua))
}

// send sends an XHR with the headers given (built earlier, possibly stale).
func (r *jarRig) send(t *testing.T, host, path string, h map[string]string) jarEchoReq {
	t.Helper()
	_, _, st, err := r.c.doRequestWithBody(context.Background(), r.acc, r.acc.client, "GET", r.url(host, path), h, nil)
	require.NoError(t, err)
	require.Equal(t, 200, st)
	return r.last()
}

// page navigates the Account's navigation client to path of the site.
func (r *jarRig) page(t *testing.T, path string) jarEchoReq {
	t.Helper()
	nav := r.c.navClientFor(r.acc)
	require.NotNil(t, nav)
	h, order := navigationRequest(nav.ChromeHeaders(), "GET", nil)
	_, _, st, err := nav.DoWithHeaderOrderCtx(context.Background(), "GET", r.url("x", path), h, nil, order)
	require.NoError(t, err)
	require.Equal(t, 200, st)
	return r.last()
}

func (r *jarRig) last() jarEchoReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[len(r.reqs)-1]
}

func currentAccount(t *testing.T) *Account {
	return &Account{Username: "u", AuthToken: "AUTH", CT0: "OLDCT0", Profile: chromeProfile(t, stealth.ProfileChrome152, "chrome", "macos")}
}

// A cookie a page response sets reaches the account's next XHR, and a cookie an
// api-host response sets for the site reaches the site's XHRs: the two clients
// of one account share a jar, scoped the way a browser scopes it.
//
// Mutation: client.go buildAccountClientFor — `if accountWire(acc) == wireChrome {`
// -> `if accountWire(acc) == wireChrome && !navigation {` (the navigation client
// keeps its own jar) => RED at "page cookie reaches the XHR" (guest_id absent).
func TestSharedJar_PageSetCookieReachesNextXHR(t *testing.T) {
	r := newJarRig(t, currentAccount(t))

	r.page(t, "/page")
	got := r.xhr(t, "api.x", "/i/api/graphql/q/Op")
	assert.Equal(t, "g1", got.value("guest_id"), "page cookie reaches the XHR (site-scoped cookie, api host)")

	r.xhr(t, "api.x", "/rotate")
	got = r.xhr(t, "x", "/i/api/graphql/q/Op")
	assert.Equal(t, "ROTATED", got.value("ct0"), "ct0 set by an api-host response reaches the site's XHR")
	got = r.page(t, "/home")
	assert.Equal(t, "ROTATED", got.value("ct0"), "and the next page navigation")
	assert.Equal(t, "g1", got.value("guest_id"))
}

// A cookie set without Domain by an api-host response is that host's alone, as
// in a browser: it does not reach the site.
func TestSharedJar_HostOnlyCookieStaysOnItsHost(t *testing.T) {
	r := newJarRig(t, currentAccount(t))
	r.xhr(t, "api.x", "/hostonly")
	got := r.xhr(t, "x", "/i/api/graphql/q/Op")
	assert.NotContains(t, strings.Join(got.Cookie, ";"), "TWIN")
}

// Every cookie name goes out exactly once, in one Cookie header, on an XHR and
// on a navigation, including after the server rotated ct0 into the jar; a
// request built with the previous ct0 carries the jar's.
//
// Mutation: client.go doRequestWithBody — delete the `sj.forRequest(urlStr, headers)`
// call => RED (the explicit header and the jar both send auth_token and ct0).
func TestSharedJar_EachCookieNameOnce(t *testing.T) {
	r := newJarRig(t, currentAccount(t))
	check := func(label string, got jarEchoReq) {
		t.Helper()
		assert.Len(t, got.Cookie, 1, label+": one Cookie header")
		assert.ElementsMatch(t, []string{"auth_token", "ct0", "guest_id"}, got.names(), label+": each name once")
	}
	r.page(t, "/page")
	check("XHR", r.xhr(t, "api.x", "/i/api/graphql/q/Op"))
	check("after the rotation", func() jarEchoReq { r.xhr(t, "api.x", "/rotate"); return r.xhr(t, "x", "/i/api/graphql/q/Op") }())
	check("navigation", r.page(t, "/home"))
}

// x-csrf-token is the ct0 that went out, in the cases where the Account's value
// and the jar's differ: the server rotated ct0 into the jar and the Account has
// not adopted it, and a request whose headers were built before the Account
// took a newer ct0 (cold xtid, pacing gap) -- #187.
//
// Mutation: cookies.go sessionJar.forRequest — delete the
// `h["x-csrf-token"] = ct0` assignment => RED (csrf OLDCT0 beside cookie ROTATED).
func TestSharedJar_CSRFIsTheCT0SentAsCookie(t *testing.T) {
	r := newJarRig(t, currentAccount(t))

	r.xhr(t, "api.x", "/rotate") // the jar now holds ROTATED; the Account still OLDCT0
	got := r.xhr(t, "x", "/i/api/graphql/q/Op")
	assert.Equal(t, "ROTATED", got.value("ct0"))
	assert.Equal(t, got.value("ct0"), got.CSRF, "csrf equals the ct0 cookie that went out")

	tok, ct0, ua := r.acc.Credentials()
	stale := twitterHeaders(tok, ct0, ua) // built before the rotation is adopted
	r.acc.SetCT0("NEWCT0")                // the adoption lands; the stale request is still to be sent
	got = r.send(t, "x", "/i/api/graphql/q/Op", stale)
	assert.Equal(t, "NEWCT0", got.value("ct0"), "an older request does not pin the older ct0")
	assert.Equal(t, "NEWCT0", got.CSRF)
	assert.Equal(t, []string{"auth_token", "ct0"}, without(got.names(), "guest_id"))
}

// SetCredentials replaces the values in the jar: the old ones are never sent
// again, not even through a host-only twin an api-host response left behind.
//
// Mutation: cookies.go sessionJar.sync — delete the `s.seed("ct0", ct0, false)`
// line => RED (the old ct0 keeps going out).
func TestSharedJar_SetCredentialsReplacesValues(t *testing.T) {
	r := newJarRig(t, currentAccount(t))
	r.xhr(t, "api.x", "/hostonly") // a host-only ct0=TWIN on api.x.localhost, beside the seeded one
	r.xhr(t, "api.x", "/i/api/graphql/q/Op")

	r.acc.SetCredentials("NEWTOK", "NEWCT0")
	for _, host := range []string{"api.x", "x"} {
		got := r.xhr(t, host, "/i/api/graphql/q/Op")
		assert.Equal(t, "NEWTOK", got.value("auth_token"), host)
		assert.Equal(t, "NEWCT0", got.value("ct0"), host)
		assert.Equal(t, "NEWCT0", got.CSRF, host)
		assert.ElementsMatch(t, []string{"auth_token", "ct0"}, got.names(), host+": each name once, no twin")
		assert.NotContains(t, strings.Join(got.Cookie, ";"), "OLDCT0")
		assert.NotContains(t, strings.Join(got.Cookie, ";"), "TWIN")
		assert.NotContains(t, strings.Join(got.Cookie, ";"), "AUTH;")
	}
}

// The exported fields can be written without SetCredentials; the next request
// still sends the Account's values.
//
// Mutation: cookies.go Account.syncedJar — delete the `sj.sync(a.AuthToken, a.CT0)`
// line => RED (the request sends the values the jar was seeded with).
func TestSharedJar_FieldWriteIsSent(t *testing.T) {
	r := newJarRig(t, currentAccount(t))
	r.xhr(t, "x", "/i/api/graphql/q/Op")
	r.acc.AuthToken, r.acc.CT0 = "FIELDTOK", "FIELDCT0"
	got := r.xhr(t, "x", "/i/api/graphql/q/Op")
	assert.Equal(t, "FIELDTOK", got.value("auth_token"))
	assert.Equal(t, "FIELDCT0", got.value("ct0"))
	assert.Equal(t, "FIELDCT0", got.CSRF)
}

// An Account that has not changed never overwrites a ct0 the server rotated
// into the jar: sync compares with its last write, not with the jar.
//
// Mutation: cookies.go sessionJar.sync — compare `ct0 != s.seededCT0` with the
// jar's value instead (e.g. `ct0 != s.value(s.scopes[0].origin, "ct0")`) => RED
// (ROTATED is overwritten by OLDCT0).
func TestSharedJar_UnchangedAccountKeepsRotation(t *testing.T) {
	r := newJarRig(t, currentAccount(t))
	r.xhr(t, "api.x", "/rotate")
	for i := 0; i < 2; i++ {
		got := r.xhr(t, "x", "/i/api/graphql/q/Op")
		assert.Equal(t, "ROTATED", got.value("ct0"))
	}
}

// A legacy identity keeps the transport's own jar and its explicit Cookie
// header, byte for byte: the caller jar is for current identities only.
//
// Mutation: client.go buildAccountClientFor — drop the `accountWire(acc) == wireChrome`
// condition => RED (the legacy account gets a jar).
func TestSharedJar_LegacyAccountKeepsExplicitHeader(t *testing.T) {
	acc := &Account{Username: "u", AuthToken: "AUTH", CT0: "OLDCT0", Profile: legacyProfile, UserAgent: legacyProfile.UserAgent}
	r := newJarRig(t, acc)
	assert.Nil(t, acc.jar.Load(), "legacy account has no caller jar")
	got := r.xhr(t, "x", "/i/api/graphql/q/Op")
	assert.Equal(t, []string{"auth_token=AUTH; ct0=OLDCT0"}, got.Cookie)
	assert.Equal(t, "OLDCT0", got.CSRF)
}

// The explicit pair is dropped only for a name the jar sends to that host; a
// host outside the seeded scopes keeps it (the pair then has no other source).
//
// Mutation: cookies.go sessionJar.forRequest — drop the pair unconditionally
// (`held := true`) => RED on the "outside the scopes" case.
func TestSessionJar_ForRequestScopes(t *testing.T) {
	sj, err := newSessionJar(nil)
	require.NoError(t, err)
	sj.sync("AUTH", "CT0")
	build := func() map[string]string {
		return map[string]string{"cookie": "auth_token=AUTH; ct0=CT0; x=1", "x-csrf-token": "STALE"}
	}
	for _, host := range []string{"https://api.x.com/i/api/q", "https://x.com/i/api/q", "https://upload.twitter.com/1.1/media/upload.json"} {
		h := build()
		sj.forRequest(host, h)
		assert.Equal(t, "x=1", h["cookie"], host)
		assert.Equal(t, "CT0", h["x-csrf-token"], host)
	}
	h := build()
	sj.forRequest("https://elsewhere.example/q", h)
	assert.Equal(t, "auth_token=AUTH; ct0=CT0; x=1", h["cookie"], "outside the scopes")
	assert.Equal(t, "STALE", h["x-csrf-token"])

	h = map[string]string{"x-guest-token": "g"} // a request without the headers is left alone
	sj.forRequest("https://api.x.com/q", h)
	assert.Equal(t, map[string]string{"x-guest-token": "g"}, h)
}

// The jar carries the public suffix list: a response cannot set a cookie for a
// public suffix (without the list, the jar keys co.uk like a site and shares the
// cookie with every .co.uk host).
//
// Mutation: cookies.go newSessionJar — pass an empty cookiejar.Options => RED.
func TestSessionJar_RejectsPublicSuffixCookie(t *testing.T) {
	sj, err := newSessionJar(nil)
	require.NoError(t, err)
	evil, _ := url.Parse("https://evil.co.uk/")
	other, _ := url.Parse("https://other.co.uk/")
	sj.jar.SetCookies(evil, []*http.Cookie{{Name: "evil", Value: "1", Domain: "co.uk", Path: "/"}})
	assert.Empty(t, sj.jar.Cookies(other))
}

// An Account with a jar sends its session cookies from the jar, so the warm-page
// fetcher carries no cookie of its own; it is still the authenticated fetcher.
//
// Mutation: client.go warmFetcherFor — delete the `sf.Cookie = ""` line => RED.
func TestWarmFetcherFor_JarAccountCarriesNoCookie(t *testing.T) {
	r := newJarRig(t, currentAccount(t))
	r.c.warmDoerOverride = nil // the doer is the Account's own clients, whose jar sends the cookies
	h := twitterHeaders("AUTH", "CT0", r.acc.UserAgent)
	f := r.c.warmFetcherFor(r.acc, r.acc.client, h)
	require.NotNil(t, f, "authenticated")
	assert.Empty(t, stealthFetcherCookie(f))

	legacy := &Account{Username: "l", AuthToken: "AUTH", CT0: "C", Profile: legacyProfile}
	assert.Contains(t, stealthFetcherCookie(r.c.warmFetcherFor(legacy, r.acc.client, h)), "auth_token=AUTH", "no jar: the explicit cookie stays")
}

func stealthFetcherCookie(f interface{}) string {
	if sf, ok := f.(*bundle.StealthFetcher); ok {
		return sf.Cookie
	}
	return "<not a StealthFetcher>"
}
