package twitter

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anatolykoptev/go-twitter/internal/gqlextract"
)

// Runtime queryId refresh journeys (#68). Each drives the real request path
// (doRequestWithBody -> xtid -> refresh trigger; doPoolRequest -> fault
// trigger) against a fake X: an authenticated warm page, a Vite chunk carrying
// the operation ids, and a GraphQL endpoint that answers by id — the id X
// currently serves gets 200, any other gets 404.

const (
	qidEntryURL  = testViteBase + "entry-client-0000111122223333.js"
	qidSignURL   = testViteBase + "sign.o-0123456789abcdef.js"
	qidWarmPage  = "https://x.com/tesla"
	qidNewSearch = "NewSearchID_0001"
	qidNewUser   = "NewUserID_0002"
	qidNewFlag   = "brand_new_flag_enabled"
)

// qidClock is the refresher's fake clock.
type qidClock struct{ ns atomic.Int64 }

func newQIDClock() *qidClock {
	c := &qidClock{}
	c.ns.Store(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *qidClock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *qidClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// useQIDRefresher installs an ENABLED process-wide refresher with the
// production constants and a fake clock, and a clean active snapshot; both are
// restored afterwards.
func useQIDRefresher(t *testing.T) (*qidRefresher, *qidClock) {
	t.Helper()
	clk := newQIDClock()
	r := newQIDRefresher(qidConfig{
		ttl: qidRefreshTTL, faultWindow: qidFaultWindow, minInterval: qidMinInterval,
		timeout: 30 * time.Second, faultThreshold: qidFaultThreshold,
	})
	r.now = clk.Now
	r.started = clk.Now()
	prev, prevSnap := processQIDPtr.Load(), activeQID.Load()
	processQIDPtr.Store(r)
	activeQID.Store(nil)
	t.Cleanup(func() {
		r.wg.Wait()
		processQIDPtr.Store(prev)
		activeQID.Store(prevSnap)
	})
	return r, clk
}

// qidWorld is the fake X.
type qidWorld struct {
	mu         sync.Mutex
	live       map[string]string // op -> the id GraphQL accepts
	advertised map[string]string // op -> the id the bundle declares
	extraJS    string            // appended verbatim to the ops chunk
	failFetch  bool              // page and chunks answer 500
	newFlag    bool              // the bundle and page declare qidNewFlag
	dropFlags  bool              // the bundle declares almost no feature flags
	hits       map[string]int    // URL -> fetch count (page + chunks)
	gqlIDs     []string          // id path segment of every GraphQL request, in order
	srv        *httptest.Server
}

func newQIDWorld(t *testing.T) *qidWorld {
	t.Helper()
	w := &qidWorld{live: map[string]string{}, advertised: map[string]string{}, hits: map[string]int{}}
	for op, ep := range Endpoints {
		w.live[op], w.advertised[op] = ep.ID, ep.ID
	}
	return w
}

// start serves the GraphQL side on its own httptest server and points the
// package at it.
func (w *qidWorld) start(t *testing.T) {
	t.Helper()
	w.srv = httptest.NewServer(http.HandlerFunc(w.serveGraphQL))
	t.Cleanup(w.srv.Close)
	orig := twitterBase
	twitterBase = w.srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = orig })
}

// retire makes X serve (and the bundle declare) a new id for op.
func (w *qidWorld) retire(op, id string) {
	w.mu.Lock()
	w.live[op], w.advertised[op] = id, id
	w.mu.Unlock()
}

func (w *qidWorld) set(f func(w *qidWorld)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w)
}

// bundleFetches is how many times the ops chunk was fetched from X.
func (w *qidWorld) bundleFetches() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hits[qidEntryURL]
}

func (w *qidWorld) pageFetches() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hits[qidWarmPage]
}

func (w *qidWorld) requestedIDs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.gqlIDs...)
}

func (w *qidWorld) serveGraphQL(rw http.ResponseWriter, req *http.Request) {
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/i/api/graphql/"), "/")
	id, op := parts[0], ""
	if len(parts) > 1 {
		op = parts[1]
	}
	w.mu.Lock()
	w.gqlIDs = append(w.gqlIDs, id)
	live := w.live[op]
	w.mu.Unlock()
	if id != live {
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"errors":[{"message":"Not Found"}]}`))
		return
	}
	if op == "UserByScreenName" {
		_, _ = rw.Write([]byte(socialUserBody))
		return
	}
	_, _ = rw.Write([]byte(`{"data":{"ok":true}}`))
}

// DoCtx implements warmDoer: the authenticated page and the Vite chunks.
func (w *qidWorld) DoCtx(_ context.Context, _, u string, _ map[string]string, _ io.Reader) ([]byte, map[string]string, int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hits[u]++
	if w.failFetch {
		return nil, nil, http.StatusInternalServerError, nil
	}
	switch u {
	case qidWarmPage:
		return w.pageHTML(), nil, http.StatusOK, nil
	case qidEntryURL:
		return w.entryJS(), nil, http.StatusOK, nil
	case qidSignURL:
		return []byte(testIndicesJS), nil, http.StatusOK, nil
	}
	return nil, nil, http.StatusNotFound, nil
}

func (w *qidWorld) flagNames() []string {
	names := []string{}
	for name := range compiledGQLFeatures() {
		names = append(names, name)
	}
	if w.newFlag {
		names = append(names, qidNewFlag)
	}
	sort.Strings(names)
	if w.dropFlags {
		names = names[:3]
	}
	return names
}

func (w *qidWorld) pageHTML() []byte {
	key := base64.StdEncoding.EncodeToString([]byte{0, 20, 30, 40, 50, 0, 70, 80})
	defaults := []string{}
	for _, name := range w.flagNames() {
		defaults = append(defaults, fmt.Sprintf(`%q:{"value":true}`, name))
	}
	return []byte(`<html><head><meta name="twitter-site-verification" content="` + key + `">` +
		`<script type="module" src="` + qidEntryURL + `"></script></head><body>` + testSVG +
		`<script>window.__INITIAL_STATE__={"featureSwitch":{"defaultConfig":{` + strings.Join(defaults, ",") + `}}};</script>` +
		`</body></html>`)
}

func (w *qidWorld) entryJS() []byte {
	var b strings.Builder
	b.WriteString(`const s=()=>import("./sign.o-0123456789abcdef.js");export const ops=[`)
	ops := make([]string, 0, len(w.advertised))
	for op := range w.advertised {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		fmt.Fprintf(&b, `{queryId:%q,operationName:%q,operationType:"query",metadata:{}},`, w.advertised[op], op)
	}
	b.WriteString("];")
	names := make([]string, 0)
	for _, name := range w.flagNames() {
		names = append(names, fmt.Sprintf("%q", name))
	}
	fmt.Fprintf(&b, "export const f=[%s],fieldToggles:[];", strings.Join(names, ","))
	b.WriteString(w.extraJS)
	return []byte(b.String())
}

// newQIDClient builds a pool client over the fake world: the xtid warm path
// and the queryId refresh both read through the world, like production reads
// through the account's client.
func newQIDClient(t *testing.T, w *qidWorld, accounts int) (*Client, []*Account) {
	t.Helper()
	w.start(t)
	c, accs := newPoolWaitClient(t, accounts)
	c.warmDoerOverride = w
	c.xtidMgr.SetWarmPageHook(observeWarmPage)
	return c, accs
}

func (c *Client) search(ctx context.Context) error {
	_, err := c.searchTimelineRaw(ctx, "golang", SearchLatest, 10)
	return err
}

func ctxTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// F1 — journey: X retires the SearchTimeline id. Calls get 404; after K faults a
// refresh happens, and the next call uses the new id and succeeds.
//
// Mutation: in request.go doPoolRequest delete the `c.noteEndpointFault(...)`
// call in the endpoint-fault arm => RED.
func TestQIDRefresh_F1_RetiredIDRecoversAfterKFaults(t *testing.T) {
	r, clk := useQIDRefresher(t)
	w := newQIDWorld(t)
	c, _ := newQIDClient(t, w, 1)
	ctx := ctxTimeout(t)
	compiledID := Endpoints["SearchTimeline"].ID

	// A healthy call: the first authenticated request refreshes once, with the
	// bundle still declaring the compiled ids.
	require.NoError(t, c.search(ctx))
	r.wg.Wait()
	require.Equal(t, 1, w.bundleFetches(), "the first authenticated request refreshes once")
	require.NotNil(t, activeQID.Load())

	// X retires the id; well past the minimum interval, still inside the TTL.
	w.retire("SearchTimeline", qidNewSearch)
	clk.Advance(qidMinInterval + time.Minute)

	for i := 0; i < qidFaultThreshold; i++ {
		err := c.search(ctx)
		require.ErrorIs(t, err, ErrEndpoint, "call %d must fault on the retired id", i)
		clk.Advance(5 * time.Second) // inside the fault window
	}
	r.wg.Wait()
	require.Equal(t, 2, w.bundleFetches(), "K faults on one op must trigger exactly one refresh")

	require.NoError(t, c.search(ctx), "the call after the refresh must use the new id")
	ids := w.requestedIDs()
	assert.Equal(t, qidNewSearch, ids[len(ids)-1])
	assert.Equal(t, compiledID, ids[len(ids)-2], "the K-th faulting call still used the compiled id: no in-call retry")
}

// F8 — the account-pinned write path counts endpoint faults as well: K faults
// on a retired CreateTweet id refresh the ids, and the next write uses the new
// one. A write is never replayed within the faulting call.
//
// Mutation: in request.go doPOST delete the `c.noteEndpointFault(...)` call in
// the `status != 200` arm => RED.
func TestQIDRefresh_F8_WritePathFaultsTriggerRefresh(t *testing.T) {
	r, clk := useQIDRefresher(t)
	w := newQIDWorld(t)
	c, accs := newQIDClient(t, w, 1)
	ctx := ctxTimeout(t)
	post := func() error {
		_, err := c.postMutation(ctx, accs[0], "CreateTweet", map[string]any{"tweet_text": "hello"})
		return err
	}

	require.NoError(t, post())
	r.wg.Wait()
	require.EqualValues(t, 1, r.attempts.Load())

	const newCreate = "NewCreateID_0003"
	w.retire("CreateTweet", newCreate)
	clk.Advance(qidMinInterval + time.Minute)
	for i := 0; i < qidFaultThreshold; i++ {
		require.ErrorIs(t, post(), ErrEndpoint, "write %d must fault on the retired id", i)
		clk.Advance(5 * time.Second)
	}
	r.wg.Wait()
	require.EqualValues(t, 2, r.attempts.Load(), "K write faults must trigger one refresh")

	require.NoError(t, post(), "the next write uses the refreshed id")
	ids := w.requestedIDs()
	assert.Equal(t, newCreate, ids[len(ids)-1])
}

// F2 — journey on the *WithSocial path with a fake go-social and a fake X. A
// refresh during lease 1 serves lease 2: the fake X sees exactly one bundle
// fetch.
//
// Mutation: in queryid_refresh.go qidRefresh() replace `return processQID()` with
//
//	if c.qid == nil { c.qid = newQIDRefresher(processQID().cfg) }; return c.qid
//
// (per-Client state) => RED.
func TestQIDRefresh_F2_SocialLeaseTwoServedByLeaseOneRefresh(t *testing.T) {
	r, clk := useQIDRefresher(t)
	w := newQIDWorld(t)
	w.retire("UserByScreenName", qidNewUser)
	rig := newSocialRigX(t, w.serveGraphQL)

	var clientsMu sync.Mutex
	var clients []*Client
	prevHook := testHookSocialClient
	testHookSocialClient = func(c *Client) {
		prevHook(c)
		c.warmDoerOverride = w
		clientsMu.Lock()
		clients = append(clients, c)
		clientsMu.Unlock()
	}
	waitAll := func() {
		r.wg.Wait()
		clientsMu.Lock()
		defer clientsMu.Unlock()
		for _, c := range clients {
			c.qidRefresh().wg.Wait()
		}
	}

	// Lease 1 hits the retired id: a fault for the caller, a refresh for the process.
	_, err := tryWithAccount(context.Background(), rig.sc, "UserByScreenName", func(ctx context.Context, tw *Client) (*TwitterUser, error) {
		return tw.GetUserByScreenName(ctx, "tesla")
	})
	require.ErrorIs(t, err, ErrEndpoint)
	waitAll()

	// Past the shared chunk cache and the observed-page lifetime, inside the TTL:
	// a second refresh by lease 2 could no longer hide behind either and would
	// have to hit X again.
	clk.Advance(qidObservedPageMaxAge + time.Minute)

	// Lease 2 is a brand-new Client on another account: it must use the refreshed id.
	user, err := GetUserWithSocial(context.Background(), rig.sc, "tesla")
	require.NoError(t, err)
	require.Equal(t, "999", user.ID)
	waitAll()

	assert.Equal(t, 1, w.bundleFetches(), "one refresh serves both leases: the bundle is fetched once")
	assert.Equal(t, 1, w.pageFetches(), "xtid's warm-page fetch is shared with the refresh and not repeated by lease 2")
	assert.EqualValues(t, 1, r.attempts.Load(), "lease 2 must not start a refresh of its own")
	ids := w.requestedIDs()
	require.Len(t, ids, 2)
	assert.Equal(t, qidNewUser, ids[1])
}

// F3a — validation: a bundle with too few known operations changes nothing.
//
// Mutation: in queryid_refresh.go validateQIDs delete the
// `if float64(len(found)) < qidMinKnownFraction*float64(len(known))` block => RED.
func TestQIDRefresh_F3a_TooFewOperationsChangesNothing(t *testing.T) {
	r, _ := useQIDRefresher(t)
	w := newQIDWorld(t)
	c, _ := newQIDClient(t, w, 1)

	w.set(func(w *qidWorld) {
		w.live["SearchTimeline"] = qidNewSearch
		w.advertised = map[string]string{"SearchTimeline": qidNewSearch, "UserByScreenName": Endpoints["UserByScreenName"].ID}
	})
	_ = c.search(ctxTimeout(t)) // first authenticated request: the refresh runs on a partial bundle
	r.wg.Wait()

	require.GreaterOrEqual(t, w.bundleFetches(), 1, "the refresh must have read the bundle")
	assert.Nil(t, activeQID.Load(), "a partial bundle must not half-apply")
	assert.Equal(t, Endpoints["SearchTimeline"].ID, endpointFor("SearchTimeline").ID)
}

// F3b — validation: one malformed id for a known operation rejects the whole
// extraction (a tampered bundle is not trusted for its other ids).
//
// Mutation: in queryid_refresh.go validateQIDs delete the `if malformed(op)`
// block => RED.
func TestQIDRefresh_F3b_MalformedIDRejectsWholeBundle(t *testing.T) {
	r, _ := useQIDRefresher(t)
	w := newQIDWorld(t)
	c, _ := newQIDClient(t, w, 1)

	w.retire("SearchTimeline", qidNewSearch)
	w.set(func(w *qidWorld) { w.extraJS = `{queryId:"BAD/ID",operationName:"UserTweets"},` })
	_ = c.search(ctxTimeout(t))
	r.wg.Wait()

	require.GreaterOrEqual(t, w.bundleFetches(), 1)
	assert.Nil(t, activeQID.Load(), "a bundle carrying a malformed id must change nothing, not even the well-formed ops")
}

// F3c — validation: the id alphabet is enforced by the validator itself, not
// only by the extractor in front of it.
//
// Mutation: in queryid_refresh.go validateQIDs delete the
// `if !gqlextract.QueryIDRe.MatchString(id)` block => RED.
func TestQIDRefresh_F3c_ValidatorEnforcesIDAlphabet(t *testing.T) {
	known := knownOperations()
	extracted := map[string]string{}
	for _, op := range known {
		extracted[op] = Endpoints[op].ID
	}
	require.True(t, gqlextract.QueryIDRe.MatchString(extracted["SearchTimeline"]))
	extracted["SearchTimeline"] = "../../etc/passwd"

	_, err := validateQIDs(known, extracted, func(string) bool { return false })
	require.ErrorIs(t, err, errQIDRejected)

	extracted["SearchTimeline"] = qidNewSearch
	got, err := validateQIDs(known, extracted, func(string) bool { return false })
	require.NoError(t, err)
	assert.Equal(t, qidNewSearch, got["SearchTimeline"])
}

// F4 — a per-op env override beats the refreshed id.
//
// Mutation: in queryid_refresh.go endpointFor delete the `!isEnvPinned(op)`
// condition around the refreshed id => RED.
func TestQIDRefresh_F4_EnvOverrideBeatsRefreshedID(t *testing.T) {
	r, _ := useQIDRefresher(t)
	w := newQIDWorld(t)

	const pinned = "PinnedByOperator_01"
	origEp, origPins := Endpoints["SearchTimeline"], envPinned.Load()
	t.Cleanup(func() { Endpoints["SearchTimeline"] = origEp; envPinned.Store(origPins) })
	t.Setenv("TWITTER_QID_SEARCH_TIMELINE", pinned)
	ApplyEnvOverrides()

	w.set(func(w *qidWorld) { w.live["SearchTimeline"] = pinned })
	w.set(func(w *qidWorld) { w.advertised["SearchTimeline"] = qidNewSearch })
	c, _ := newQIDClient(t, w, 1)
	ctx := ctxTimeout(t)

	require.NoError(t, c.search(ctx))
	r.wg.Wait()
	snap := activeQID.Load()
	require.NotNil(t, snap, "the refresh must have applied")
	require.Equal(t, qidNewSearch, snap.ids["SearchTimeline"], "the refreshed id is in the snapshot ...")

	require.NoError(t, c.search(ctx), "... but the operator's pin still serves requests")
	ids := w.requestedIDs()
	assert.Equal(t, pinned, ids[len(ids)-1])
}

// F5 — stale fallback: a failed fetch keeps the last good map, calls keep
// working, and the failure alerts once per ok->failed transition.
//
// Mutation: in queryid_refresh.go run() add `activeQID.Store(nil)` to the
// failure branch (clear the map on failure) => RED.
func TestQIDRefresh_F5_FetchFailureKeepsLastGoodMap(t *testing.T) {
	r, clk := useQIDRefresher(t)
	var alerts atomic.Int32
	r.setAlertHook(func(topic string, _ any) {
		if topic == "queryid.refresh_failed" {
			alerts.Add(1)
		}
	})
	w := newQIDWorld(t)
	w.retire("SearchTimeline", qidNewSearch)
	c, _ := newQIDClient(t, w, 1)
	ctx := ctxTimeout(t)

	_ = c.search(ctx) // first request: 404 on the compiled id; the first-trigger refresh learns the new one
	r.wg.Wait()
	good := activeQID.Load()
	require.NotNil(t, good)
	require.NoError(t, c.search(ctx), "calls use the refreshed id")

	// The TTL elapses and X stops serving the bundle: the refresh fails.
	w.set(func(w *qidWorld) { w.failFetch = true })
	for i := 0; i < 3; i++ {
		clk.Advance(qidRefreshTTL + time.Hour)
		require.NoError(t, c.search(ctx), "calls keep working on the last good map")
		r.wg.Wait()
	}
	assert.Same(t, good, activeQID.Load(), "a failed refresh must not touch the active map")
	assert.EqualValues(t, 1, alerts.Load(), "three failures in a row alert once, not per attempt")

	// Recovery re-arms the alert.
	w.set(func(w *qidWorld) { w.failFetch = false })
	clk.Advance(qidMinInterval + time.Minute)
	require.NoError(t, c.search(ctx))
	r.wg.Wait()
	assert.NotSame(t, good, activeQID.Load(), "the next success replaces the map")
	w.set(func(w *qidWorld) { w.failFetch = true })
	clk.Advance(qidRefreshTTL + time.Hour)
	require.NoError(t, c.search(ctx))
	r.wg.Wait()
	assert.EqualValues(t, 2, alerts.Load(), "a new ok->failed transition alerts again")
}

// F6 — storm bound: 50 concurrent faults, then 50 more, cause at most one
// refresh per interval even though X keeps faulting (the refresh cannot help).
//
// Mutation: in queryid_refresh.go intervalOK replace its body with `return true`
// (remove the min-interval check) => RED.
func TestQIDRefresh_F6_FaultStormRefreshesOncePerInterval(t *testing.T) {
	r, clk := useQIDRefresher(t)
	w := newQIDWorld(t)
	c, _ := newQIDClient(t, w, 6)
	ctx := ctxTimeout(t)

	require.NoError(t, c.search(ctx))
	r.wg.Wait()
	base := r.attempts.Load() // the first-trigger refresh
	require.EqualValues(t, 1, base)

	// X breaks the id and the bundle does not advertise a fix: faults never stop.
	w.set(func(w *qidWorld) { w.live["SearchTimeline"] = "XBrokeIt_0001" })
	clk.Advance(qidMinInterval + time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.search(ctx) }()
	}
	wg.Wait()
	r.wg.Wait()
	// Count refresh RUNS, not chunk fetches: the shared chunk cache can serve a
	// repeated walk from memory, which would hide a storm.
	assert.Equal(t, base+1, r.attempts.Load(), "50 concurrent faults trigger one refresh")

	clk.Advance(10 * time.Second)
	for i := 0; i < 50; i++ {
		_ = c.search(ctx)
	}
	r.wg.Wait()
	assert.Equal(t, base+1, r.attempts.Load(), "faults inside the interval must not refresh again")
}

// F7 — the refresh metric records trigger and result, plus the changed-ids and
// age gauges.
//
// Mutation: in queryid_refresh.go run() delete the
// `m.recordQIDRefresh(trig, qidResultOK)` call => RED.
func TestQIDRefresh_F7_MetricRecordsTriggerAndResult(t *testing.T) {
	r, clk := useQIDRefresher(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	w := newQIDWorld(t)
	w.retire("SearchTimeline", qidNewSearch)
	c, _ := newQIDClient(t, w, 1)
	c.metrics = newClientMetrics(mp)
	ctx := ctxTimeout(t)

	_ = c.search(ctx) // first trigger, ok, 1 id changed
	r.wg.Wait()
	w.set(func(w *qidWorld) { w.failFetch = true })
	clk.Advance(qidRefreshTTL + time.Hour)
	_ = c.search(ctx) // ttl trigger, fetch_failed
	r.wg.Wait()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	got := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = m
		}
	}

	refreshes, ok := got[metricQIDRefreshes].Data.(metricdata.Sum[int64])
	require.True(t, ok, "queryid.refreshes must be an int64 counter")
	series := map[string]int64{}
	for _, p := range refreshes.DataPoints {
		a := attrMap(p.Attributes)
		series[a[attrTrigger]+"/"+a[attrResult]] = p.Value
	}
	assert.Equal(t, map[string]int64{"first/ok": 1, "ttl/fetch_failed": 1}, series)

	changed, ok := got[metricQIDChanged].Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	require.Len(t, changed.DataPoints, 1)
	assert.EqualValues(t, 1, changed.DataPoints[0].Value, "SearchTimeline differs from the compiled set")

	age, ok := got[metricQIDAge].Data.(metricdata.Gauge[float64])
	require.True(t, ok)
	require.NotEmpty(t, age.DataPoints)
	assert.Greater(t, age.DataPoints[len(age.DataPoints)-1].Value, qidRefreshTTL.Seconds(),
		"the age gauge reports how long the active map has been in force")
}

// The TTL trigger refreshes an old map on the next authenticated request.
func TestQIDRefresh_TTLTriggerPicksUpARotation(t *testing.T) {
	r, clk := useQIDRefresher(t)
	w := newQIDWorld(t)
	c, _ := newQIDClient(t, w, 1)
	ctx := ctxTimeout(t)

	require.NoError(t, c.search(ctx))
	r.wg.Wait()
	w.retire("SearchTimeline", qidNewSearch)
	clk.Advance(qidRefreshTTL + time.Minute)

	_ = c.search(ctx) // the request that finds the TTL elapsed still uses the old id ...
	r.wg.Wait()
	require.NoError(t, c.search(ctx), "... and the next one uses the id the TTL refresh learned")
}

// Features: a refresh adds the flags the bundle declares that the compiled
// baseline lacks, keeps every compiled value and every compiled flag, and
// leaves Followers/Following (their own set) alone. A partial feature extraction
// is rejected without blocking the queryId refresh.
func TestQIDRefresh_FeaturesAdditiveAndGuarded(t *testing.T) {
	t.Run("new flag added, compiled kept", func(t *testing.T) {
		r, _ := useQIDRefresher(t)
		w := newQIDWorld(t)
		w.set(func(w *qidWorld) { w.newFlag = true })
		c, _ := newQIDClient(t, w, 1)
		require.NoError(t, c.search(ctxTimeout(t)))
		r.wg.Wait()

		got := gqlFeatures()
		assert.Equal(t, true, got[qidNewFlag])
		for name, v := range compiledGQLFeatures() {
			assert.Equal(t, v, got[name], "compiled flag %s must keep its value", name)
		}
		assert.Equal(t, true, endpointFor("SearchTimeline").Features[qidNewFlag])
		assert.NotContains(t, endpointFor("Followers").Features, qidNewFlag, "Followers has its own feature set")
		assert.NotContains(t, compiledGQLFeatures(), qidNewFlag, "the compiled baseline is never mutated")
	})

	t.Run("partial feature set rejected, ids still apply", func(t *testing.T) {
		r, _ := useQIDRefresher(t)
		w := newQIDWorld(t)
		w.retire("SearchTimeline", qidNewSearch)
		w.set(func(w *qidWorld) { w.newFlag, w.dropFlags = true, true })
		c, _ := newQIDClient(t, w, 1)
		_ = c.search(ctxTimeout(t))
		r.wg.Wait()

		snap := activeQID.Load()
		require.NotNil(t, snap, "queryIds must apply even when the feature extraction is unusable")
		assert.Equal(t, qidNewSearch, snap.ids["SearchTimeline"])
		assert.Nil(t, snap.features)
		assert.NotContains(t, gqlFeatures(), qidNewFlag)
	})
}
