package twitter

import (
	"strconv"
)

// endpointDefaultLimit holds the per-account-per-15-min request caps for each
// GraphQL operation, live-captured from x.com's x-rate-limit-limit headers on
// 2026-06-20 (4 accounts, independent per-account `remaining` counters ⇒ the
// limits are per-account-per-endpoint, not pool-wide). These seed each
// account's per-endpoint limiter so the very first burst is sized correctly;
// every response then refines the cap via Account.SyncRateLimit. Unlisted
// endpoints fall back to defaultEndpointLimit (conservative).
//
// The previous static 50/15m cap was 3.7x too low for Followers/Following and
// 10x too low for Retweeters, which starved the scrapers and tripped
// "all accounts unavailable" storms. These are the real ceilings.
var endpointDefaultLimit = map[string]int{
	"UserByScreenName": 150,
	"Followers":        187,
	"Following":        187,
	"Retweeters":       500,
	"UserTweets":       50,
}

// defaultEndpointLimit is the conservative fallback cap for any endpoint not in
// endpointDefaultLimit (unmeasured). It is intentionally low — the adaptive
// SyncRateLimit raises it the moment x.com reports the real limit, so a
// too-low seed only delays the first few requests, never exceeds the cap.
const defaultEndpointLimit = 50

// seedRateLimits primes an account's per-endpoint limiter with the measured
// per-endpoint defaults so the first window is sized to X's real ceilings
// instead of the generic static cap. Idempotent; safe to call once at startup.
func seedRateLimits(acc *Account) {
	for endpoint, limit := range endpointDefaultLimit {
		acc.UpdateEndpointLimit(endpoint, limit)
	}
}

// xRateLimitLimit is the response header carrying the per-account-per-endpoint
// request ceiling for the current 15-minute window. x.com (and the canonical
// Twitter client libraries) emit it lowercase.
const xRateLimitLimit = "x-rate-limit-limit"

// parseRateLimitLimit extracts a positive integer cap from the
// x-rate-limit-limit header value. Returns (0, false) when the header is
// absent, malformed, or non-positive so the caller leaves the existing cap
// untouched (never collapsing a key to deny-everything).
func parseRateLimitLimit(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
