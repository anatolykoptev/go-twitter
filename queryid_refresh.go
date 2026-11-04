package twitter

import (
	"maps"
	"reflect"
	"sync/atomic"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/internal/queryid"
)

// Runtime queryId/feature refresh (#68): the refresher itself lives in
// internal/queryid; this file is the Client wiring and the root's side of the
// precedence chain. Precedence per operation: TWITTER_QID_* env override >
// runtime-refreshed > generated (internal/operations/queryids_gen.go) >
// committed literal (internal/operations/endpoints.go).

// rootCatalog is the root's endpoint table as internal/queryid sees it; every
// func reads the live table on each call.
var rootCatalog = queryid.Catalog{
	Operations: knownOperations,
	Known: func(op string) bool {
		_, ok := Endpoints[op]
		return ok
	},
	CountChanged: countChangedIDs,
}

// defaultQIDConfig is the production refresher config with the root's catalog.
func defaultQIDConfig() queryid.Config {
	cfg := queryid.DefaultConfig()
	cfg.Catalog = rootCatalog
	return cfg
}

// newQIDRefresher builds a refresher over the root's endpoint table.
func newQIDRefresher(cfg queryid.Config) *queryid.Refresher {
	cfg.Catalog = rootCatalog
	return queryid.New(cfg)
}

var processQIDPtr atomic.Pointer[queryid.Refresher]

// processQID returns the process-wide refresher, creating it on first use.
func processQID() *queryid.Refresher {
	if r := processQIDPtr.Load(); r != nil {
		return r
	}
	r := newQIDRefresher(defaultQIDConfig())
	if processQIDPtr.CompareAndSwap(nil, r) {
		return r
	}
	return processQIDPtr.Load()
}

// qidRefresh returns the refresher this client reports to: the process-wide
// one, unless a test injected its own.
func (c *Client) qidRefresh() *queryid.Refresher {
	if c.qid != nil {
		return c.qid
	}
	return processQID()
}

// noteEndpointFault reports one errEndpoint response for op to the refresher,
// with the credentials of the account that saw it (its cookie authenticates the
// refresh's page fetch; the cookie is never logged).
func (c *Client) noteEndpointFault(op string, acc *Account, bc *stealth.BrowserClient, authTok, ct0, ua string) {
	r := c.qidRefresh()
	r.OnEndpointFault(op, func() bundle.Fetcher {
		return r.Shared(c.warmFetcherFor(acc, bc, twitterHeaders(authTok, ct0, ua)))
	}, c.metrics.instruments())
}

// observeWarmPage is the xtid.Manager warm-page hook: it hands the page xtid
// just fetched to the process-wide refresher.
func observeWarmPage(_ string, page []byte) { processQID().ObservePage(page) }

// knownOperations is the set of operations this library issues.
func knownOperations() []string {
	ops := make([]string, 0, len(Endpoints))
	for name := range Endpoints {
		ops = append(ops, name)
	}
	return ops
}

// countChangedIDs counts applied ids that differ from the compiled set.
func countChangedIDs(ids map[string]string) int {
	n := 0
	for op, id := range ids {
		if ep, ok := Endpoints[op]; ok && ep.ID != id && !isEnvPinned(op) {
			n++
		}
	}
	return n
}

// baselineFeatureOps are the operations whose compiled Features are the shared
// gqlFeatures() baseline, computed once at init; only those follow a refreshed
// baseline (Followers/Following carry their own, deliberately different set).
var baselineFeatureOps = func() map[string]bool {
	out := make(map[string]bool, len(Endpoints))
	base := compiledGQLFeatures()
	for name, ep := range Endpoints {
		out[name] = reflect.DeepEqual(ep.Features, base)
	}
	return out
}()

// envPinned is the set of operations with a TWITTER_QID_* override in force,
// rebuilt by every ApplyEnvOverrides. An env override always wins.
var envPinned atomic.Pointer[map[string]struct{}]

func isEnvPinned(op string) bool {
	p := envPinned.Load()
	if p == nil {
		return false
	}
	_, ok := (*p)[op]
	return ok
}

// endpointFor returns the endpoint for op as it should be requested NOW:
// Endpoints (env > generated > committed) with the runtime-refreshed id and
// feature baseline layered between env and generated. Lock-free: two atomic
// loads. Unknown ops return the zero Endpoint like a plain map lookup.
func endpointFor(op string) Endpoint {
	ep, ok := Endpoints[op]
	if !ok {
		return ep
	}
	snap := queryid.Active.Load()
	if snap == nil {
		return ep
	}
	if !isEnvPinned(op) {
		if id, ok := snap.IDs[op]; ok {
			ep.ID = id
		}
	}
	if snap.Features != nil && baselineFeatureOps[op] {
		ep.Features = maps.Clone(snap.Features)
	}
	return ep
}
