package twitter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/internal/gqlextract"
	"github.com/anatolykoptev/go-twitter/xtid"
	"golang.org/x/sync/singleflight"
)

// Runtime queryId/feature refresh (#68).
//
// queryIds are compiled in, and the weekly codegen that refreshes them cannot
// reach X's authenticated bundle. When X retires an id every account gets an
// endpoint fault (#61) and nothing recovers until a release. The runtime
// already fetches the authenticated x.com page for xtid, so it can read the
// ids from the same bundle: the refresh walks that bundle, validates the
// extraction as a whole, and swaps an immutable snapshot in atomically.
//
// Precedence per operation: TWITTER_QID_* env override > runtime-refreshed >
// generated (queryids_gen.go) > committed literal (endpoints.go). The first
// failure keeps the last good snapshot; nothing here ever clears it.

const (
	// qidRefreshTTL is how long a successful refresh is trusted. 6h matches the
	// bundle chunk-map cache TTL (bundle.defaultCacheTTL) and keeps the cost to
	// at most four bundle walks a day per process, while X rotates ids at the
	// scale of days to weeks; the fault trigger covers the faster case.
	qidRefreshTTL = 6 * time.Hour

	// qidFaultThreshold / qidFaultWindow: K endpoint faults on ONE operation
	// within the window trigger a refresh. A single 404 can be a one-off; three
	// within two minutes on the same op is the signature of a dead id (one call
	// on a 3-account pool already produces three, and a *WithSocial caller that
	// stops after two faults per call reaches it on its second call).
	qidFaultThreshold = 3
	qidFaultWindow    = 2 * time.Minute

	// qidMinInterval is the minimum spacing between two refresh ATTEMPTS, success
	// or failure, from any trigger. A refresh walks up to qidMaxBundles chunks
	// through an account's client, so a 404 storm (X down, not an id rotation)
	// must not repeat that walk: at most 96 walks a day in the worst case, and a
	// failing bundle fetch backs off by the same interval.
	qidMinInterval = 15 * time.Minute

	// qidMinKnownFraction: a bundle that yields fewer than this share of the
	// operations this library knows is treated as partial (an unfinished chunk
	// walk, a bot wall) and rejected whole. The proven-extractable set today is 9
	// of the 19 known operations (47%, queryids_gen.go; the rest are session-only
	// or Relay-hash mutations), so 40% tolerates losing one and rejects half a
	// bundle.
	qidMinKnownFraction = 0.4

	// qidMinFeatureFraction: the same guard for the feature set, against the
	// compiled baseline.
	qidMinFeatureFraction = 0.75

	// qidRefreshTimeout bounds one refresh (page + chunk walk).
	qidRefreshTimeout = 3 * time.Minute

	// qidMaxBundles bounds the chunk walk; same cap as cmd/gql-sync so the two
	// extract from the same coverage.
	qidMaxBundles = 512

	// qidObservedPageMaxAge is how long the page and the chunks xtid fetched stay
	// reusable by the refresh: long enough to cover the background walk that
	// starts right after xtid's init, short enough that nothing stale is served.
	qidObservedPageMaxAge = 2 * time.Minute
	// qidChunkCacheBytes caps the shared chunk cache (bodies are held only for
	// qidObservedPageMaxAge).
	qidChunkCacheBytes = 32 << 20

	// envQIDRefresh set to "off" disables the runtime refresh (operator kill
	// switch, next to the per-op TWITTER_QID_* overrides).
	envQIDRefresh = "TWITTER_QID_REFRESH"
)

// Refresh triggers (twitter.trigger) and results (twitter.result). Bounded.
const (
	qidTriggerFirst = "first" // no successful refresh yet
	qidTriggerTTL   = "ttl"   // the active snapshot outlived qidRefreshTTL
	qidTriggerFault = "fault" // K endpoint faults on one operation

	qidResultOK          = "ok"
	qidResultFetchFailed = "fetch_failed" // page/bundle could not be read
	qidResultRejected    = "rejected"     // read, but failed validation; nothing applied
)

// errQIDRejected marks an extraction that failed validation.
var errQIDRejected = errors.New("queryid refresh: extraction rejected")

// activeQID is the process-wide refreshed snapshot endpointFor reads (nil = the
// compiled ids and features are active). It is global because the Endpoints
// registry it overlays is global: a refresh published by any refresher is seen
// by every client, lock-free.
var activeQID atomic.Pointer[qidSnapshot]

// qidSnapshot is one immutable refreshed state. Never mutated after Store.
type qidSnapshot struct {
	ids      map[string]string // known operation -> validated queryId
	features map[string]any    // refreshed baseline feature set; nil = no change
	at       time.Time
	changed  int // ids differing from the compiled set
}

// qidConfig holds the tunables; production uses the consts above, tests shrink
// them.
type qidConfig struct {
	ttl, faultWindow, minInterval, timeout time.Duration
	faultThreshold                         int
	disabled                               bool
}

func defaultQIDConfig() qidConfig {
	return qidConfig{
		ttl:            qidRefreshTTL,
		faultWindow:    qidFaultWindow,
		minInterval:    qidMinInterval,
		timeout:        qidRefreshTimeout,
		faultThreshold: qidFaultThreshold,
		disabled:       os.Getenv(envQIDRefresh) == "off",
	}
}

// warmPage is the part of the authenticated warm page the refresh needs.
type warmPage struct {
	at       time.Time
	scripts  []string
	defaults map[string]bool
}

// qidRefresher owns the active snapshot and the trigger bookkeeping. One
// instance lives for the whole process (processQID), so a refresh learned on
// one *WithSocial lease serves the next without another bundle fetch.
type qidRefresher struct {
	cfg     qidConfig
	now     func() time.Time
	started time.Time

	chunks *bundle.MemoCache // CDN chunks shared between xtid's scan and the refresh walk
	sf     singleflight.Group
	wg     sync.WaitGroup // in-flight refreshes; tests wait on it

	lastAttemptNS atomic.Int64
	lastSuccessNS atomic.Int64

	mu        sync.Mutex
	faults    map[string][]time.Time
	failing   bool
	alertHook func(topic string, payload any)
	page      *warmPage
}

func newQIDRefresher(cfg qidConfig) *qidRefresher {
	r := &qidRefresher{cfg: cfg, now: time.Now, faults: make(map[string][]time.Time)}
	r.started = r.now()
	r.chunks = bundle.NewMemoCache(qidObservedPageMaxAge, qidChunkCacheBytes, func() time.Time { return r.now() })
	return r
}

// shared wraps an authenticated warm fetcher so the chunks xtid's indices scan
// reads are served to the refresh's walk without a second request. nil stays
// nil (a guest request has no authenticated fetcher).
func (r *qidRefresher) shared(f bundle.Fetcher) bundle.Fetcher {
	if r == nil || r.cfg.disabled || f == nil {
		return f
	}
	return &bundle.MemoFetcher{Inner: f, Cache: r.chunks}
}

var processQIDPtr atomic.Pointer[qidRefresher]

// processQID returns the process-wide refresher, creating it on first use.
func processQID() *qidRefresher {
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
func (c *Client) qidRefresh() *qidRefresher {
	if c.qid != nil {
		return c.qid
	}
	return processQID()
}

// setAlertHook is wired by NewClient from ClientConfig.PoolAlertHook so
// the refresh-failed alert reaches the consumer's pool hook, once per
// ok->failed transition (the xtid init-failed pattern).
func (r *qidRefresher) setAlertHook(fn func(topic string, payload any)) {
	r.mu.Lock()
	r.alertHook = fn
	r.mu.Unlock()
}

// noteEndpointFault reports one errEndpoint response for op to the refresher,
// with the credentials of the account that saw it (its cookie authenticates the
// refresh's page fetch; the cookie is never logged).
func (c *Client) noteEndpointFault(op string, bc *stealth.BrowserClient, authTok, ct0, ua string) {
	r := c.qidRefresh()
	r.onEndpointFault(op, func() bundle.Fetcher {
		return r.shared(warmFetcher(c.warmClient(bc), twitterHeaders(authTok, ct0, ua)))
	}, c.metrics)
}

// observeWarmPage is the xtid.Manager warm-page hook: it hands the page xtid
// just fetched to the process-wide refresher.
func observeWarmPage(_ string, page []byte) { processQID().observePage(page) }

// observePage stores the authenticated warm page xtid just fetched, so the next
// refresh reuses it instead of requesting it again.
func (r *qidRefresher) observePage(page []byte) {
	if r == nil || r.cfg.disabled {
		return
	}
	wp := parseWarmPage(page, r.now())
	r.mu.Lock()
	r.page = wp
	r.mu.Unlock()
}

func parseWarmPage(page []byte, at time.Time) *warmPage {
	return &warmPage{
		at:       at,
		scripts:  bundle.ViteScripts(string(page), xtid.AuthedWarmPageURL),
		defaults: gqlextract.ParseFeatureDefaults(page),
	}
}

// onAuthedRequest runs on every request that carries an account's cookies (the
// authenticated warm-fetch condition): it samples the map age and fires the
// first/TTL trigger when due. f is the same authenticated fetcher xtid uses.
func (r *qidRefresher) onAuthedRequest(f bundle.Fetcher, m *clientMetrics) {
	if r == nil || r.cfg.disabled || f == nil {
		return
	}
	now := r.now()
	m.recordQIDAge(r.age(now))
	if trig, due := r.lazyDue(now); due {
		r.fire(trig, f, m)
	}
}

// onEndpointFault counts an endpoint fault (errEndpoint, #61) against op and
// fires the fault trigger at K within the window. mk builds the fetcher lazily;
// a request without an account cookie has none and cannot refresh.
func (r *qidRefresher) onEndpointFault(op string, mk func() bundle.Fetcher, m *clientMetrics) {
	if r == nil || r.cfg.disabled {
		return
	}
	if _, known := Endpoints[op]; !known {
		return // REST names (DMInbox, ...) have no queryId to refresh
	}
	if !r.noteFault(op) {
		return
	}
	if f := mk(); f != nil {
		r.fire(qidTriggerFault, f, m)
	}
}

// noteFault records a fault and reports whether op reached the threshold; the
// op's history is cleared on a hit so the next trigger needs K fresh faults.
func (r *qidRefresher) noteFault(op string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	cut := now.Add(-r.cfg.faultWindow)
	recent := r.faults[op][:0]
	for _, t := range r.faults[op] {
		if t.After(cut) {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	if len(recent) >= r.cfg.faultThreshold {
		delete(r.faults, op)
		return true
	}
	r.faults[op] = recent
	return false
}

// lazyDue reports whether the first/TTL trigger is due now.
func (r *qidRefresher) lazyDue(now time.Time) (string, bool) {
	if !r.intervalOK(now) {
		return "", false
	}
	ls := r.lastSuccessNS.Load()
	switch {
	case ls == 0:
		return qidTriggerFirst, true
	case now.Sub(time.Unix(0, ls)) > r.cfg.ttl:
		return qidTriggerTTL, true
	}
	return "", false
}

// intervalOK is the minimum-interval gate: no attempt within minInterval of the
// previous one. Every trigger passes it before and inside the singleflight.
func (r *qidRefresher) intervalOK(now time.Time) bool {
	la := r.lastAttemptNS.Load()
	return la == 0 || now.Sub(time.Unix(0, la)) >= r.cfg.minInterval
}

// age is the age of the active map in seconds: since the last successful
// refresh, or since process start while the compiled ids are still active.
func (r *qidRefresher) age(now time.Time) float64 {
	since := r.started
	if s := activeQID.Load(); s != nil {
		since = s.at
	}
	return now.Sub(since).Seconds()
}

// fire starts a refresh in the background if the interval allows it. The
// request that hit the fault (or tripped the TTL) never waits for it.
func (r *qidRefresher) fire(trig string, f bundle.Fetcher, m *clientMetrics) {
	if !r.intervalOK(r.now()) {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		_, _, _ = r.sf.Do("refresh", func() (any, error) {
			r.run(trig, f, m)
			return nil, nil
		})
	}()
}

// run executes one refresh attempt. It re-checks the interval inside the flight
// so a caller that arrived just after the previous flight finished is refused.
func (r *qidRefresher) run(trig string, f bundle.Fetcher, m *clientMetrics) {
	now := r.now()
	if !r.intervalOK(now) {
		return
	}
	r.lastAttemptNS.Store(now.UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.timeout)
	defer cancel()
	snap, err := r.build(ctx, f)
	if err != nil {
		result := qidResultFetchFailed
		if errors.Is(err, errQIDRejected) {
			result = qidResultRejected
		}
		m.recordQIDRefresh(trig, result)
		r.noteFailure(trig, result, err)
		return
	}
	activeQID.Store(snap)
	r.lastSuccessNS.Store(snap.at.UnixNano())
	m.recordQIDRefresh(trig, qidResultOK)
	m.recordQIDChanged(snap.changed)
	r.noteSuccess(trig, snap)
}

// noteSuccess clears the failing state and logs the recovery or first success.
func (r *qidRefresher) noteSuccess(trig string, snap *qidSnapshot) {
	r.mu.Lock()
	wasFailing := r.failing
	r.failing = false
	r.mu.Unlock()
	level := slog.LevelDebug
	if wasFailing || snap.changed > 0 {
		level = slog.LevelInfo
	}
	slog.Log(context.Background(), level, "queryid refresh applied",
		slog.String("trigger", trig), slog.Int("ops", len(snap.ids)),
		slog.Int("changed", snap.changed), slog.Bool("features_updated", snap.features != nil),
		slog.Bool("recovered", wasFailing))
}

// noteFailure keeps the last good snapshot untouched and reports the failure
// ONCE per ok->failed transition (log + alert hook), never per attempt.
func (r *qidRefresher) noteFailure(trig, result string, err error) {
	r.mu.Lock()
	transition := !r.failing
	r.failing = true
	hook := r.alertHook
	r.mu.Unlock()
	if !transition {
		return
	}
	slog.Warn("queryid refresh failed; keeping the last good ids",
		slog.String("trigger", trig), slog.String("result", result), slog.Any("error", err))
	if hook == nil {
		hook = func(topic string, payload any) {
			slog.Warn("pool alert", slog.String("topic", topic), slog.Any("payload", payload))
		}
	}
	hook("queryid.refresh_failed", map[string]any{"error": err.Error(), "trigger": trig, "result": result})
}

// build reads the bundle and returns a validated snapshot, or an error that
// leaves the active snapshot untouched.
func (r *qidRefresher) build(ctx context.Context, f bundle.Fetcher) (*qidSnapshot, error) {
	page, err := r.pageFor(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(page.scripts) == 0 {
		return nil, errors.New("queryid refresh: no x-web scripts on the warm page")
	}
	if bundle.IsLoggedOutShell(page.scripts) {
		return nil, bundle.ErrLoggedOutShell
	}

	ext := gqlextract.NewExtractor(slog.Default(), gqlextract.QueryIDShapeOnly())
	feat := gqlextract.NewFeatureCollector()
	walked := 0
	visit := func(u string, body []byte) {
		walked++
		ext.Consume(u, body)
		feat.Consume(u, body)
	}
	walk := &bundle.Map{}
	if err := walk.WalkImports(ctx, f, page.scripts, visit, bundle.Options{MaxBundles: qidMaxBundles}); err != nil {
		return nil, fmt.Errorf("queryid refresh: walk bundles: %w", err)
	}
	if walked == 0 {
		return nil, errors.New("queryid refresh: no bundle could be fetched")
	}

	ids, err := validateQIDs(knownOperations(), ext.Result(), ext.Invalid)
	if err != nil {
		return nil, err
	}
	snap := &qidSnapshot{ids: ids, at: r.now(), changed: countChangedIDs(ids)}

	extracted, _ := gqlextract.IntersectFeatures(feat.Result(), page.defaults, slog.Default())
	if merged, ferr := mergeFeatures(compiledGQLFeatures(), extracted); ferr != nil {
		slog.Info("queryid refresh: features unchanged", slog.Any("reason", ferr))
	} else {
		snap.features = merged
	}
	return snap, nil
}

// pageFor returns the authenticated warm page: the one xtid just fetched when
// it is fresh, else a fetch of our own.
func (r *qidRefresher) pageFor(ctx context.Context, f bundle.Fetcher) (*warmPage, error) {
	r.mu.Lock()
	p := r.page
	r.mu.Unlock()
	if p != nil && r.now().Sub(p.at) <= qidObservedPageMaxAge {
		return p, nil
	}
	body, err := bundle.FetchPage(ctx, f, xtid.AuthedWarmPageURL)
	if err != nil {
		return nil, fmt.Errorf("queryid refresh: fetch warm page: %w", err)
	}
	return parseWarmPage(body, r.now()), nil
}

// knownOperations is the set of operations this library issues.
func knownOperations() []string {
	ops := make([]string, 0, len(Endpoints))
	for name := range Endpoints {
		ops = append(ops, name)
	}
	return ops
}

// validateQIDs checks a bundle extraction as a whole and returns the ids to
// apply. Any failure rejects EVERYTHING: a tampered or partial bundle must not
// half-apply. Only known operations are ever applied.
func validateQIDs(known []string, extracted map[string]string, malformed func(op string) bool) (map[string]string, error) {
	found := make(map[string]string, len(known))
	for _, op := range known {
		if malformed(op) {
			return nil, fmt.Errorf("%w: malformed queryId for %s", errQIDRejected, op)
		}
		id, ok := extracted[op]
		if !ok {
			continue
		}
		if !gqlextract.QueryIDRe.MatchString(id) {
			return nil, fmt.Errorf("%w: queryId for %s fails the id alphabet", errQIDRejected, op)
		}
		found[op] = id
	}
	if float64(len(found)) < qidMinKnownFraction*float64(len(known)) {
		return nil, fmt.Errorf("%w: only %d of %d known operations found (min fraction %.2f)",
			errQIDRejected, len(found), len(known), qidMinKnownFraction)
	}
	return found, nil
}

// countChangedIDs counts applied ids that differ from the compiled set.
func countChangedIDs(ids map[string]string) int {
	n := 0
	for op, id := range ids {
		if ep, ok := Endpoints[op]; ok && ep.ID != id {
			n++
		}
	}
	return n
}

// mergeFeatures returns base plus the flags the bundle declares that base lacks
// (their authenticated-page default), or an error when there is nothing to
// apply: a partial feature extraction, or no new flag. Known flags keep their
// compiled values (the committed-values-win rule of cmd/gql-sync) and flags that
// vanished from the bundle are kept (dropping one X still requires is a 400).
func mergeFeatures(base, extracted map[string]any) (map[string]any, error) {
	if float64(len(extracted)) < qidMinFeatureFraction*float64(len(base)) {
		return nil, fmt.Errorf("%w: only %d feature flags extracted against %d compiled (min fraction %.2f)",
			errQIDRejected, len(extracted), len(base), qidMinFeatureFraction)
	}
	out := maps.Clone(base)
	added := 0
	for name, v := range extracted {
		if _, known := out[name]; !known {
			out[name] = v
			added++
		}
	}
	if added == 0 {
		return nil, errors.New("no new feature flags")
	}
	return out, nil
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
	snap := activeQID.Load()
	if snap == nil {
		return ep
	}
	if !isEnvPinned(op) {
		if id, ok := snap.ids[op]; ok {
			ep.ID = id
		}
	}
	if snap.features != nil && baselineFeatureOps[op] {
		ep.Features = maps.Clone(snap.features)
	}
	return ep
}
