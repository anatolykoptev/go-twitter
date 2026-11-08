package twitter

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/internal/queryid"
	"github.com/anatolykoptev/go-twitter/internal/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestXtidFailureStreak_LogsOncePerStreakAndIntervalAndRecovers(t *testing.T) {
	var s xtidFailureStreak
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	logNow, sup := s.onFailure(t0)
	assert.True(t, logNow, "first failure of a streak logs")
	assert.Zero(t, sup)

	for i := 1; i <= 3; i++ {
		logNow, _ = s.onFailure(t0.Add(time.Duration(i) * time.Minute))
		assert.False(t, logNow, "failures inside the interval stay quiet")
	}

	logNow, sup = s.onFailure(t0.Add(xtidFailureLogInterval))
	assert.True(t, logNow, "one more line once the interval has passed")
	assert.Equal(t, 3, sup, "the line reports how many failures it stands for")

	s.onSuccess()
	logNow, sup = s.onFailure(t0.Add(xtidFailureLogInterval + time.Second))
	assert.True(t, logNow, "a new streak after a success logs at once, interval or not")
	assert.Zero(t, sup)
}

// A request whose id cannot be generated still goes out and is counted.
func TestDoRequest_XtidFailureIsCountedAndRequestContinues(t *testing.T) {
	var served atomic.Int32
	f := newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		assert.Empty(t, r.Header.Get("x-client-transaction-id"))
		_, _ = w.Write([]byte(`{"data":{"list":{"tweets_timeline":{"timeline":{` + instructions() + `}}}}}`))
	}, "xtid_fail_acc")

	for i := 0; i < 2; i++ {
		_, err := f.c.GetListTweets(context.Background(), "L1", 20)
		require.NoError(t, err)
	}
	assert.EqualValues(t, 2, served.Load(), "both requests reached the server without the header")

	m, ok := f.collect(t)[telemetry.MetricXtidFailures]
	require.True(t, ok, "xtid.failures must be reported")
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	assert.Equal(t, int64(2), total)
}

// refuseWarmDoer answers every warm-page fetch with a permanent 403, so the
// transaction-id generator cannot be built.
type refuseWarmDoer struct{}

func (refuseWarmDoer) DoCtx(context.Context, string, string, map[string]string, io.Reader) ([]byte, map[string]string, int, error) {
	return nil, nil, 403, nil
}

// webClientFailureValue is the web client's btoa(`e:${error}`) (main bundle
// module 265390, function a) for the TypeError its generator throws when the
// page's meta/SVG nodes are missing. The text is derived statically from the
// generator's first unguarded dereference (childNodes of an undefined node), not
// observed at runtime. Spelled out here, not derived from the package under test.
const webClientFailureValue = "ZTpUeXBlRXJyb3I6IENhbm5vdCByZWFkIHByb3BlcnRpZXMgb2YgdW5kZWZpbmVkIChyZWFkaW5nICdjaGlsZE5vZGVzJyk="

// A current-wire request whose generator fails still carries the header, with
// the value the web client sends in that situation.
//
// Mutation: client.go doRequestWithBody, the xtid failure branch — delete the
// `if c.wireFor(acc, bc) == wireChrome { headers[...] = xtidFailureValue }`
// assignment => RED here (header absent).
func TestChromeWire_GeneratorFailureSendsWebClientValue(t *testing.T) {
	p := chromeProfile(t, stealth.ProfileChrome152, "chrome", "macos")
	c, err := NewClient(ClientConfig{DefaultProxy: deadProxy})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.warmDoerOverride = refuseWarmDoer{}
	bc, rec := recorderClient(t, p)
	c.owned.setWire(bc, wireGenFor(p.TLSProfile))

	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET", "https://x.com/i/api/graphql/w/Op", twitterHeaders("AUTH", "CT0", p.UserAgent), nil)

	got := rec.last()
	assert.Equal(t, webClientFailureValue, got.Headers["x-client-transaction-id"])
	raw, decErr := base64.StdEncoding.DecodeString(got.Headers["x-client-transaction-id"])
	require.NoError(t, decErr)
	assert.Equal(t, "e:", string(raw[:2]), "web client form: btoa(`e:${error}`)")
	assert.Contains(t, wireNames(got), "x-client-transaction-id", "the header is on the wire, in its browser position")
	assert.True(t, xtidFailing(c), "the failure is still recorded")
}

// The frozen legacy shape keeps omitting the header when the generator fails.
//
// Mutation: client.go doRequestWithBody, the same branch — drop the wireChrome
// condition so every wire gets the value => RED here (header present).
func TestLegacyFreeze_GeneratorFailureOmitsHeader(t *testing.T) {
	c, bc, rec := legacyClient(t)
	c.warmDoerOverride = refuseWarmDoer{}

	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET", apiGraphQL, twitterHeaders("AUTH", "CT0", legacyProfile.UserAgent), nil)

	got := rec.last()
	_, present := got.Headers["x-client-transaction-id"]
	assert.False(t, present, "legacy wire sends no x-client-transaction-id when the generator fails")
	assert.True(t, xtidFailing(c), "the failure is still recorded")
}

func xtidFailing(c *Client) bool {
	c.xtidFail.mu.Lock()
	defer c.xtidFail.mu.Unlock()
	return c.xtidFail.failing
}

var (
	staleKey = []byte{0, 20, 30, 40, 50, 0, 70, 80}
	freshKey = []byte{0, 21, 31, 41, 51, 0, 71, 81}
)

// rotatingWarmBackend stands in for x.com's page set. The verification key it
// serves changes after the first warm-page fetch, as when X ships new key
// material while the client still holds the old one.
type rotatingWarmBackend struct {
	mu       sync.Mutex
	pages    int           // warm-page fetches so far
	pageWait time.Duration // delay on every fetch after the first
}

func (b *rotatingWarmBackend) pageFetches() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pages
}

func (b *rotatingWarmBackend) DoCtx(ctx context.Context, _, u string, _ map[string]string, _ io.Reader) ([]byte, map[string]string, int, error) {
	switch u {
	case "https://x.com/tesla":
		b.mu.Lock()
		b.pages++
		n := b.pages
		b.mu.Unlock()
		if n > 1 && b.pageWait > 0 {
			select {
			case <-time.After(b.pageWait):
			case <-ctx.Done():
				return nil, nil, 0, ctx.Err()
			}
		}
		key := staleKey
		if n > 1 {
			key = freshKey
		}
		return []byte(`<html><head><meta name="twitter-site-verification" content="` + base64.StdEncoding.EncodeToString(key) + `">` +
			`<script type="module" src="` + testViteBase + `entry-client-0000111122223333.js"></script>` +
			`</head><body>` + testSVG + `</body></html>`), nil, 200, nil
	case testViteBase + "entry-client-0000111122223333.js":
		return []byte(`const s=()=>import("./sign.o-0123456789abcdef.js");`), nil, 200, nil
	case testViteBase + "sign.o-0123456789abcdef.js":
		return []byte(testIndicesJS), nil, 200, nil
	}
	return nil, nil, 404, nil
}

// idBuiltWith reports whether a transaction id was built from key (the first
// byte is the XOR mask, the next len(key) bytes are the key under it).
func idBuiltWith(id string, key []byte) bool {
	raw, err := base64.RawStdEncoding.DecodeString(id)
	if err != nil || len(raw) < 1+len(key) {
		return false
	}
	for i := range key {
		if raw[1+i]^raw[0] != key[i] {
			return false
		}
	}
	return true
}

// staleXtidServer answers 404 to any request whose transaction id was not built
// from acceptKey (nil accepts nothing), 200 otherwise. When barrier > 0 it holds
// every response until that many requests have arrived.
type staleXtidServer struct {
	srv       *httptest.Server
	hits      atomic.Int32
	acceptKey []byte
	barrier   int32
	arrived   chan struct{}
}

func newStaleXtidServer(t *testing.T, acceptKey []byte, barrier int) *staleXtidServer {
	t.Helper()
	s := &staleXtidServer{acceptKey: acceptKey, barrier: int32(barrier), arrived: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.hits.Add(1)
		if s.barrier > 0 && n <= s.barrier {
			if n == s.barrier {
				close(s.arrived)
			}
			select {
			case <-s.arrived:
			case <-time.After(5 * time.Second):
			}
		}
		if s.acceptKey != nil && idBuiltWith(r.Header.Get("x-client-transaction-id"), s.acceptKey) {
			_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Not Found"}]}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// currentWire puts the accounts on the current wire generation (the refresh is
// not offered to the frozen legacy one).
func currentWire(accs ...*Account) {
	for _, a := range accs {
		a.Profile.TLSProfile = stealth.ProfileChrome152
	}
}

// withMetrics installs a readable meter on c.
func withMetrics(t *testing.T, c *Client) *metricsFixture {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	c.metrics = newClientMetrics(mp)
	return &metricsFixture{c: c, reader: reader}
}

// resultCounts sums a counter by its twitter.result attribute.
func resultCounts(t *testing.T, f *metricsFixture, name string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	m, ok := f.collect(t)[name]
	if !ok {
		return out
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	for _, dp := range sum.DataPoints {
		v, _ := dp.Attributes.Value("twitter.result")
		out[v.AsString()] += dp.Value
	}
	return out
}

func (s *staleXtidServer) url() string { return s.srv.URL + "/i/api/graphql/x/" + poolWaitEndpoint }

// A 404 on a live route caused by stale key material: the client refreshes the
// generator once and the same request then succeeds, without charging or
// benching the account.
//
// Mutation: request.go doPoolRequest — delete the `if c.retryAfterXtidRefresh(`
// block in the endpoint-fault arm => RED (the call fails with ErrEndpoint).
func TestXtidRefresh_Stale404ThenSuccess(t *testing.T) {
	srv := newStaleXtidServer(t, freshKey, 0)
	backend := &rotatingWarmBackend{}
	c, accs := newPoolWaitClient(t, 1)
	currentWire(accs...)
	f := withMetrics(t, c)
	c.warmDoerOverride = backend
	c.xtidMgr.SetForcedRefreshMinAge(0)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	body, _, err := c.doGET(ctx, poolWaitEndpoint, srv.url())

	require.NoError(t, err)
	assert.Contains(t, string(body), `"ok":true`)
	assert.EqualValues(t, 2, srv.hits.Load(), "the stale request, then one retry with the refreshed keys")
	assert.Equal(t, 2, backend.pageFetches(), "init + exactly one refresh")
	total, failed, _ := accs[0].Stats()
	assert.Zero(t, failed, "a stale-key 404 is not the account's fault")
	assert.Equal(t, 1, total, "only the successful retry is the account's request")
	assert.True(t, accs[0].IsActive())
	assert.Equal(t, map[string]int64{"refreshed": 1}, resultCounts(t, f, "twitter.client.xtid.refreshes"))
	assert.Equal(t, map[string]int64{"rescued": 1}, resultCounts(t, f, "twitter.client.xtid.retries"))
}

// The refresh is bounded to once per call: a 404 that survives it takes the
// ordinary endpoint-fault path, no further refresh or retry.
//
// Mutations: xtid_refresh.go `st.attempted = true` => `false` is RED (a third
// request and a second page fetch); request.go doPoolRequest — delete the
// c.noteEndpointFault call in the retry branch => RED on the fault count.
func TestXtidRefresh_SecondFaultIsNotRetried(t *testing.T) {
	srv := newStaleXtidServer(t, nil, 0) // 404 whatever the id
	backend := &rotatingWarmBackend{}
	c, accs := newPoolWaitClient(t, 1)
	currentWire(accs...)
	f := withMetrics(t, c)
	// A threshold far above the test's faults keeps the queryId refresher from
	// firing, so its fault history is what the endpoint-fault bookkeeping left.
	// (and a fresh map keeps its first/TTL refresh from fetching pages too).
	c.qid = newQIDRefresher(queryid.Config{TTL: time.Hour, FaultWindow: time.Minute, FaultThreshold: 10, Timeout: time.Second})
	c.qid.LastSuccessNS.Store(time.Now().UnixNano())
	c.warmDoerOverride = backend
	c.xtidMgr.SetForcedRefreshMinAge(0)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, err := c.doGET(ctx, poolWaitEndpoint, srv.url())

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEndpoint)
	assert.EqualValues(t, 2, srv.hits.Load(), "original + one retry after the refresh, nothing more")
	assert.Equal(t, 2, backend.pageFetches(), "init + exactly one refresh")
	assert.Equal(t, map[string]int64{"refreshed": 1}, resultCounts(t, f, "twitter.client.xtid.refreshes"))
	assert.Equal(t, map[string]int64{"still_404": 1}, resultCounts(t, f, "twitter.client.xtid.retries"))
	c.qid.Mu.Lock()
	faults := len(c.qid.Faults[poolWaitEndpoint])
	c.qid.Mu.Unlock()
	assert.Equal(t, 2, faults, "both 404s count toward the queryId refresher, the retried one included")
}

// Many calls hitting the stale-key 404 together trigger one refresh.
//
// Mutation: xtid/manager.go Refresh — run the rebuild directly instead of
// through m.sf.DoChan => RED (page fetched once per call).
func TestXtidRefresh_ConcurrentFaultsShareOneRefresh(t *testing.T) {
	const calls = 6
	srv := newStaleXtidServer(t, freshKey, calls)
	backend := &rotatingWarmBackend{pageWait: 300 * time.Millisecond}
	c, accs := newPoolWaitClient(t, calls)
	currentWire(accs...)
	c.warmDoerOverride = backend
	c.xtidMgr.SetForcedRefreshMinAge(0)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errs := make([]error, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = c.doGET(ctx, poolWaitEndpoint, srv.url())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "call %d", i)
	}
	assert.Equal(t, 2, backend.pageFetches(), "init + ONE shared refresh for %d concurrent faults", calls)
}

// Writes get the same single refresh-and-retry: a 404 means the route did not
// run, so the second send cannot duplicate the write.
//
// Mutation: request.go doPOST — delete the `if c.retryAfterXtidRefresh(` block
// => RED.
func TestXtidRefresh_PostRetriesOnceAfterRefresh(t *testing.T) {
	srv := newStaleXtidServer(t, freshKey, 0)
	backend := &rotatingWarmBackend{}
	c, accs := newPoolWaitClient(t, 1)
	currentWire(accs...)
	c.warmDoerOverride = backend
	c.xtidMgr.SetForcedRefreshMinAge(0)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	body, err := c.doPOST(ctx, accs[0], "CreateTweet", srv.url(), []byte(`{}`))

	require.NoError(t, err)
	assert.True(t, bytes.Contains(body, []byte(`"ok":true`)))
	assert.EqualValues(t, 2, srv.hits.Load())
	assert.Equal(t, 2, backend.pageFetches())
}

// Keys younger than the floor (the normal case for a request right after init)
// are not the cause: the 404 stays an endpoint fault, with no extra request and
// no page fetch.
//
// Mutation: xtid/manager.go refreshDue — drop the `>= m.forcedMinAge` term
// => RED.
func TestXtidRefresh_YoungKeysKeepEndpointFaultSemantics(t *testing.T) {
	srv := newStaleXtidServer(t, nil, 0)
	backend := &rotatingWarmBackend{}
	c, accs := newPoolWaitClient(t, 1)
	currentWire(accs...)
	f := withMetrics(t, c)
	c.warmDoerOverride = backend // default floor: one minute

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, err := c.doGET(ctx, poolWaitEndpoint, srv.url())

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEndpoint)
	assert.EqualValues(t, 1, srv.hits.Load())
	assert.Equal(t, 1, backend.pageFetches(), "init only")
	assert.Equal(t, map[string]int64{"suppressed_floor": 1}, resultCounts(t, f, "twitter.client.xtid.refreshes"))
	assert.Empty(t, resultCounts(t, f, "twitter.client.xtid.retries"))
}

// The frozen legacy wire is frozen in behaviour too: a 404 there is an endpoint
// fault exactly as before — one request, no forced page fetch, no refresh
// counted — even with the key-age floor out of the way.
//
// Mutation: xtid_refresh.go retryAfterXtidRefresh — drop the
// `c.wireFor(acc, bc) != wireChrome ||` term => RED (second request, second
// page fetch).
func TestXtidRefresh_LegacyWireKeepsOldCadence(t *testing.T) {
	srv := newStaleXtidServer(t, freshKey, 0) // would be rescued by a refresh
	backend := &rotatingWarmBackend{}
	c, accs := newPoolWaitClient(t, 1)
	require.Equal(t, wireLegacy, accountWire(accs[0]), "fixture accounts are legacy-wire")
	f := withMetrics(t, c)
	c.warmDoerOverride = backend
	c.xtidMgr.SetForcedRefreshMinAge(0)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, err := c.doGET(ctx, poolWaitEndpoint, srv.url())

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEndpoint)
	assert.EqualValues(t, 1, srv.hits.Load(), "exactly one request on the legacy wire")
	assert.Equal(t, 1, backend.pageFetches(), "init only, no forced refresh")
	assert.Empty(t, resultCounts(t, f, "twitter.client.xtid.refreshes"))
}
