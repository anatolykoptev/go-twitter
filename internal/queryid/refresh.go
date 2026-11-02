package queryid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/internal/gqlextract"
	"github.com/anatolykoptev/go-twitter/internal/operations"
	"github.com/anatolykoptev/go-twitter/internal/telemetry"
	"github.com/anatolykoptev/go-twitter/internal/xwebnative"
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
// generated (internal/operations/queryids_gen.go) > committed literal (internal/operations/endpoints.go). The first
// failure keeps the last good snapshot; nothing here ever clears it.

const (
	// RefreshTTL is how long a successful refresh is trusted. 6h matches the
	// bundle chunk-map cache TTL (bundle.defaultCacheTTL) and keeps the cost to
	// at most four bundle walks a day per process, while X rotates ids at the
	// scale of days to weeks; the fault trigger covers the faster case.
	RefreshTTL = 6 * time.Hour

	// FaultThreshold / FaultWindow: K endpoint faults on ONE operation
	// within the window trigger a refresh. A single 404 can be a one-off; three
	// within two minutes on the same op is the signature of a dead id (one call
	// on a 3-account pool already produces three, and a *WithSocial caller that
	// stops after two faults per call reaches it on its second call).
	FaultThreshold = 3
	FaultWindow    = 2 * time.Minute

	// MinInterval is the minimum spacing between two refresh ATTEMPTS, success
	// or failure, from any trigger. A refresh walks up to maxBundles chunks
	// through an account's client, so a 404 storm (X down, not an id rotation)
	// must not repeat that walk: at most 96 walks a day in the worst case, and a
	// failing bundle fetch backs off by the same interval.
	MinInterval = 15 * time.Minute

	// MaxBackoff caps the spacing between attempts after consecutive failed or
	// rejected refreshes: the spacing starts at MinInterval, doubles per
	// failure, and resets on success. A bundle that never yields (e.g. the live
	// shape differs) is retried at most once a day, fault trigger included.
	MaxBackoff = 24 * time.Hour

	// MinKnownFraction: a bundle that yields fewer than this share of the
	// operations this library knows is treated as partial (an unfinished chunk
	// walk, a bot wall) and rejected whole. The proven-extractable set today is 9
	// of the 19 known operations (47%, internal/operations/queryids_gen.go; the rest are session-only
	// or Relay-hash mutations), so 40% tolerates losing one and rejects half a
	// bundle.
	MinKnownFraction = 0.4

	// minFeatureFraction: the same guard for the feature set, against the
	// compiled baseline.
	minFeatureFraction = 0.75

	// refreshTimeout bounds one refresh (page + chunk walk).
	refreshTimeout = 3 * time.Minute

	// maxBundles bounds the chunk walk; same cap as cmd/gql-sync so the two
	// extract from the same coverage.
	maxBundles = 512

	// XWebGap is the pause each crawl worker takes between two chunk requests,
	// the same as cmd/xweb-report. The chunk map is NOT cached across refreshes:
	// each opt-in refresh re-walks the graph (the MemoFetcher only keeps bodies
	// for ObservedPageMaxAge), bounded by MinInterval and the failure backoff.
	XWebGap = 100 * time.Millisecond

	// ObservedPageMaxAge is how long the page and the chunks xtid fetched stay
	// reusable by the refresh: long enough to cover the background walk that
	// starts right after xtid's init, short enough that nothing stale is served.
	ObservedPageMaxAge = 2 * time.Minute
	// chunkCacheBytes caps the shared chunk cache (bodies are held only for
	// ObservedPageMaxAge).
	chunkCacheBytes = 32 << 20

	// envRefresh set to "off" disables the runtime refresh (operator kill
	// switch, next to the per-op TWITTER_QID_* overrides).
	envRefresh = "TWITTER_QID_REFRESH"

	// EnvXWeb set to "on" additionally reads the x-web (Vite + Relay) chunk
	// graph during a refresh, for the operations in XWebNativeOps. OFF by default:
	// the crawl is far larger than the legacy walk (the logged-out graph is ~2000
	// chunks) and the x-web ids are only valid for requests shaped for x-web.
	EnvXWeb = "TWITTER_QID_XWEB"
)

// XWebNativeOps are the operations go-twitter issues against the x-web stack,
// mapped to the Relay operation kind their artifact must declare (the shared
// list in internal/xwebnative, also read by cmd/xweb-report). Only these take an
// id from the x-web crawl; every other operation keeps its legacy id because an
// x-web persisted id is a different call (other variables, other response
// shape; see docs/xweb-migration.md).
var XWebNativeOps = xwebnative.Ops()

// Refresh triggers (twitter.trigger) and results (twitter.result). Bounded.
const (
	triggerFirst = "first" // no successful refresh yet
	triggerTTL   = "ttl"   // the active snapshot outlived RefreshTTL
	triggerFault = "fault" // K endpoint faults on one operation

	ResultOK          = "ok"
	ResultOKXWebOnly  = "ok_xweb_only" // applied from an x-web-only page: the legacy walk was clean and saw no legacy operation at all
	ResultFetchFailed = "fetch_failed" // page/bundle could not be read
	ResultRejected    = "rejected"     // read, but failed validation; nothing applied
	ResultPanic       = "panic"        // the walk panicked on a network-supplied body; recovered
	ResultSuppressed  = "suppressed"   // due, but the interval or failure backoff refused it
)

// ErrRejected marks an extraction that failed validation.
var ErrRejected = errors.New("queryid refresh: extraction rejected")

// ErrTooFew is the "partial bundle" flavour of ErrRejected: nothing was
// malformed, the walk just saw too small a share of the known operations. It is
// the one rejection an x-web-only bundle legitimately triggers.
var ErrTooFew = errors.New("too few known operations")

// Active is the process-wide refreshed snapshot endpointFor reads (nil = the
// compiled ids and features are active). It is global because the Endpoints
// registry it overlays is global: a refresh published by any refresher is seen
// by every client, lock-free.
var Active atomic.Pointer[Snapshot]

// Catalog is the root package's view of its endpoint table, which this package
// cannot import. The root passes it in Config; every func reads the live table
// on each call. New panics if any func is nil.
type Catalog struct {
	// Operations lists the operations the library issues.
	Operations func() []string
	// Known reports whether op has a queryId to refresh.
	Known func(op string) bool
	// CountChanged counts ids differing from the compiled set (env-pinned
	// operations excluded).
	CountChanged func(ids map[string]string) int
}

// Snapshot is one immutable refreshed state. Never mutated after Store.
type Snapshot struct {
	IDs      map[string]string // known operation -> validated queryId
	Features map[string]any    // refreshed baseline feature set; nil = no change
	at       time.Time
	Changed  int // ids differing from the compiled set
}

// Config holds the tunables; production uses the consts above, tests shrink
// them.
type Config struct {
	TTL, FaultWindow, MinInterval, Timeout time.Duration
	FaultThreshold                         int
	Disabled                               bool
	// xweb also reads the x-web chunk graph (see EnvXWeb).
	XWeb bool
	// Catalog is the root's endpoint table; required (see New).
	Catalog Catalog
}

// DefaultConfig is the production configuration; the env kill switch and the
// x-web opt-in are read here.
func DefaultConfig() Config {
	return Config{
		TTL:            RefreshTTL,
		FaultWindow:    FaultWindow,
		MinInterval:    MinInterval,
		Timeout:        refreshTimeout,
		FaultThreshold: FaultThreshold,
		Disabled:       os.Getenv(envRefresh) == "off",
		XWeb:           os.Getenv(EnvXWeb) == "on",
	}
}

// WarmPage is the part of the authenticated warm page the refresh needs.
type WarmPage struct {
	at        time.Time
	Scripts   []string // start bundles: x-web (Vite) scripts first, then webpack ones
	Vite      []string // the x-web (Vite) subset of scripts: the x-web crawl's start points
	Layout    string   // Layout*
	pageBytes int
	defaults  map[string]bool
}

// Warm-page layouts (log field "layout"). The logged-out page is Vite
// (x-web); the authenticated page can still be the legacy responsive-web
// webpack build, which xtid already handles (xtid.locateIndices).
const (
	LayoutVite    = "vite"
	LayoutWebpack = "webpack"
	LayoutBoth    = "vite+webpack"
	LayoutUnknown = "unknown"
)

// Refresher owns the active snapshot and the trigger bookkeeping. One
// instance lives for the whole process (processQID), so a refresh learned on
// one *WithSocial lease serves the next without another bundle fetch.
type Refresher struct {
	Cfg     Config
	Now     func() time.Time
	Started time.Time

	chunks *bundle.MemoCache // CDN chunks shared between xtid's scan and the refresh walk
	sf     singleflight.Group
	WG     sync.WaitGroup // in-flight refreshes; tests wait on it

	NextAllowedNS    atomic.Int64 // no attempt before this instant (min interval, then failure backoff)
	LastSuccessNS    atomic.Int64
	noDefaultsLogged atomic.Bool  // the "no featureSwitch defaults" line is logged once
	Attempts         atomic.Int64 // refresh runs that passed the interval gate (tests count these; chunk hits can be memoized)

	Mu        sync.Mutex
	Faults    map[string][]time.Time
	Failing   bool
	Streak    int // consecutive failed attempts; drives the backoff
	alertHook func(topic string, payload any)
	page      *WarmPage
}

// New returns a refresher with cfg. It panics when cfg.Catalog is incomplete:
// a missing func is a wiring bug, to be caught at construction, not on a request.
func New(cfg Config) *Refresher {
	if cfg.Catalog.Operations == nil || cfg.Catalog.Known == nil || cfg.Catalog.CountChanged == nil {
		panic("queryid: New requires a complete Catalog (Operations, Known and CountChanged)")
	}
	r := &Refresher{Cfg: cfg, Now: time.Now, Faults: make(map[string][]time.Time)}
	r.Started = r.Now()
	r.chunks = bundle.NewMemoCache(ObservedPageMaxAge, chunkCacheBytes, func() time.Time { return r.Now() })
	return r
}

// Shared wraps an authenticated warm fetcher so the chunks xtid's indices scan
// reads are served to the refresh's walk without a second request. nil stays
// nil (a guest request has no authenticated fetcher).
func (r *Refresher) Shared(f bundle.Fetcher) bundle.Fetcher {
	if r == nil || r.Cfg.Disabled || f == nil {
		return f
	}
	return &bundle.MemoFetcher{Inner: f, Cache: r.chunks}
}

// SetAlertHook is wired by NewClient from ClientConfig.PoolAlertHook so
// the refresh-failed alert reaches the consumer's pool hook, once per
// ok->failed transition (the xtid init-failed pattern).
func (r *Refresher) SetAlertHook(fn func(topic string, payload any)) {
	r.Mu.Lock()
	r.alertHook = fn
	r.Mu.Unlock()
}

// ObservePage stores the authenticated warm page xtid just fetched, so the next
// refresh reuses it instead of requesting it again.
func (r *Refresher) ObservePage(page []byte) {
	if r == nil || r.Cfg.Disabled {
		return
	}
	wp := ParseWarmPage(page, r.Now())
	r.Mu.Lock()
	r.page = wp
	r.Mu.Unlock()
}

// ParseWarmPage detects the page layout and collects the start bundles. When a
// page shows both layouts both are walked (Vite first), bounded together by
// maxBundles: the extractor keeps the highest-ranked source per operation
// (responsive-web > x-web), so walking both never loses an id, and costs nothing
// on the single-layout pages seen in practice.
func ParseWarmPage(page []byte, at time.Time) *WarmPage {
	body := string(page)
	vite := bundle.ViteScripts(body, xtid.AuthedWarmPageURL)
	web := bundle.WebpackScripts(body, xtid.AuthedWarmPageURL)
	layout := LayoutUnknown
	switch {
	case len(vite) > 0 && len(web) > 0:
		layout = LayoutBoth
	case len(vite) > 0:
		layout = LayoutVite
	case len(web) > 0:
		layout = LayoutWebpack
	}
	return &WarmPage{
		at:        at,
		Scripts:   append(slices.Clone(vite), web...),
		Vite:      vite,
		Layout:    layout,
		pageBytes: len(page),
		defaults:  gqlextract.ParseFeatureDefaults(page),
	}
}

// OnAuthedRequest runs on every request that carries an account's cookies (the
// authenticated warm-fetch condition): it samples the map age and fires the
// first/TTL trigger when due. f is the same authenticated fetcher xtid uses.
func (r *Refresher) OnAuthedRequest(f bundle.Fetcher, m *telemetry.Metrics) {
	if r == nil || r.Cfg.Disabled || f == nil {
		return
	}
	now := r.Now()
	m.RecordQIDAge(r.Age(now))
	trig, due := r.lazyDue(now)
	if !due {
		return
	}
	if !r.IntervalOK(now) {
		m.RecordQIDRefresh(trig, ResultSuppressed)
		return
	}
	r.fire(trig, f, m)
}

// OnEndpointFault counts an endpoint fault (errEndpoint, #61) against op and
// fires the fault trigger at K within the window. mk builds the fetcher lazily;
// a request without an account cookie has none and cannot refresh. A threshold
// hit that the interval or backoff refuses keeps the op's fault history, so the
// trigger fires as soon as it is allowed instead of needing K fresh faults.
func (r *Refresher) OnEndpointFault(op string, mk func() bundle.Fetcher, m *telemetry.Metrics) {
	if r == nil || r.Cfg.Disabled {
		return
	}
	if !r.Cfg.Catalog.Known(op) {
		return // REST names (DMInbox, ...) have no queryId to refresh
	}
	if !r.noteFault(op) {
		return
	}
	if !r.IntervalOK(r.Now()) {
		m.RecordQIDRefresh(triggerFault, ResultSuppressed)
		return
	}
	f := mk()
	if f == nil {
		return
	}
	r.clearFaults(op)
	r.fire(triggerFault, f, m)
}

// noteFault records a fault and reports whether op has reached the threshold
// within the window.
func (r *Refresher) noteFault(op string) bool {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	now := r.Now()
	cut := now.Add(-r.Cfg.FaultWindow)
	recent := r.Faults[op][:0]
	for _, t := range r.Faults[op] {
		if t.After(cut) {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	r.Faults[op] = recent
	return len(recent) >= r.Cfg.FaultThreshold
}

// clearFaults forgets op's fault history once a refresh was actually started.
func (r *Refresher) clearFaults(op string) {
	r.Mu.Lock()
	delete(r.Faults, op)
	r.Mu.Unlock()
}

// lazyDue reports whether the first/TTL trigger is due (interval aside).
func (r *Refresher) lazyDue(now time.Time) (string, bool) {
	ls := r.LastSuccessNS.Load()
	switch {
	case ls == 0:
		return triggerFirst, true
	case now.Sub(time.Unix(0, ls)) > r.Cfg.TTL:
		return triggerTTL, true
	}
	return "", false
}

// IntervalOK is the spacing gate: no attempt before NextAllowedNS, which is the
// minimum interval after an attempt and grows by doubling (up to MaxBackoff)
// over consecutive failures. Every trigger passes it before and inside the
// singleflight.
func (r *Refresher) IntervalOK(now time.Time) bool {
	return now.UnixNano() >= r.NextAllowedNS.Load()
}

// Backoff is the spacing after the streak-th consecutive failure: minInterval,
// doubling per failure, capped at MaxBackoff.
func (r *Refresher) Backoff(streak int) time.Duration {
	wait := r.Cfg.MinInterval
	for i := 1; i < streak && wait < MaxBackoff; i++ {
		wait *= 2
	}
	return min(wait, MaxBackoff)
}

// Age is the age of the active map in seconds: since the last successful
// refresh, or since process start while the compiled ids are still active.
func (r *Refresher) Age(now time.Time) float64 {
	since := r.Started
	if s := Active.Load(); s != nil {
		since = s.at
	}
	return now.Sub(since).Seconds()
}

// fire starts a refresh in the background if the interval allows it. The
// request that hit the fault (or tripped the TTL) never waits for it.
func (r *Refresher) fire(trig string, f bundle.Fetcher, m *telemetry.Metrics) {
	if !r.IntervalOK(r.Now()) {
		return
	}
	r.WG.Add(1)
	go func() {
		defer r.WG.Done()
		_, _, _ = r.sf.Do("refresh", func() (any, error) {
			r.Run(trig, f, m)
			return nil, nil
		})
	}()
}

// Run executes one refresh attempt. It re-checks the interval inside the flight
// so a caller that arrived just after the previous flight finished is refused.
func (r *Refresher) Run(trig string, f bundle.Fetcher, m *telemetry.Metrics) {
	now := r.Now()
	if !r.IntervalOK(now) {
		m.RecordQIDRefresh(trig, ResultSuppressed)
		return
	}
	r.NextAllowedNS.Store(now.Add(r.Cfg.MinInterval).UnixNano())
	r.Attempts.Add(1)

	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), r.Cfg.Timeout)
	defer cancel()
	var seen PageSeen
	snap, result, err := r.Attempt(ctx, f, &seen)
	took := time.Since(began)
	m.RecordQIDRefreshDuration(trig, result, took)
	slog.Info("queryid refresh finished",
		slog.String("trigger", trig), slog.String("result", result), slog.Duration("took", took),
		slog.String("layout", seen.Layout), slog.Int("start_scripts", seen.Scripts))
	if err != nil {
		r.Mu.Lock()
		r.Streak++
		wait := r.Backoff(r.Streak)
		r.Mu.Unlock()
		r.NextAllowedNS.Store(now.Add(wait).UnixNano())
		m.RecordQIDRefresh(trig, result)
		r.noteFailure(trig, result, err)
		return
	}
	prev := Active.Load()
	carried := overlaySnapshot(prev, snap, r.Cfg.Catalog)
	Active.Store(snap)
	r.LastSuccessNS.Store(snap.at.UnixNano())
	r.Mu.Lock()
	r.Streak = 0
	r.Mu.Unlock()
	m.RecordQIDRefresh(trig, result)
	m.RecordQIDChanged(snap.Changed)
	r.noteSuccess(trig, snap, carried)
}

// Attempt builds a snapshot and classifies the outcome. A panic while parsing
// network-supplied bodies is recovered into a failure: a refresh must never
// take down the embedding service.
func (r *Refresher) Attempt(ctx context.Context, f bundle.Fetcher, seen *PageSeen) (snap *Snapshot, result string, err error) {
	defer func() {
		if p := recover(); p != nil {
			snap, result, err = nil, ResultPanic, fmt.Errorf("queryid refresh: panic: %v", p)
		}
	}()
	snap, err = r.Build(ctx, f, seen)
	switch {
	case err == nil && seen.xwebOnly:
		return snap, ResultOKXWebOnly, nil
	case err == nil:
		return snap, ResultOK, nil
	case errors.Is(err, ErrRejected):
		return nil, ResultRejected, err
	}
	return nil, ResultFetchFailed, err
}

// overlaySnapshot lays next over prev: ids (and features) the earlier refreshes
// healed but this walk did not see are carried forward, so one flaky chunk
// cannot send a healed operation back to a retired compiled id. It returns how
// many ids were carried and recomputes next.Changed over the merged set.
func overlaySnapshot(prev, next *Snapshot, cat Catalog) int {
	if prev == nil {
		return 0
	}
	carried := 0
	for op, id := range prev.IDs {
		if _, seen := next.IDs[op]; !seen {
			next.IDs[op] = id
			carried++
		}
	}
	switch next.Features {
	case nil:
		next.Features = prev.Features
	default:
		for name, v := range prev.Features {
			if _, seen := next.Features[name]; !seen {
				next.Features[name] = v
			}
		}
	}
	next.Changed = cat.CountChanged(next.IDs)
	return carried
}

// noteSuccess clears the failing state and logs the recovery or first success.
func (r *Refresher) noteSuccess(trig string, snap *Snapshot, carried int) {
	r.Mu.Lock()
	wasFailing := r.Failing
	r.Failing = false
	r.Mu.Unlock()
	level := slog.LevelDebug
	if wasFailing || snap.Changed > 0 || carried > 0 {
		level = slog.LevelInfo
	}
	slog.Log(context.Background(), level, "queryid refresh applied",
		slog.String("trigger", trig), slog.Int("ops", len(snap.IDs)),
		slog.Int("changed", snap.Changed), slog.Int("carried_forward", carried), slog.Bool("features_updated", snap.Features != nil),
		slog.Bool("recovered", wasFailing))
}

// noteFailure keeps the last good snapshot untouched and reports the failure
// ONCE per ok->failed transition (log + alert hook), never per attempt.
func (r *Refresher) noteFailure(trig, result string, err error) {
	r.Mu.Lock()
	transition := !r.Failing
	r.Failing = true
	hook := r.alertHook
	r.Mu.Unlock()
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

// Consume feeds one fetched body to the extractor. A var only so a test can
// make it panic.
var Consume = func(ext *gqlextract.Extractor, u string, body []byte) { ext.Consume(u, body) }

// ConsumeXWeb feeds one fetched chunk to the x-web extractor. A var only so a
// test can make it panic.
var ConsumeXWeb = func(ctx context.Context, ext *gqlextract.XWebExtractor, u string, body []byte) {
	ext.ConsumeContext(ctx, u, body)
}

// PageSeen is what a refresh learned about the warm page, for the finish log
// (never page contents).
type PageSeen struct {
	Layout  string
	Scripts int
	// xwebOnly: the refresh was applied from x-web ids alone (see Build).
	xwebOnly bool
}

// failureCounter counts the fetch errors of a walk, so a partial walk can be
// told apart from a clean one that simply found nothing.
type failureCounter struct {
	bundle.Fetcher
	failures atomic.Int64
}

func (c *failureCounter) Fetch(ctx context.Context, u string) ([]byte, error) {
	b, err := c.Fetcher.Fetch(ctx, u)
	if err != nil {
		c.failures.Add(1)
	}
	return b, err
}

// Build reads the bundle and returns a validated snapshot, or an error that
// leaves the active snapshot untouched. seen is filled as soon as the page is
// parsed so a failed refresh still reports the layout.
func (r *Refresher) Build(ctx context.Context, f bundle.Fetcher, seen *PageSeen) (*Snapshot, error) {
	page, err := r.pageFor(ctx, f)
	if err != nil {
		return nil, err
	}
	seen.Layout, seen.Scripts = page.Layout, len(page.Scripts)
	if len(page.Scripts) == 0 {
		return nil, fmt.Errorf("queryid refresh: no x-web or responsive-web scripts on the warm page (layout=%s, page_bytes=%d)",
			page.Layout, page.pageBytes)
	}
	if bundle.IsLoggedOutShell(page.Scripts) {
		return nil, bundle.ErrLoggedOutShell
	}

	ext := gqlextract.NewExtractor(slog.Default(), gqlextract.QueryIDShapeOnly())
	feat := gqlextract.NewFeatureCollector()
	walked := 0
	visit := func(u string, body []byte) {
		walked++
		Consume(ext, u, body)
		feat.Consume(u, body)
	}
	walk := &bundle.Map{}
	counted := &failureCounter{Fetcher: f}
	if err := walk.WalkImports(ctx, counted, page.Scripts, visit, bundle.Options{MaxBundles: maxBundles}); err != nil {
		return nil, fmt.Errorf("queryid refresh: walk bundles: %w", err)
	}
	if walked == 0 {
		return nil, errors.New("queryid refresh: no bundle could be fetched")
	}

	xids := map[string]string{}
	if r.Cfg.XWeb {
		xids = r.WalkXWeb(ctx, f, page)
	}
	ids, err := ValidateIDs(r.Cfg.Catalog.Operations(), ext.Result(), ext.Invalid)
	if err != nil {
		// The x-web ids may stand alone ONLY on positive evidence of an x-web-only
		// page: a Vite-only layout whose legacy chunks were all fetched and yet
		// declared no legacy operation. "Too few operations" alone is also what a
		// flaky or partial legacy walk looks like, and that must stay a failure
		// (streak, backoff, alert) even when an x-web id happens to be in hand.
		xwebOnly := len(ext.Result()) == 0 && counted.failures.Load() == 0 && walked > 0 && page.Layout == LayoutVite
		if len(xids) == 0 || !errors.Is(err, ErrTooFew) || !xwebOnly {
			return nil, err
		}
		seen.xwebOnly = true
		slog.Info("queryid refresh: x-web-only page; applying x-web ids alone",
			slog.Int("legacy_chunks", walked), slog.Int("xweb_ids", len(xids)))
		ids = map[string]string{}
	}
	maps.Copy(ids, xids) // x-web wins for its native ops: the legacy shape never carries them
	snap := &Snapshot{IDs: ids, at: r.Now(), Changed: r.Cfg.Catalog.CountChanged(ids)}

	if len(page.defaults) == 0 && r.noDefaultsLogged.CompareAndSwap(false, true) {
		slog.Info("queryid refresh: no featureSwitch defaults on the warm page; features stay compiled",
			slog.String("layout", page.Layout))
	}
	extracted, _ := gqlextract.IntersectFeatures(feat.Result(), page.defaults, slog.Default())
	if merged, ferr := mergeFeatures(operations.CompiledGQLFeatures(), extracted); ferr != nil {
		slog.Info("queryid refresh: features unchanged", slog.Any("reason", ferr))
	} else {
		snap.Features = merged
	}
	return snap, nil
}

// WalkXWeb crawls the x-web chunk graph from the page's Vite scripts and returns
// the ids of the XWebNativeOps it found, stopping as soon as all are in hand. It
// never fails the refresh: a crawl that finds nothing (or panics on a hostile
// body) yields no ids and the legacy result stands.
func (r *Refresher) WalkXWeb(ctx context.Context, f bundle.Fetcher, page *WarmPage) (out map[string]string) {
	out = map[string]string{}
	if len(page.Vite) == 0 {
		return out
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Warn("queryid refresh: x-web crawl panicked; legacy result stands", slog.Any("panic", p))
			out = map[string]string{}
		}
	}()
	ext := gqlextract.NewXWebExtractor(slog.Default())
	done := func() bool {
		got := ext.Result()
		for op := range XWebNativeOps {
			if _, ok := got[op]; !ok {
				return false
			}
		}
		return true
	}
	stats, err := bundle.CrawlXWeb(ctx, f, page.Vite, func(u string, body []byte) { ConsumeXWeb(ctx, ext, u, body) },
		bundle.CrawlOptions{Done: done, Gap: XWebGap})
	byName := map[string]gqlextract.Op{}
	for _, op := range ext.Ops() {
		byName[op.Name] = op
	}
	conflicts := ext.Conflicts()
	for op, kind := range XWebNativeOps {
		x, ok := byName[op]
		switch {
		case !ok || x.Kind != kind:
		case len(conflicts[op]) > 0:
			// Two chunks declared different ids for one operation: neither is
			// trusted, and the committed/legacy id stays in force.
			slog.Warn("queryid refresh: x-web id conflict; operation left unchanged",
				slog.String("op", op), slog.Int("ids", 1+len(conflicts[op])))
		default:
			out[op] = x.ID
		}
	}
	slog.Info("queryid refresh: x-web crawl",
		slog.Int("chunks", stats.Fetched), slog.Int("failed", stats.Failed), slog.Bool("capped", stats.Capped),
		slog.Int("truncated_chunks", ext.Truncated()), slog.Int("native_ops_found", len(out)), slog.Int("native_ops_wanted", len(XWebNativeOps)), slog.Any("error", err))
	return out
}

// pageFor returns the authenticated warm page: the one xtid just fetched when
// it is fresh, else a fetch of our own.
func (r *Refresher) pageFor(ctx context.Context, f bundle.Fetcher) (*WarmPage, error) {
	r.Mu.Lock()
	p := r.page
	r.Mu.Unlock()
	if p != nil && r.Now().Sub(p.at) <= ObservedPageMaxAge {
		return p, nil
	}
	body, err := bundle.FetchPage(ctx, f, xtid.AuthedWarmPageURL)
	if err != nil {
		return nil, fmt.Errorf("queryid refresh: fetch warm page: %w", err)
	}
	return ParseWarmPage(body, r.Now()), nil
}

// ValidateIDs checks a bundle extraction as a whole and returns the ids to
// apply. Any failure rejects EVERYTHING: a tampered or partial bundle must not
// half-apply. Only known operations are ever applied.
func ValidateIDs(known []string, extracted map[string]string, malformed func(op string) bool) (map[string]string, error) {
	found := make(map[string]string, len(known))
	for _, op := range known {
		if malformed(op) {
			return nil, fmt.Errorf("%w: malformed queryId for %s", ErrRejected, op)
		}
		id, ok := extracted[op]
		if !ok {
			continue
		}
		if !gqlextract.QueryIDRe.MatchString(id) {
			return nil, fmt.Errorf("%w: queryId for %s fails the id alphabet", ErrRejected, op)
		}
		found[op] = id
	}
	if float64(len(found)) < MinKnownFraction*float64(len(known)) {
		return nil, fmt.Errorf("%w: %w: only %d of %d known operations found (min fraction %.2f)",
			ErrRejected, ErrTooFew, len(found), len(known), MinKnownFraction)
	}
	return found, nil
}

// mergeFeatures returns base plus the flags the bundle declares that base lacks
// (their authenticated-page default), or an error when there is nothing to
// apply: a partial feature extraction, or no new flag. Known flags keep their
// compiled values (the committed-values-win rule of cmd/gql-sync) and flags that
// vanished from the bundle are kept (dropping one X still requires is a 400).
func mergeFeatures(base, extracted map[string]any) (map[string]any, error) {
	if float64(len(extracted)) < minFeatureFraction*float64(len(base)) {
		return nil, fmt.Errorf("%w: only %d feature flags extracted against %d compiled (min fraction %.2f)",
			ErrRejected, len(extracted), len(base), minFeatureFraction)
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
