package twitter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mapJar is a cookie jar for the unit cases: "<name>" -> value, any URL.
type mapJar map[string]string

func (j mapJar) GetCookieValue(_, name string) string { return j[name] }

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

// A session cookie the client's jar holds is sent from the jar only: the
// explicit header keeps just the names the jar lacks, and x-csrf-token follows
// the ct0 that goes out as the cookie. fhttp appends the jar to whatever Cookie
// header is set, so a name left in both places would be sent twice.
//
// Mutation: cookies.go singleSourceCookies — replace the `continue` that drops a
// pair the jar holds with `kept = append(kept, pair)` => RED on every
// "jar holds" case (the pair stays in the explicit header).
func TestSingleSourceCookies_JarHoldsNameSentFromJarOnly(t *testing.T) {
	cases := []struct {
		name       string
		jar        mapJar
		in         string
		wantCookie string // "" = the header is removed
		wantCSRF   string
	}{
		{"jar rotated ct0 is the newest", mapJar{"ct0": "NEW"}, "auth_token=T; ct0=OLD", "auth_token=T", "NEW"},
		{"jar holds both", mapJar{"ct0": "NEW", "auth_token": "T"}, "auth_token=T; ct0=OLD", "", "NEW"},
		{"jar holds auth_token only", mapJar{"auth_token": "T2"}, "auth_token=T; ct0=C", "ct0=C", "C"},
		{"jar holds neither", mapJar{"guest_id": "g"}, "auth_token=T; ct0=C", "auth_token=T; ct0=C", "C"},
		{"jar holds a ct0 that is not a token", mapJar{"ct0": "a b\r\nx"}, "auth_token=T; ct0=C", "auth_token=T; ct0=C", "C"},
		{"other explicit cookies are kept in order", mapJar{"ct0": "NEW"}, "auth_token=T; ct0=OLD; x=1", "auth_token=T; x=1", "NEW"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string]string{"cookie": tc.in, "x-csrf-token": "OLDCSRF"}
			if strings.Contains(tc.in, "ct0=C") {
				h["x-csrf-token"] = "C"
			}
			singleSourceCookies(tc.jar, "https://api.x.com/i/api/graphql/q/Op", h)
			if tc.wantCookie == "" {
				assert.NotContains(t, h, "cookie")
			} else {
				assert.Equal(t, tc.wantCookie, h["cookie"])
			}
			assert.Equal(t, tc.wantCSRF, h["x-csrf-token"])
		})
	}
}

// A request without a cookie header is left alone (guest paths).
func TestSingleSourceCookies_NoHeaderNoChange(t *testing.T) {
	h := map[string]string{"x-guest-token": "g"}
	singleSourceCookies(mapJar{"ct0": "NEW"}, "https://api.x.com/1.1/x.json", h)
	assert.Equal(t, map[string]string{"x-guest-token": "g"}, h)
}

// Through the Client: a current identity's request carries ct0 once, from the
// jar, and the csrf header agrees with it.
//
// Mutation: client.go doRequestWithBody — delete the singleSourceCookies call
// => RED (cookie stays "auth_token=AUTH; ct0=OLDCT0").
func TestSingleSourceCookies_ChromeRequestUsesJarCT0(t *testing.T) {
	c, bc, rec, ua := chromeClient(t)
	rec.jar = map[string]string{"https://api.x.com/i/api/graphql/q/Op ct0": "NEWCT0"}
	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET",
		"https://api.x.com/i/api/graphql/q/Op", twitterHeaders("AUTH", "OLDCT0", ua), nil)
	got := rec.last()
	assert.Equal(t, "auth_token=AUTH", got.Headers["cookie"])
	assert.Equal(t, "NEWCT0", got.Headers["x-csrf-token"])
}

// The legacy wire is frozen, cookies included: the explicit header goes out as
// built even when the jar holds the same names.
//
// Mutation: client.go doRequestWithBody — drop the `== wireChrome` condition
// around the singleSourceCookies call => RED (cookie loses ct0).
func TestSingleSourceCookies_LegacyWireUntouched(t *testing.T) {
	c, bc, rec := legacyClient(t)
	rec.jar = map[string]string{
		"https://api.twitter.com/graphql/q/Op ct0":        "NEWCT0",
		"https://api.twitter.com/graphql/q/Op auth_token": "AUTH",
	}
	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET",
		"https://api.twitter.com/graphql/q/Op", twitterHeaders("AUTH", "OLDCT0", legacyProfile.UserAgent), nil)
	got := rec.last()
	assert.Equal(t, "auth_token=AUTH; ct0=OLDCT0", got.Headers["cookie"])
	assert.Equal(t, "OLDCT0", got.Headers["x-csrf-token"])
}

// pageNav is a navTransport that serves a fixed jar and records the request.
type pageNav struct {
	jar     mapJar
	headers map[string]string
}

func (p *pageNav) ChromeHeaders() map[string]string     { return map[string]string{"user-agent": "ua"} }
func (p *pageNav) GetCookieValue(_, name string) string { return p.jar[name] }
func (p *pageNav) DoWithHeaderOrderCtx(_ context.Context, _, _ string, h map[string]string, _ io.Reader, _ []string) ([]byte, map[string]string, int, error) {
	p.headers = h
	return nil, nil, 200, nil
}

// A warm page navigation sends a session cookie its own client's jar holds once,
// from the jar.
//
// Mutation: warm_nav.go warmRouter.DoCtx — delete the singleSourceCookies call
// => RED (cookie stays "auth_token=T; ct0=OLD").
func TestWarmNavigation_JarCookieNotDuplicated(t *testing.T) {
	nav := &pageNav{jar: mapJar{"ct0": "PAGECT0"}}
	r := &warmRouter{page: nav}
	_, _, _, err := r.DoCtx(context.Background(), "GET", "https://x.com/home", map[string]string{"cookie": "auth_token=T; ct0=OLD"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "auth_token=T", nav.headers["cookie"])
}

// echoCookies is a TLS server standing in for x.com: /set answers with the
// Set-Cookie headers of a ct0 rotation and a page cookie, anything else echoes
// the Cookie header(s) it received.
func echoCookies(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/set" {
			http.SetCookie(w, &http.Cookie{Name: "ct0", Value: "ROTATED", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "guest_id", Value: "g1", Path: "/"})
			return
		}
		_ = json.NewEncoder(w).Encode(r.Header.Values("Cookie"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sentCookieNames(t *testing.T, body []byte) []string {
	t.Helper()
	var lines []string
	require.NoError(t, json.Unmarshal(body, &lines), string(body))
	return cookieNames(strings.Join(lines, "; "))
}

// On the real transport (tls-client, its cookie jar), a ct0 the server rotated
// into the jar and the explicit session cookies reach the wire as one Cookie
// header with each name once, the rotated ct0 among them. The first request
// pins the premise: without the rule fhttp appends the jar to the explicit
// header and ct0 goes out twice.
//
// Mutation: cookies.go singleSourceCookies — return at the top => RED on the
// second assertion (ct0 twice).
func TestSingleSourceCookies_RealJarEachNameOnce(t *testing.T) {
	srv := echoCookies(t)
	p := chromeProfile(t, stealth.ProfileChrome152, "chrome", "macos")
	bc, err := stealth.NewClient(append([]stealth.ClientOption{
		stealth.WithIdentity(stealth.BrowserIdentity{BrowserProfile: p}), stealth.WithHeaderOrder(twitterHeaderOrder),
		stealth.WithInsecureSkipVerify(), stealth.WithoutSSRFGuard(),
	}, wireClientOptions(wireChrome)...)...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close() })

	_, _, status, err := bc.Do("GET", srv.URL+"/set", map[string]string{}, nil)
	require.NoError(t, err)
	require.Equal(t, 200, status)
	require.Equal(t, "ROTATED", bc.GetCookieValue(srv.URL+"/x", "ct0"), "the jar adopted the server's ct0")

	send := func(h map[string]string) []byte {
		order := wireChrome.shape("GET", srv.URL+"/x", h)
		body, _, st, err := bc.DoWithHeaderOrder("GET", srv.URL+"/x", h, nil, order)
		require.NoError(t, err)
		require.Equal(t, 200, st)
		return body
	}
	raw := twitterHeaders("AUTH", "OLDCT0", p.UserAgent)
	assert.ElementsMatch(t, []string{"auth_token", "ct0", "ct0", "guest_id"}, sentCookieNames(t, send(raw)), "premise: the jar is appended to the explicit header")

	h := twitterHeaders("AUTH", "OLDCT0", p.UserAgent)
	singleSourceCookies(bc, srv.URL+"/x", h)
	assert.ElementsMatch(t, []string{"auth_token", "ct0", "guest_id"}, sentCookieNames(t, send(h)))
	assert.Equal(t, "ROTATED", h["x-csrf-token"])
}
