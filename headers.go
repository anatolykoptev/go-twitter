package twitter

import stealth "github.com/anatolykoptev/go-stealth"

// defaultUserAgent is the fallback User-Agent when no per-account UA is set.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

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
		"referer":                   "https://twitter.com/",
		"origin":                    "https://twitter.com",
		"sec-fetch-dest":            "empty",
		"sec-fetch-mode":            "cors",
		"sec-fetch-site":            "same-origin",
	}
	if ch := stealth.ClientHintsHeaders(userAgent); ch != nil {
		for k, v := range ch {
			h[k] = v
		}
	}
	return h
}

// guestHeaders returns headers for unauthenticated (guest token) requests.
func guestHeaders(guestToken string) map[string]string {
	return map[string]string{
		"authorization":             "Bearer " + BearerToken,
		"x-guest-token":             guestToken,
		"x-twitter-active-user":     "yes",
		"x-twitter-client-language": "en",
		"content-type":              "application/json",
		"user-agent":                defaultUserAgent,
		"accept":                    "*/*",
		"accept-language":           "en-US,en;q=0.9",
		"accept-encoding":           "gzip, deflate, br",
		"referer":                   "https://twitter.com/",
		"origin":                    "https://twitter.com",
	}
}

// loginFlowHeaders returns headers required for the login flow API.
func loginFlowHeaders(guestToken, ct0 string) map[string]string {
	h := map[string]string{
		"authorization":             "Bearer " + BearerToken,
		"content-type":              "application/json",
		"x-guest-token":             guestToken,
		"x-twitter-active-user":     "yes",
		"x-twitter-client-language": "en",
		"user-agent":                defaultUserAgent,
		"accept":                    "*/*",
		"accept-language":           "en-US,en;q=0.9",
		"referer":                   "https://twitter.com/",
		"origin":                    "https://twitter.com",
	}
	if ct0 != "" {
		h["x-csrf-token"] = ct0
	}
	return h
}

// twitterHeaderOrder is the Twitter-specific header order for TLS fingerprint consistency.
var twitterHeaderOrder = []string{
	"authorization",
	"content-type",
	"x-csrf-token",
	"x-twitter-active-user",
	"x-twitter-client-language",
	"x-client-transaction-id",
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
