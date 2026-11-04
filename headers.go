package twitter

import (
	stealth "github.com/anatolykoptev/go-stealth"
)

// defaultUserAgent is the fallback User-Agent where no client exists to ask.
// It is the UA go-stealth pairs with its own default TLS profile, so it moves
// with go-stealth's default instead of drifting from it; clients that exist
// report their own (clientUA).
var defaultUserAgent = stealthDefaultUserAgent()

// twitterHeaders returns the base headers required by Twitter's GraphQL API.
func twitterHeaders(authToken, ct0, userAgent string) map[string]string {
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	h := map[string]string{
		"authorization":             "Bearer " + BearerToken,
		"x-csrf-token":              ct0,
		"x-twitter-active-user":     "yes",
		"x-twitter-auth-type":       "OAuth2Session",
		"x-twitter-client-language": "en",
		"content-type":              "application/json",
		"cookie":                    "auth_token=" + authToken + "; ct0=" + ct0,
		"user-agent":                userAgent,
		"accept":                    "*/*",
		"accept-language":           "en-US,en;q=0.9",
		"accept-encoding":           "gzip, deflate, br",
		"referer":                   "https://x.com/",
		"origin":                    "https://x.com",
		"sec-fetch-dest":            "empty",
		"sec-fetch-mode":            "cors",
		"sec-fetch-site":            "same-origin",
	}
	return withClientHints(h, userAgent)
}

// withClientHints adds the Client Hints a Chromium browser with userAgent sends
// beside it (none for other browsers) and returns h.
func withClientHints(h map[string]string, userAgent string) map[string]string {
	for k, v := range stealth.ClientHintsHeaders(userAgent) {
		h[k] = v
	}
	return h
}

// guestHeaders returns headers for unauthenticated (guest token) requests.
func guestHeaders(guestToken, userAgent string) map[string]string {
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	return withClientHints(map[string]string{
		"authorization":             "Bearer " + BearerToken,
		"x-guest-token":             guestToken,
		"x-twitter-active-user":     "yes",
		"x-twitter-client-language": "en",
		"content-type":              "application/json",
		"user-agent":                userAgent,
		"accept":                    "*/*",
		"accept-language":           "en-US,en;q=0.9",
		"accept-encoding":           "gzip, deflate, br",
		"referer":                   "https://x.com/",
		"origin":                    "https://x.com",
	}, userAgent)
}

// loginFlowHeaders returns headers required for the login flow API. userAgent
// is the one the sending client presents (see clientUA); empty means the default.
func loginFlowHeaders(guestToken, ct0, userAgent string) map[string]string {
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	h := map[string]string{
		"authorization":             "Bearer " + BearerToken,
		"content-type":              "application/json",
		"x-guest-token":             guestToken,
		"x-twitter-active-user":     "yes",
		"x-twitter-client-language": "en",
		"user-agent":                userAgent,
		"accept":                    "*/*",
		"accept-language":           "en-US,en;q=0.9",
		"referer":                   "https://x.com/",
		"origin":                    "https://x.com",
	}
	if ct0 != "" {
		h["x-csrf-token"] = ct0
	}
	return withClientHints(h, userAgent)
}

// twitterHeaderOrder is the Twitter-specific header order for TLS fingerprint consistency.
var twitterHeaderOrder = []string{
	"authorization",
	"content-type",
	"x-csrf-token",
	"x-twitter-active-user",
	"x-twitter-client-language",
	"x-client-transaction-id",
	"x-xp-forwarded-for",
	"sec-ch-ua",
	"sec-ch-ua-mobile",
	"sec-ch-ua-platform",
	"sec-fetch-dest",
	"sec-fetch-mode",
	"sec-fetch-site",
	"cookie",
	"user-agent",
	"accept",
	"accept-language",
	"accept-encoding",
}
