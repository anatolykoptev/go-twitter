package twitter

import (
	"context"
	"sort"
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
	c.owned.setWire(bc, wireGenFor(p.TLSProfile)) // as the Client does for the clients it builds
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
// same-origin fetch metadata, and the header positions of the fetch capture. The
// signed-in browser sends no x-xp-forwarded-for, so neither does a current
// identity.
//
// Mutation: wire.go shape — leave origin in place for same-origin GETs => RED.
func TestChromeWire_AuthenticatedSameOriginGet(t *testing.T) {
	c, bc, rec, ua := chromeClient(t)
	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET",
		"https://x.com/i/api/graphql/q/Op", twitterHeaders("AUTH", "CT0", ua), nil)
	got := rec.last()
	assert.Equal(t, oracleSameOriginAuthedGet, wireNames(got))
	assert.Equal(t, "same-origin", got.Headers["sec-fetch-site"])
	assert.NotContains(t, got.Headers, "origin")
	assert.NotContains(t, got.Headers, "x-xp-forwarded-for", "the signed-in browser sends none")
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
// Mutation: wire.go shape — skip the fetch metadata for requests without a
// csrf token => RED.
func TestChromeWire_GuestAndLoginFlowCarryBrowserHeaders(t *testing.T) {
	c, bc, rec, _ := chromeClient(t)
	check := func(name string) {
		t.Helper()
		got := rec.last()
		for _, k := range []string{"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "accept-encoding", "accept-language", "accept"} {
			assert.NotEmpty(t, got.Headers[k], "%s: %s", name, k)
		}
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

// Priority and the HTTP/2 weight arrive together, from the transport's fetch
// mode, and only for current identities: `priority: u=1, i` with weight 220 for
// wireChrome; neither for legacy. go-twitter never sets the header itself.
//
// Mutation: wire.go chromeClientOptions — empty (no WithFetchMode) => RED (a
// current identity sends no priority and the navigation weight).
func TestChromeWire_PriorityAndWeightFollowTheGeneration(t *testing.T) {
	c, bc, rec, ua := chromeClient(t)
	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET", "https://x.com/i/api/graphql/q/Op", twitterHeaders("AUTH", "CT0", ua), nil)
	assert.Equal(t, "u=1, i", rec.last().Headers["priority"])
	assert.True(t, rec.cfg.FetchMode, "HTTP/2 weight 220 (fetch mode) is on for a current identity")
	assert.Equal(t, "priority", chromeHeaderOrder[len(chromeHeaderOrder)-1], "priority stays last, where Chrome writes it")
	assert.Equal(t, "priority", oracleXHRGet[len(oracleXHRGet)-1])
	assert.Equal(t, chromeAcceptEncoding, rec.last().Headers["accept-encoding"])
	assert.Contains(t, rec.last().Headers["accept-encoding"], "zstd")

	h := map[string]string{}
	wireChrome.shape("GET", "https://x.com/i/api/graphql/q/Op", h)
	assert.NotContains(t, h, "priority", "go-twitter leaves priority to the transport")

	lc, lbc, lrec := legacyClient(t)
	_, _, _, _ = lc.doRequestWithBody(context.Background(), nil, lbc, "GET", apiGraphQL, twitterHeaders("AUTH", "CT0", legacyProfile.UserAgent), nil)
	assert.NotContains(t, lrec.last().Headers, "priority")
	assert.NotContains(t, lrec.last().Headers["accept-encoding"], "zstd")
	assert.False(t, lrec.cfg.FetchMode)
}

// The clients a Client builds get the fetch mode by generation: current
// accounts yes; the shared account-less client, a legacy-profile account and an
// account whose profile is unstated, no.
//
// Mutation: client.go buildAccountClient — pass chromeClientOptions whatever
// the account's generation => RED (the chrome_131 and unpaired accounts get
// fetch mode).
func TestFetchMode_OnlyForCurrentIdentities(t *testing.T) {
	win := chromeProfile(t, stealth.ProfileChrome152, "chrome", "windows")
	rec := &wireRecorder{}
	c, err := NewClient(ClientConfig{
		Accounts: []*Account{
			{Username: "cur", AuthToken: "t", CT0: "c", Profile: win, UserAgent: win.UserAgent},
			{Username: "old", AuthToken: "t", CT0: "c", Profile: legacyProfile, UserAgent: legacyProfile.UserAgent},
			{Username: "odd", AuthToken: "t", CT0: "c", UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Chrome/123.0.0.0 Safari/537.36"},
		},
		stealthOpts: []stealth.ClientOption{stealth.WithBackend(rec.factory)},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.Len(t, rec.cfgs, 4)
	var got []bool
	for _, cfg := range rec.cfgs {
		got = append(got, cfg.FetchMode)
	}
	assert.Equal(t, []bool{false, true, false, false}, got, "shared, cur, old, odd")
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
	assert.Equal(t, wireLegacy, (*clientSet)(nil).wireOf(nil))
	assert.Equal(t, wireLegacy, (&clientSet{}).wireOf(&stealth.BrowserClient{}), "an unregistered client is legacy")
}

// An account's generation follows the profile STATED on it (set by the caller or
// paired with its user agent), never the transport's defaulted profile: an
// account whose profile was not stated and could not be paired stays legacy.
//
// Mutation: wire.go accountWire — fall back to defaultTLSProfile when the
// account states none => RED (the unpaired-UA account gets the Chrome shape).
func TestAccountWire_FollowsTheStatedProfile(t *testing.T) {
	win := chromeProfile(t, stealth.ProfileChrome152, "chrome", "windows")
	mk := func(a *Account) *Client {
		c, err := NewClient(ClientConfig{Accounts: []*Account{a}, DefaultProxy: deadProxy})
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	// A user agent no built-in profile presents: no profile can be paired.
	odd := &Account{Username: "odd", AuthToken: "t", CT0: "c", UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Chrome/123.0.0.0 Safari/537.36"}
	c := mk(odd)
	assert.Empty(t, odd.Profile.TLSProfile)
	assert.Equal(t, wireLegacy, c.owned.wireOf(odd.client), "unpaired UA stays legacy")

	// A user agent a built-in profile presents is paired, so it is stated.
	paired := &Account{Username: "paired", AuthToken: "t", CT0: "c", UserAgent: win.UserAgent}
	c = mk(paired)
	assert.Equal(t, stealth.ProfileChrome152, paired.Profile.TLSProfile)
	assert.Equal(t, wireChrome, c.owned.wireOf(paired.client))

	// No profile, no UA: derived, hence current.
	derived := &Account{Username: "derived", AuthToken: "t", CT0: "c"}
	c = mk(derived)
	assert.Equal(t, wireChrome, c.owned.wireOf(derived.client))

	// An account added after NewClient gets its client lazily, with the same rule.
	c, err := NewClient(ClientConfig{DefaultProxy: deadProxy})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	lateOdd := &Account{Username: "lateodd", AuthToken: "t", CT0: "c", UserAgent: odd.UserAgent}
	bc, err := c.clientForAccount(lateOdd)
	require.NoError(t, err)
	assert.Equal(t, wireLegacy, c.owned.wireOf(bc))
	late := &Account{Username: "late", AuthToken: "t", CT0: "c"}
	bc, err = c.clientForAccount(late)
	require.NoError(t, err)
	assert.Equal(t, wireChrome, c.owned.wireOf(bc))

	// An explicitly stated legacy profile stays legacy.
	old := &Account{Username: "old", AuthToken: "t", CT0: "c", Profile: legacyProfile}
	c = mk(old)
	assert.Equal(t, wireLegacy, c.owned.wireOf(old.client))
}

// Every identity this package can derive for an account without one is a
// profile wireGenFor knows (a derived identity never lands on the frozen shape
// by omission).
func TestWireGen_DerivedIdentitiesAreCurrent(t *testing.T) {
	for _, p := range identityCandidates() {
		assert.Equal(t, wireChrome, wireGenFor(p.TLSProfile), "%s/%s/%s", p.Browser, p.OS, p.TLSProfile)
	}
}

// The transport-level options reach current identities only: whatever
// chromeClientOptions holds (WithFetchMode once go-stealth has it) is never
// applied to a legacy generation.
//
// Mutation: wire.go wireClientOptions — drop the generation check => RED.
func TestWireClientOptions_NeverForLegacy(t *testing.T) {
	old := chromeClientOptions
	chromeClientOptions = []stealth.ClientOption{stealth.WithTimeout(1)}
	t.Cleanup(func() { chromeClientOptions = old })
	assert.Len(t, wireClientOptions(wireChrome), 1)
	assert.Empty(t, wireClientOptions(wireLegacy))
}

// The shared account-less client (the guest-token fallback) keeps the v0.8.6
// shape unless the consumer opts in, whatever TLS profile go-stealth gives it by
// default: a deployment that hosts only legacy accounts must not start
// presenting the new shape from its guest fallback.
//
// Mutation: wire.go accountlessWire — drop the ChromeWireForAccountlessClients
// check (always follow the default profile) => RED on the default-config rows.
func TestAccountlessClient_LegacyUnlessOptedIn(t *testing.T) {
	var recCfg stealth.BackendConfig
	run := func(t *testing.T, optIn bool) (guestGET, activate recordedReq) {
		rec := &wireRecorder{}
		c, err := NewClient(ClientConfig{
			ChromeWireForAccountlessClients: optIn,
			stealthOpts: []stealth.ClientOption{
				stealth.WithBackend(rec.factory),
			},
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		// No pooled account: the call falls back to a guest token.
		_, _, _ = c.doPoolRequest(context.Background(), "GET", "TestEndpoint", "https://x.com/i/api/graphql/test/TestOp", nil)
		for _, r := range rec.reqs {
			switch {
			case strings.HasSuffix(r.URL, "/1.1/guest/activate.json"):
				activate = r
			case strings.Contains(r.URL, "/graphql/") && r.Headers["x-guest-token"] != "":
				guestGET = r
			}
		}
		recCfg = rec.cfg
		require.NotEmpty(t, activate.URL, "guest fallback never activated a token")
		require.NotEmpty(t, guestGET.URL, "guest fallback never sent the GraphQL GET")
		return guestGET, activate
	}
	keys := func(h map[string]string) []string {
		var out []string
		for k := range sansGenerated(h) {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	wantKeys := func(h map[string]string) []string { return keys(h) }

	get, act := run(t, false)
	assert.Equal(t, legacyOrder, get.Order, "guest GET order is the v0.8.6 list")
	assert.Equal(t, legacyOrder, act.Order)
	assert.Equal(t, wantKeys(legacyGuestHeaders()), keys(get.Headers), "guest GET header set is v0.8.6's")
	assert.Equal(t, wantKeys(legacyActivateHeaders()), keys(act.Headers), "activate header set is v0.8.6's")
	assert.Equal(t, "gzip, deflate, br", get.Headers["accept-encoding"])
	assert.NotContains(t, get.Headers, "sec-fetch-site")
	assert.NotContains(t, get.Headers, "priority", "no fetch mode on the legacy account-less client")
	assert.False(t, recCfg.FetchMode)

	get, act = run(t, true)
	assert.NotEqual(t, legacyOrder, get.Order, "opted in: the Chrome order")
	assert.Equal(t, "u=1, i", get.Headers["priority"], "opted in: fetch mode")
	assert.True(t, recCfg.FetchMode)
	assert.Equal(t, "cross-site", act.Headers["sec-fetch-site"])
	assert.Contains(t, get.Headers, "sec-fetch-site")
}
