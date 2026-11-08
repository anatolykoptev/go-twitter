package twitter

import (
	"fmt"
	"maps"
	"net/url"
	"os"

	"github.com/anatolykoptev/go-twitter/internal/operations"
	"github.com/anatolykoptev/go-twitter/internal/queryid"
	"github.com/anatolykoptev/go-twitter/internal/xtidprobe"
)

// twitterBase is the GraphQL root. A var (not const) only so tests can point the
// client at an httptest server; production code never assigns it.
var twitterBase = "https://x.com/i/api/graphql"

// twitterAPIURL is the REST/login root. A var (not const) only so tests can
// point the login flow at an httptest server; production code never assigns it.
var twitterAPIURL = legacyAPIURL

// legacyAPIURL is the REST/login host of the frozen wire generation. The web
// client of a current identity uses api.x.com (see apiHostFor).
const legacyAPIURL = "https://api.twitter.com"

const (

	// accountSettingsURL is the authenticated REST endpoint used by
	// ValidateAccount to check whether an account's credentials are still
	// alive. Returns 200 on success, 401 on expired auth, 403 on suspension.
	accountSettingsURL = "https://api.twitter.com/1.1/account/settings.json"

	// T5.6 DM cluster — 1.1 REST endpoints (same host the lib already uses for
	// auth/media/settings). DMs are NOT GraphQL ops here, so they live as plain
	// URL consts (like accountSettingsURL), not in the Endpoints map.
	//
	// dmInboxURL: GET inbox metadata. CONFIRMED against
	// trevorhobenshield/twitter-api-client Account.dm_inbox (v1_api =
	// "https://api.twitter.com/1.1", path dm/inbox_initial_state.json).
	dmInboxURL = "https://api.twitter.com/1.1/dm/inbox_initial_state.json"
	// dmNewURL: POST a new DM into an existing conversation. CONFIRMED against
	// d60/twikit v11.dm_new (Endpoint.DM_NEW = .../1.1/dm/new2.json), flat JSON
	// body {conversation_id,text,...}. (NOT the public events/new.json shape.)
	dmNewURL = "https://api.twitter.com/1.1/dm/new2.json"
)

// bearerTokens is the list of known Twitter web-app bearer tokens.
var bearerTokens = []string{
	"AAAAAAAAAAAAAAAAAAAAANRILgAAAAAAnNwIzUejRCOuH5E6I8xnZz4puTs%3D1Zv7ttfk8LF81IUq16cHjhLTvJu4FA33AGWWjCpTnA",
	"AAAAAAAAAAAAAAAAAAAAAFQODgEAAAAAVHTp76lzh3rFzcHbmHVvQxYYpTw%3DckAlMINMjmCwxUcaXbAN4XqJVdgMJaHqNOFgPMK0zN1qLqLQCF",
}

// BearerToken is the active bearer token (first in list).
var BearerToken = bearerTokens[0]

// Endpoint holds the operation ID, path template, and per-operation feature flags.
type Endpoint struct {
	ID       string
	Name     string
	Features map[string]any
}

// URL returns the full URL for this endpoint. The variable ID segment is
// path-escaped as defense-in-depth: a poisoned queryID must never break out of
// its path segment. e.Name is a hardcoded const, so it is left verbatim.
func (e Endpoint) URL() string {
	return fmt.Sprintf("%s/%s/%s", twitterBase, url.PathEscape(e.ID), e.Name)
}

// EndpointURL returns the URL for a named operation, or an error if unknown.
func EndpointURL(operation string) (string, error) {
	if _, ok := Endpoints[operation]; !ok {
		return "", fmt.Errorf("unknown operation: %s", operation)
	}
	return endpointFor(operation).URL(), nil
}

// Endpoints maps operation names to their current GraphQL IDs and feature flags.
// The committed table and the generated queryIDs live in internal/operations.
var Endpoints = endpointsFromTable(operations.Table())

func endpointsFromTable(t map[string]operations.Operation) map[string]Endpoint {
	out := make(map[string]Endpoint, len(t))
	for name, op := range t {
		out[name] = Endpoint(op)
	}
	return out
}

// ApplyEnvOverrides reads TWITTER_QID_* env vars and overrides queryIds in Endpoints.
// Called automatically by init(); can also be called manually in tests.
func ApplyEnvOverrides() {
	pinned := make(map[string]struct{})
	for name, envKey := range operations.EnvOverrideKeys {
		if qid := os.Getenv(envKey); qid != "" {
			if ep, ok := Endpoints[name]; ok {
				ep.ID = qid
				Endpoints[name] = ep
				pinned[name] = struct{}{}
			}
		}
	}
	// A pinned op is never replaced by a runtime-refreshed id (see endpointFor).
	envPinned.Store(&pinned)
}

// applyGeneratedOverrides applies the cmd/gql-sync-generated queryIDs (in
// queryids_gen.go) over the committed literals in Endpoints. It runs before
// ApplyEnvOverrides so the final priority is env > generated > committed. Ops
// absent from generatedQueryIDs keep their committed literal; empty generated
// IDs are skipped (a partial/blank sync never blanks an endpoint).
//
// It only UPDATES the ID of ops already present in Endpoints; a generated op
// with no committed Endpoints entry is intentionally NOT added. Endpoints is the
// authoritative set of operations this client issues — sync refreshes IDs for
// known ops, it never introduces new endpoints.
func applyGeneratedOverrides() {
	for name, id := range operations.GeneratedQueryIDs {
		if id == "" {
			continue
		}
		if ep, ok := Endpoints[name]; ok { // intentional: update known ops only, never add
			ep.ID = id
			Endpoints[name] = ep
		}
	}
}

func init() {
	applyGeneratedOverrides() // generated overrides committed literal
	ApplyEnvOverrides()       // env overrides generated (highest priority)
}

// gqlFeatures returns the canonical Twitter GraphQL feature flags. It prefers the
// cmd/gql-sync-generated baseline (features_gen.go) when present, falling back to
// the committed literal when generation has not run (or produced nothing). The
// returned map is always a fresh COPY so a caller mutating per-op features can
// never corrupt the shared package-level source. A runtime-refreshed baseline
// (internal/queryid) takes precedence over the compiled one.
func gqlFeatures() map[string]any {
	if snap := queryid.Active.Load(); snap != nil && snap.Features != nil {
		return maps.Clone(snap.Features)
	}
	return compiledGQLFeatures()
}

// compiledGQLFeatures is the compiled baseline: the generated set when present,
// else the committed literal. Fresh copy on every call.
func compiledGQLFeatures() map[string]any { return operations.CompiledGQLFeatures() }

// bookmarkFeatures returns the shared baseline feature set plus the
// graphql_timeline_v2_bookmark_timeline flag the Bookmarks op requires (without
// it x.com returns the legacy bookmark shape / a 400). gqlFeatures() already
// returns a fresh copy, so mutating it here never corrupts the shared baseline.
func bookmarkFeatures() map[string]any {
	f := gqlFeatures()
	f["graphql_timeline_v2_bookmark_timeline"] = true
	return f
}

func init() { xtidprobe.GraphQLFeatures = gqlFeatures }
