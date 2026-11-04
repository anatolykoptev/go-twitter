package twitter

import (
	"context"
	"strings"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins the request shape of v0.8.6 for accounts whose identity is the
// legacy one (TLS profile chrome_131). Those accounts are live: every header,
// its value and its position must stay byte-identical, so a change to the wire
// shape reaches only accounts on current identities. The golden data below was
// recorded from v0.8.6 itself, before the wire-generation switch existed; do not
// regenerate it from the code under test.

const legacyBearer = "Bearer AAAAAAAAAAAAAAAAAAAAANRILgAAAAAAnNwIzUejRCOuH5E6I8xnZz4puTs%3D1Zv7ttfk8LF81IUq16cHjhLTvJu4FA33AGWWjCpTnA"

var legacyProfile = stealth.BrowserProfile{
	UserAgent:  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	TLSProfile: stealth.ProfileChrome131, Browser: "chrome", OS: "windows",
}

// v0.8.6 twitterHeaderOrder.
var legacyOrder = []string{
	"authorization", "content-type", "x-csrf-token", "x-twitter-active-user", "x-twitter-client-language",
	"x-client-transaction-id", "x-xp-forwarded-for", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
	"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "cookie", "user-agent", "accept", "accept-language",
	"accept-encoding",
}

const (
	legacyCH     = `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`
	legacyPlat   = `"Windows"`
	legacyAccept = "*/*"
	legacyLang   = "en-US,en;q=0.9"
	legacyAE     = "gzip, deflate, br"
)

func legacyAuthHeaders() map[string]string {
	return map[string]string{
		"accept": legacyAccept, "accept-encoding": legacyAE, "accept-language": legacyLang,
		"authorization": legacyBearer, "content-type": "application/json",
		"cookie": "auth_token=AUTH; ct0=CT0", "origin": "https://x.com", "referer": "https://x.com/",
		"sec-ch-ua": legacyCH, "sec-ch-ua-mobile": "?0", "sec-ch-ua-platform": legacyPlat,
		"sec-fetch-dest": "empty", "sec-fetch-mode": "cors", "sec-fetch-site": "same-origin",
		"user-agent": legacyProfile.UserAgent, "x-csrf-token": "CT0", "x-twitter-active-user": "yes",
		"x-twitter-auth-type": "OAuth2Session", "x-twitter-client-language": "en",
	}
}

func legacyGuestHeaders() map[string]string {
	return map[string]string{
		"accept": legacyAccept, "accept-encoding": legacyAE, "accept-language": legacyLang,
		"authorization": legacyBearer, "content-type": "application/json",
		"origin": "https://x.com", "referer": "https://x.com/",
		"sec-ch-ua": legacyCH, "sec-ch-ua-mobile": "?0", "sec-ch-ua-platform": legacyPlat,
		"user-agent": legacyProfile.UserAgent, "x-guest-token": "GT", "x-twitter-active-user": "yes",
		"x-twitter-client-language": "en",
	}
}

// legacyFlowHeaders is loginFlowHeaders: no accept-encoding of its own.
func legacyFlowHeaders() map[string]string {
	h := legacyGuestHeaders()
	delete(h, "accept-encoding")
	return h
}

func legacyActivateHeaders() map[string]string {
	return map[string]string{
		"authorization": legacyBearer, "content-type": "application/json",
		"sec-ch-ua": legacyCH, "sec-ch-ua-mobile": "?0", "sec-ch-ua-platform": legacyPlat,
		"user-agent": legacyProfile.UserAgent,
	}
}

const (
	wireGETHead = "authorization,content-type,x-csrf-token,x-twitter-active-user,x-twitter-client-language," +
		"x-client-transaction-id,x-xp-forwarded-for,sec-ch-ua,sec-ch-ua-mobile,sec-ch-ua-platform," +
		"sec-fetch-dest,sec-fetch-mode,sec-fetch-site,cookie,user-agent,accept,accept-language,accept-encoding"
	// Wire order on the HTTP/2 encoder: the listed headers, then the rest
	// lexicographically with canonical (upper-case) keys before lower-case ones.
	wireAuthGET  = wireGETHead + ",origin,referer,x-twitter-auth-type"
	wireAuthPOST = wireGETHead + ",origin,referer,x-twitter-auth-type,content-length"
	wireGuestGET = "authorization,content-type,x-twitter-active-user,x-twitter-client-language,sec-ch-ua,sec-ch-ua-mobile,sec-ch-ua-platform,user-agent,accept,accept-language,accept-encoding,origin,referer,x-guest-token"
	// The guest GET through the request layer also carries the generated ids,
	// both listed in the order, right after x-twitter-client-language.
	wireGuestGETSigned = "authorization,content-type,x-twitter-active-user,x-twitter-client-language,x-client-transaction-id,x-xp-forwarded-for,sec-ch-ua,sec-ch-ua-mobile,sec-ch-ua-platform,user-agent,accept,accept-language,accept-encoding,origin,referer,x-guest-token"
	wireFlowPOST       = wireGuestGET + ",content-length"
	wireActivate       = "authorization,content-type,sec-ch-ua,sec-ch-ua-mobile,sec-ch-ua-platform,user-agent,accept-encoding,content-length"
	apiGraphQL         = "https://x.com/i/api/graphql/q/Op"
	apiRESTGet         = "https://x.com/i/api/1.1/account/settings.json"
	apiGuestAct        = "https://api.twitter.com/1.1/guest/activate.json"
	apiFlowInit        = "https://api.twitter.com/1.1/onboarding/task.json?flow_name=login"
	apiFlowStep        = "https://api.twitter.com/1.1/onboarding/task.json"
	apiFlowWelcom      = "https://api.twitter.com/1.1/onboarding/task.json?flow_name=welcome"
)

// legacyClient returns a Client whose xtid warm-page fetch is served locally
// (no network) and a legacy-identity transport recorder.
func legacyClient(t *testing.T) (*Client, *stealth.BrowserClient, *wireRecorder) {
	t.Helper()
	c, err := NewClient(ClientConfig{DefaultProxy: deadProxy})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.warmDoerOverride = &fakeWarmBackend{}
	bc, rec := recorderClient(t, legacyProfile)
	return c, bc, rec
}

// sansGenerated drops the two per-request generated values (their presence is
// checked by the caller; their content is not part of the pin).
func sansGenerated(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if k != "x-client-transaction-id" && k != "x-xp-forwarded-for" {
			out[k] = v
		}
	}
	return out
}

func assertLegacyReq(t *testing.T, got recordedReq, method, url string, wantH map[string]string, wantWire string) {
	t.Helper()
	assert.Equal(t, method, got.Method)
	assert.Equal(t, url, got.URL)
	assert.Equal(t, legacyOrder, got.Order, "order list handed to the transport")
	assert.Equal(t, wantH, sansGenerated(got.Headers), "header set")
	assert.Equal(t, wantWire, strings.Join(wireOrder(got.Method, got.Headers, got.Order), ","), "wire order")
}

// Legacy freeze: a chrome_131 account sends exactly the v0.8.6 headers, values
// and order for an authenticated GET, an authenticated POST and a guest GET.
//
// Mutation: wire.go wireGenFor — return wireChrome for every profile (or drop
// chrome_131 from the legacy set) => RED (every case here gets the new order).
func TestLegacyFreeze_Requests(t *testing.T) {
	c, bc, rec := legacyClient(t)
	ctx := context.Background()

	// Authenticated GraphQL GET.
	_, _, _, _ = c.doRequestWithBody(ctx, nil, bc, "GET", apiGraphQL, twitterHeaders("AUTH", "CT0", legacyProfile.UserAgent), nil)
	got := rec.last()
	assert.NotEmpty(t, got.Headers["x-client-transaction-id"], "request went through the xtid path")
	assert.NotEmpty(t, got.Headers["x-xp-forwarded-for"], "request went through the xpff path")
	assertLegacyReq(t, got, "GET", apiGraphQL, legacyAuthHeaders(), wireAuthGET)

	// Authenticated REST GET: content-type is dropped, nothing else changes.
	_, _, _, _ = c.doRequestWithBody(ctx, nil, bc, "GET", apiRESTGet, twitterHeaders("AUTH", "CT0", legacyProfile.UserAgent), nil)
	wantREST := legacyAuthHeaders()
	delete(wantREST, "content-type")
	got = rec.last()
	assert.Equal(t, wantREST, sansGenerated(got.Headers))
	assert.Equal(t, legacyOrder, got.Order)

	// Authenticated GraphQL POST with a body.
	_, _, _, _ = c.doRequestWithBody(ctx, nil, bc, "POST", apiGraphQL, twitterHeaders("AUTH", "CT0", legacyProfile.UserAgent), strings.NewReader("{}"))
	assertLegacyReq(t, rec.last(), "POST", apiGraphQL, legacyAuthHeaders(), wireAuthPOST)

	// Guest GET.
	_, _, _, _ = c.doRequestWithBody(ctx, nil, bc, "GET", apiGraphQL, guestHeaders("GT", legacyProfile.UserAgent), nil)
	got = rec.last()
	assert.NotEmpty(t, got.Headers["x-client-transaction-id"])
	assertLegacyReq(t, got, "GET", apiGraphQL, legacyGuestHeaders(), wireGuestGETSigned)
}

// Legacy freeze for the unauthenticated flows: guest activation and the
// login-flow requests keep their v0.8.6 headers (including the absence of
// fetch metadata and of an explicit accept-encoding on the flow requests).
//
// Mutation: auth.go getGuestToken — send via the new generation's shaping
// regardless of the client's profile => RED.
func TestLegacyFreeze_AuthFlows(t *testing.T) {
	c, bc, rec := legacyClient(t)

	_, err := c.getGuestToken(bc)
	require.NoError(t, err)
	assertLegacyReq(t, rec.last(), "POST", apiGuestAct, legacyActivateHeaders(), wireActivate)

	_, err = c.initLoginFlowFull(bc, "GT")
	require.NoError(t, err)
	assertLegacyReq(t, rec.last(), "POST", apiFlowInit, legacyFlowHeaders(), wireFlowPOST)

	_, err = c.submitFlowStep(bc, "GT", `{"flow_token":"x"}`)
	require.NoError(t, err)
	assertLegacyReq(t, rec.last(), "POST", apiFlowStep, legacyFlowHeaders(), wireFlowPOST)

	_, _ = c.openAccountOn(bc) // welcome flow: activate, then the welcome POST
	welcome := legacyFlowHeaders()
	welcome["x-guest-token"] = "gt-test" // the token the recorder's activate answer carried
	assertLegacyReq(t, rec.last(), "POST", apiFlowWelcom, welcome, wireFlowPOST)
}
