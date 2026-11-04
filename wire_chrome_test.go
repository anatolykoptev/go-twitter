package twitter

import (
	"context"
	"strings"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Oracle: request headers of real Google Chrome 156.0.8078.80 (macOS, headful)
// loading x.com logged out, taken from the browser's own network log (wire
// order after the pseudo-headers; captured 2026-11-04), and of a same-origin
// fetch() carrying the headers x.com's script sets, sent from the same browser
// to an echo service. Chrome for Testing 152 sends the same order, with
// accept-language before sec-ch-ua instead of after accept-encoding. The data
// below is copied from those captures, not derived from this package.
var (
	// oracleXHRGet: XHR GET from x.com to a host of x.com's own site, logged out.
	oracleXHRGet = strings.Split("sec-ch-ua-platform,authorization,sec-ch-ua,x-twitter-client-language,"+
		"sec-ch-ua-mobile,x-twitter-active-user,x-client-transaction-id,x-guest-token,user-agent,accept,origin,"+
		"sec-fetch-site,sec-fetch-mode,sec-fetch-dest,referer,accept-encoding,accept-language,cookie,priority", ",")
	// oracleXHRPost: the same with a body; content-length leads, content-type
	// follows user-agent.
	oracleXHRPost = strings.Split("content-length,sec-ch-ua-platform,authorization,sec-ch-ua,x-twitter-client-language,"+
		"sec-ch-ua-mobile,x-twitter-active-user,x-client-transaction-id,x-guest-token,user-agent,content-type,accept,origin,"+
		"sec-fetch-site,sec-fetch-mode,sec-fetch-dest,referer,accept-encoding,accept-language,cookie,priority", ",")
	// oracleSameOriginAuthedGet: logged-in-shaped same-origin GET (no origin
	// header, as the Fetch standard has it).
	oracleSameOriginAuthedGet = strings.Split("sec-ch-ua-platform,authorization,x-csrf-token,sec-ch-ua,"+
		"x-twitter-client-language,sec-ch-ua-mobile,x-twitter-active-user,x-client-transaction-id,x-twitter-auth-type,"+
		"user-agent,content-type,accept,sec-fetch-site,sec-fetch-mode,sec-fetch-dest,referer,accept-encoding,"+
		"accept-language,cookie,priority", ",")
)

// Values Chrome sent for the fixed headers of an XHR.
var oracleXHRValues = map[string]string{
	"accept":          "*/*",
	"accept-language": "en-US,en;q=0.9",
	"priority":        "u=1, i",
	"referer":         "https://x.com/",
	"sec-fetch-mode":  "cors",
	"sec-fetch-dest":  "empty",
}

// chromeProfile is a current desktop Chrome identity (the shape wireChrome is
// for), as go-stealth lists it.
func chromeProfile(t *testing.T, tls stealth.TLSProfile, browser, osName string) stealth.BrowserProfile {
	t.Helper()
	for _, p := range stealth.BuiltinProfiles {
		if p.TLSProfile == tls && p.Browser == browser && p.OS == osName {
			return p
		}
	}
	t.Fatalf("no built-in profile %s/%s/%s", tls, browser, osName)
	return stealth.BrowserProfile{}
}

func chromeClient(t *testing.T) (*Client, *stealth.BrowserClient, *wireRecorder, string) {
	t.Helper()
	p := chromeProfile(t, stealth.ProfileChrome152, "chrome", "macos")
	c, err := NewClient(ClientConfig{DefaultProxy: deadProxy})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.warmDoerOverride = &fakeWarmBackend{}
	bc, rec := recorderClient(t, p)
	// One authenticated request initialises the transaction-id keys from the
	// local page set, so later logged-out requests get an id without any fetch.
	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET", "https://x.com/i/api/graphql/w/Warm", twitterHeaders("W", "W", p.UserAgent), nil)
	return c, bc, rec, p.UserAgent
}

func wireNames(r recordedReq) []string { return wireOrder(r.Method, r.Headers, r.Order) }

// without returns names minus drop.
func without(names []string, drop ...string) []string {
	var out []string
	for _, n := range names {
		keep := true
		for _, d := range drop {
			if n == d {
				keep = false
			}
		}
		if keep {
			out = append(out, n)
		}
	}
	return out
}

// A current identity sends a logged-out XHR to a host of x.com's site in the
// order and with the values the browser sends.
//
// Mutation: wire.go chromeHeaderOrder — move "cookie" before "user-agent" (the
// v0.8.6 position) => RED on the order assertion.
func TestChromeWire_LoggedOutXHR(t *testing.T) {
	c, bc, rec, ua := chromeClient(t)
	ctx := context.Background()

	h := guestHeaders("GT", ua)
	h["cookie"] = "guest_id=v1%3A1" // the cookie jar adds this on the wire
	_, _, _, _ = c.doRequestWithBody(ctx, nil, bc, "GET", "https://api.x.com/1.1/hashflags.json", h, nil)
	got := rec.last()
	assert.Equal(t, oracleXHRGet, wireNames(got), "GET order")
	for k, v := range oracleXHRValues {
		assert.Equal(t, v, got.Headers[k], k)
	}
	assert.Equal(t, "https://x.com", got.Headers["origin"], "cross-origin XHR carries origin even on GET")
	assert.Equal(t, "same-site", got.Headers["sec-fetch-site"])
	assert.NotContains(t, got.Headers, "x-xp-forwarded-for", "logged out: the browser sends none")

	h = guestHeaders("GT", ua)
	h["cookie"] = "guest_id=v1%3A1"
	_, _, _, _ = c.doRequestWithBody(ctx, nil, bc, "POST", "https://api.x.com/1.1/flow/viewer.json", h, strings.NewReader("{}"))
	got = rec.last()
	assert.Equal(t, oracleXHRPost, wireNames(got), "POST order")
}

// An authenticated same-origin GET goes out as a same-origin fetch: no origin,
// same-origin fetch metadata, and the header positions of the fetch capture.
// x-xp-forwarded-for has no captured position (the capture sent none), so it is
// left out of the comparison.
//
// Mutation: wire.go shape — leave origin in place for same-origin GETs => RED.
func TestChromeWire_AuthenticatedSameOriginGet(t *testing.T) {
	c, bc, rec, ua := chromeClient(t)
	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET",
		"https://x.com/i/api/graphql/q/Op", twitterHeaders("AUTH", "CT0", ua), nil)
	got := rec.last()
	assert.Equal(t, oracleSameOriginAuthedGet, without(wireNames(got), "x-xp-forwarded-for"))
	assert.Equal(t, "same-origin", got.Headers["sec-fetch-site"])
	assert.NotContains(t, got.Headers, "origin")
	assert.Contains(t, got.Headers, "x-xp-forwarded-for", "logged in: still sent")
}

// sec-fetch-site and origin follow the request: Fetch's rules, per target host
// and method.
//
// Mutation: wire.go fetchSite — return "same-origin" for every host (the
// v0.8.6 hard-coded value) => RED on the api.x.com and api.twitter.com rows.
func TestChromeWire_FetchSiteAndOriginRule(t *testing.T) {
	cases := []struct {
		method, url, site string
		origin            bool
	}{
		{"GET", "https://x.com/i/api/graphql/q/Op", "same-origin", false},
		{"POST", "https://x.com/i/api/graphql/q/Op", "same-origin", true},
		{"GET", "https://api.x.com/1.1/hashflags.json", "same-site", true},
		{"POST", "https://api.x.com/1.1/flow/viewer.json", "same-site", true},
		{"POST", "https://api.twitter.com/1.1/guest/activate.json", "cross-site", true},
		{"GET", "https://api.twitter.com/1.1/account/settings.json", "cross-site", true},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.url, func(t *testing.T) {
			h := twitterHeaders("AUTH", "CT0", chromeProfile(t, stealth.ProfileChrome152, "chrome", "windows").UserAgent)
			wireGenFor(stealth.ProfileChrome152).shape(tc.method, tc.url, h)
			assert.Equal(t, tc.site, h["sec-fetch-site"])
			if tc.origin {
				assert.Equal(t, "https://x.com", h["origin"])
			} else {
				assert.NotContains(t, h, "origin")
			}
		})
	}
}

// The unauthenticated flows of a current identity carry fetch metadata, an
// explicit accept-encoding and priority like every other XHR.
//
// Mutation: wire.go shape — skip priority/sec-fetch for requests without a
// csrf token => RED.
func TestChromeWire_GuestAndLoginFlowCarryBrowserHeaders(t *testing.T) {
	c, bc, rec, _ := chromeClient(t)
	check := func(name string) {
		t.Helper()
		got := rec.last()
		for _, k := range []string{"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "accept-encoding", "priority", "accept-language", "accept"} {
			assert.NotEmpty(t, got.Headers[k], "%s: %s", name, k)
		}
		assert.Equal(t, "u=1, i", got.Headers["priority"], name)
		assert.Equal(t, chromeAcceptEncoding, got.Headers["accept-encoding"], name)
	}
	_, err := c.getGuestToken(bc)
	require.NoError(t, err)
	check("guest activate")
	_, err = c.initLoginFlowFull(bc, "GT")
	require.NoError(t, err)
	check("login flow init")
	_, err = c.submitFlowStep(bc, "GT", `{}`)
	require.NoError(t, err)
	check("login flow step")
}

// Which identities get the new shape: the current Chrome/Edge profiles; the
// legacy one and every profile not listed keep v0.8.6.
//
// Mutation: wire.go wireGenFor — return wireLegacy for chrome_152 => RED.
func TestWireGenFor(t *testing.T) {
	for _, p := range []stealth.TLSProfile{stealth.ProfileChrome146, stealth.ProfileChrome150, stealth.ProfileChrome152} {
		assert.Equal(t, wireChrome, wireGenFor(p), p)
	}
	for _, p := range []stealth.TLSProfile{
		stealth.ProfileChrome131, stealth.ProfileChrome133, stealth.ProfileChrome144, stealth.ProfileBrave146,
		stealth.ProfileFirefox148, stealth.ProfileSafariIOS26, "", "chrome_999",
	} {
		assert.Equal(t, wireLegacy, wireGenFor(p), "profile %q must stay on the frozen shape", p)
	}
	assert.Equal(t, wireLegacy, wireGenOf(nil))
}

// Every identity this package can derive for an account without one is a
// profile wireGenFor knows (a derived identity never lands on the frozen shape
// by omission), and a client's generation follows its own TLS profile.
func TestWireGen_DerivedIdentitiesAreCurrent(t *testing.T) {
	for _, p := range identityCandidates() {
		assert.Equal(t, wireChrome, wireGenFor(p.TLSProfile), "%s/%s/%s", p.Browser, p.OS, p.TLSProfile)
	}
	bc, _ := recorderClient(t, chromeProfile(t, stealth.ProfileChrome150, "chrome", "linux"))
	assert.Equal(t, wireChrome, wireGenOf(bc))
	bc, _ = recorderClient(t, legacyProfile)
	assert.Equal(t, wireLegacy, wireGenOf(bc))
}

// The transport-level options reach current identities only: whatever
// chromeClientOptions holds (WithFetchMode once go-stealth has it) is never
// applied to a legacy or unlisted profile.
//
// Mutation: wire.go wireClientOptions — drop the wireGenFor check => RED.
func TestWireClientOptions_NeverForLegacy(t *testing.T) {
	old := chromeClientOptions
	chromeClientOptions = []stealth.ClientOption{stealth.WithTimeout(1)}
	t.Cleanup(func() { chromeClientOptions = old })
	assert.Len(t, wireClientOptions(stealth.ProfileChrome152), 1)
	assert.Empty(t, wireClientOptions(stealth.ProfileChrome131))
	assert.Empty(t, wireClientOptions(stealth.ProfileBrave146))
	assert.Empty(t, wireClientOptions(""))
}
