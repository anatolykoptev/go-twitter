// Package xwebnative is the single list of GraphQL operations go-twitter issues
// against x.com's x-web (Vite + Relay) stack, as opposed to the legacy
// responsive-web stack. Both the runtime queryId refresher and cmd/xweb-report
// read it, so the two can never disagree about which operations take an x-web id.
package xwebnative

// Ops returns operation name (a key of twitter.Endpoints, equal to the Relay
// operation name) -> the Relay operation kind its artifact must declare. It is a
// fresh map on every call.
//
// Adding an entry is a request-shape change, not a bookkeeping one: the
// operation must already send x-web-shaped variables (plus its provided
// variables) and parse the x-web response. A later migration phase adds an
// operation here in the same change that switches its request.
func Ops() map[string]string {
	return map[string]string{
		"tweetNotificationsQuery": "query",
	}
}
