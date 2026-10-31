package twitter

import (
	"strconv"
)

// endpointDefaultLimit holds the per-account-per-15-min request caps for each
// GraphQL operation, live-captured from x.com's x-rate-limit-limit headers on
// 2026-06-20 (4 accounts, independent per-account `remaining` counters ⇒ the
// limits are per-account-per-endpoint, not pool-wide). These seed each
// account's per-endpoint limiter so the very first burst is sized correctly;
// every response then refines the cap via Account.SyncRateLimit. Only measured
// operations are listed — endpoints stay unlisted until X reports a real
// ceiling, and an unlisted endpoint's cap is the account's rate-limiter config
// (ClientConfig.RateLimit.RequestsPerWindow).
//
// The previous static 50/15m cap was 3.7x too low for Followers/Following and
// 10x too low for Retweeters, which starved the scrapers and tripped
// "all accounts unavailable" storms. These are the real ceilings.
var endpointDefaultLimit = map[string]int{
	"UserByScreenName":   150,
	"Followers":          187,
	"Following":          187,
	"Retweeters":         500,
	"UserTweets":         50,
	"SearchTimeline":     187,
	"HomeLatestTimeline": 500,
	deviceFollowEndpoint: 180,
}

// EndpointLimits returns the measured per-account-per-15-min request caps the
// client seeds every account's per-endpoint limiter with (see
// endpointDefaultLimit). It returns a COPY: mutating the result does not change
// the table or what new accounts are seeded with. The map holds only measured
// ceilings; an endpoint absent from it falls back to the account's configured
// limiter cap (ClientConfig.RateLimit.RequestsPerWindow), not to a fixed
// constant.
func EndpointLimits() map[string]int {
	out := make(map[string]int, len(endpointDefaultLimit))
	for endpoint, limit := range endpointDefaultLimit {
		out[endpoint] = limit
	}
	return out
}

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
