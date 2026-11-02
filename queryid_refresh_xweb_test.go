package twitter

import (
	"bytes"
	"context"
	"github.com/anatolykoptev/go-twitter/internal/queryid"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anatolykoptev/go-twitter/internal/gqlextract"
	"github.com/anatolykoptev/go-twitter/internal/telemetry"
)

// x-web source for the runtime refresh (#119 phase 1). The fake X serves, next
// to the legacy ops chunk, an x-web chunk holding the Relay artifact of
// tweetNotificationsQuery under a rotated id.

const (
	xwNewID    = "XWebRotatedID_0001"
	xwChunkURL = testViteBase + "assets/tweet-notifications-T1.js"
	xwOldID    = "VcNhzcrF-pRo8nSftDQThA" // the id inside the saved fixture chunk
)

// xwWorld layers the x-web chunk over qidWorld.
type xwWorld struct {
	*qidWorld
	chunk []byte            // served at xwChunkURL
	more  map[string][]byte // other x-web chunks by URL
}

func (x *xwWorld) DoCtx(ctx context.Context, m, u string, h map[string]string, b io.Reader) ([]byte, map[string]string, int, error) {
	x.mu.Lock()
	body, isChunk := x.more[u]
	if u == xwChunkURL {
		body, isChunk = x.chunk, true
	}
	if isChunk {
		x.hits[u]++
	}
	x.mu.Unlock()
	if isChunk {
		return body, nil, 200, nil
	}
	return x.qidWorld.DoCtx(ctx, m, u, h, b)
}

func xwFixtureChunk(t *testing.T, id string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("internal", "gqlextract", "testdata", "xweb", "tweet-notifications.js"))
	require.NoError(t, err)
	return bytes.ReplaceAll(b, []byte(xwOldID), []byte(id))
}

// newXWebClient wires a client over a world whose entry script links the x-web
// chunk (an `import("./assets/…")` the legacy walk ignores).
func newXWebClient(t *testing.T, chunk []byte) (*Client, *xwWorld) {
	t.Helper()
	w := &xwWorld{qidWorld: newQIDWorld(t), chunk: chunk}
	w.extraJS = `export const lazy=()=>import("./assets/tweet-notifications-T1.js");`
	w.start(t)
	c, _ := newPoolWaitClient(t, 1)
	c.warmDoerOverride = w
	c.xtidMgr.SetWarmPageHook(observeWarmPage)
	return c, w
}

func TestQIDRefresh_XWeb_FlagOnAppliesRotatedNativeID(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.Cfg.XWeb = true
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))

	_ = c.search(ctxTimeout(t))
	r.WG.Wait()

	require.NotNil(t, queryid.Active.Load(), "refresh must have applied")
	assert.Equal(t, xwNewID, endpointFor(tweetNotificationsOp).ID)
	assert.Equal(t, Endpoints["SearchTimeline"].ID, endpointFor("SearchTimeline").ID, "legacy ops are untouched")
	assert.Equal(t, 1, w.hits[xwChunkURL])
}

// Default OFF: the x-web chunk is not even requested, and the id stays put.
//
// Mutation: in queryid_refresh.go build() make the `if r.cfg.xweb` guard
// unconditional => RED.
func TestQIDRefresh_XWeb_DefaultOffNeverCrawls(t *testing.T) {
	r, _ := useQIDRefresher(t)
	require.False(t, r.Cfg.XWeb, "the shipped default must be off")
	require.False(t, queryid.DefaultConfig().XWeb)
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))

	_ = c.search(ctxTimeout(t))
	r.WG.Wait()

	assert.Zero(t, w.hits[xwChunkURL], "x-web chunk fetched with the flag off")
	assert.Equal(t, Endpoints[tweetNotificationsOp].ID, endpointFor(tweetNotificationsOp).ID)
}

func TestDefaultQIDConfig_XWebFollowsEnv(t *testing.T) {
	t.Setenv(queryid.EnvXWeb, "on")
	assert.True(t, queryid.DefaultConfig().XWeb)
	t.Setenv(queryid.EnvXWeb, "")
	assert.False(t, queryid.DefaultConfig().XWeb)
}

// An x-web-only bundle (the legacy walk finds none of the known ops) is still
// rejected as partial without the flag, and applies the native id with it.
//
// Mutation: in build() delete the `errors.Is(err, errQIDTooFew)` branch => RED.
func TestQIDRefresh_XWeb_OnlyBundleAppliesNativeIDAlone(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "flag off", true: "flag on"}[on], func(t *testing.T) {
			r, _ := useQIDRefresher(t)
			r.Cfg.XWeb = on
			c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))
			w.set(func(w *qidWorld) { w.advertised = map[string]string{} })

			_ = c.search(ctxTimeout(t))
			r.WG.Wait()

			if !on {
				assert.Nil(t, queryid.Active.Load(), "partial legacy bundle without x-web must change nothing")
				return
			}
			require.NotNil(t, queryid.Active.Load())
			assert.Equal(t, xwNewID, endpointFor(tweetNotificationsOp).ID)
			assert.Equal(t, Endpoints["SearchTimeline"].ID, endpointFor("SearchTimeline").ID)
		})
	}
}

// A malformed legacy id stays fatal even when x-web ids are available.
func TestQIDRefresh_XWeb_MalformedLegacyIDStillRejects(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.Cfg.XWeb = true
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))
	w.set(func(w *qidWorld) { w.extraJS += `{queryId:"BAD/ID",operationName:"UserTweets"},` })

	_ = c.search(ctxTimeout(t))
	r.WG.Wait()

	assert.Nil(t, queryid.Active.Load())
}

// Only native ops of the right kind take an x-web id; a mutation artifact
// carrying the native name is ignored.
func TestQIDRefresh_XWeb_WrongKindIgnored(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.Cfg.XWeb = true
	chunk := bytes.ReplaceAll(xwFixtureChunk(t, xwNewID), []byte("operationKind:`query`"), []byte("operationKind:`mutation`"))
	c, _ := newXWebClient(t, chunk)

	_ = c.search(ctxTimeout(t))
	r.WG.Wait()

	assert.Equal(t, Endpoints[tweetNotificationsOp].ID, endpointFor(tweetNotificationsOp).ID)
}

// A panic on a hostile x-web chunk must not take the legacy result down.
func TestQIDRefresh_XWeb_PanicKeepsLegacyRefresh(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.Cfg.XWeb = true
	orig := queryid.ConsumeXWeb
	queryid.ConsumeXWeb = func(context.Context, *gqlextract.XWebExtractor, string, []byte) { panic("hostile chunk") }
	t.Cleanup(func() { queryid.ConsumeXWeb = orig })
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))
	w.retire("SearchTimeline", qidNewSearch)

	_ = c.search(ctxTimeout(t))
	r.WG.Wait()

	require.NotNil(t, queryid.Active.Load(), "legacy refresh must survive an x-web panic")
	assert.Equal(t, qidNewSearch, endpointFor("SearchTimeline").ID)
	assert.Equal(t, Endpoints[tweetNotificationsOp].ID, endpointFor(tweetNotificationsOp).ID)
}

// xwebNativeOps is a deliberate list: an operation joins it in the same change
// that switches its request to the x-web shape. A bare addition (e.g. a legacy
// op such as SearchTimeline) must fail here.
//
// Mutation: add "SearchTimeline" to internal/xwebnative Ops() => RED.
func TestXWebNativeOps_AreExactlyTheMigratedOnes(t *testing.T) {
	assert.Equal(t, map[string]string{tweetNotificationsOp: "query"}, queryid.XWebNativeOps)
	for op := range queryid.XWebNativeOps {
		ep, ok := Endpoints[op]
		require.True(t, ok, "%s must be a known operation", op)
		assert.Equal(t, op, ep.Name, "a native op is requested under its Relay name")
	}
}

// qidSeries collects refreshes{trigger/result} from a manual reader.
func qidSeries(t *testing.T, reader sdkmetric.Reader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	series := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != telemetry.MetricQIDRefreshes {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, p := range sum.DataPoints {
				a := attrMap(p.Attributes)
				series[a[telemetry.AttrTrigger]+"/"+a[telemetry.AttrResult]] = p.Value
			}
		}
	}
	return series
}

// F2 — a Vite-only page whose legacy chunks were ALL fetched and held no legacy
// operation is a real x-web-only page: the native ids apply, with a distinct
// outcome label and no alert.
func TestQIDRefresh_XWeb_OnlyPageReportsDistinctOutcome(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.Cfg.XWeb = true
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	var alerts atomic.Int32
	r.SetAlertHook(func(string, any) { alerts.Add(1) })
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))
	c.metrics = newClientMetrics(mp)
	w.set(func(w *qidWorld) { w.advertised = map[string]string{} })

	_ = c.search(ctxTimeout(t))
	r.WG.Wait()

	require.NotNil(t, queryid.Active.Load())
	assert.Equal(t, map[string]int64{"first/" + queryid.ResultOKXWebOnly: 1}, qidSeries(t, reader))
	assert.Zero(t, alerts.Load(), "a genuine x-web-only page is not a failure")
}

// F2 — a legacy walk that lost chunks looks the same as "few ops found". An
// x-web id in hand must NOT turn that into a success: it stays a rejected
// refresh with the alert, the streak and the backoff.
//
// Mutation: in queryid_refresh.go build() drop `counted.failures.Load() == 0`
// from the xwebOnly condition => RED.
func TestQIDRefresh_XWeb_FlakyLegacyWalkStillFails(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.Cfg.XWeb = true
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	var alerts atomic.Int32
	r.SetAlertHook(func(topic string, _ any) {
		if topic == "queryid.refresh_failed" {
			alerts.Add(1)
		}
	})
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))
	c.metrics = newClientMetrics(mp)
	// a legacy chunk the walk follows but X fails to serve (404 in the fake)
	w.set(func(w *qidWorld) {
		w.advertised = map[string]string{}
		w.extraJS += `export const flaky=()=>import("./flaky-legacy-chunk.js");`
	})

	_ = c.search(ctxTimeout(t))
	r.WG.Wait()

	assert.Nil(t, queryid.Active.Load(), "a partial legacy walk must not apply, x-web id or not")
	assert.Equal(t, map[string]int64{"first/" + queryid.ResultRejected: 1}, qidSeries(t, reader))
	assert.EqualValues(t, 1, alerts.Load(), "the failure must alert")
}

// F3 — two chunks declare different ids for one native op: neither is trusted.
//
// Mutation: in walkXWeb delete the `len(conflicts[op]) > 0` case => RED.
func TestQIDRefresh_XWeb_ConflictingNativeIDsAreDropped(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.Cfg.XWeb = true
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))
	const second = testViteBase + "assets/tweet-notifications-T2.js"
	w.more = map[string][]byte{second: xwFixtureChunk(t, "XWebOtherID_0002")}
	w.set(func(w *qidWorld) {
		w.extraJS += `export const lazy2=()=>import("./assets/tweet-notifications-T2.js");`
	})
	w.retire("SearchTimeline", qidNewSearch) // a legacy change keeps the refresh observable

	_ = c.search(ctxTimeout(t))
	r.WG.Wait()

	require.NotNil(t, queryid.Active.Load())
	assert.Positive(t, w.hits[xwChunkURL], "first chunk fetched")
	assert.Positive(t, w.hits[second], "both conflicting chunks must have been read")
	assert.Equal(t, Endpoints[tweetNotificationsOp].ID, endpointFor(tweetNotificationsOp).ID,
		"a conflicted native op keeps its current id")
	assert.Equal(t, qidNewSearch, endpointFor("SearchTimeline").ID, "legacy refresh still applies")
}

// stampFetcher serves a three-chunk chain and records when each fetch started.
type stampFetcher struct {
	mu     sync.Mutex
	starts []time.Time
}

func (f *stampFetcher) Fetch(_ context.Context, u string) ([]byte, error) {
	f.mu.Lock()
	f.starts = append(f.starts, time.Now())
	f.mu.Unlock()
	switch u {
	case testViteBase + "entry.js":
		return []byte(`import"./assets/a.js"`), nil
	case testViteBase + "assets/a.js":
		return []byte(`import"./b.js"`), nil
	}
	return []byte(`export{}`), nil
}

// F4 — the runtime crawl is polite: consecutive chunk fetches are spaced by
// qidXWebGap, like cmd/xweb-report.
//
// Mutation: in walkXWeb drop `Gap: qidXWebGap` from the CrawlOptions => RED.
func TestQIDRefresh_XWeb_CrawlIsPaced(t *testing.T) {
	r, _ := useQIDRefresher(t)
	f := &stampFetcher{}
	r.WalkXWeb(context.Background(), f, &queryid.WarmPage{Vite: []string{testViteBase + "entry.js"}})

	require.Len(t, f.starts, 3)
	for i := 1; i < len(f.starts); i++ {
		assert.GreaterOrEqual(t, f.starts[i].Sub(f.starts[i-1]), queryid.XWebGap-10*time.Millisecond,
			"fetch %d started too soon after fetch %d", i, i-1)
	}
}
