package twitter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/anatolykoptev/go-twitter/xpff"
	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// failFetcher makes the xtid manager fail instantly (permanent 4xx) so request
// paths run without any network access; the transaction-id header is optional.
type failFetcher struct{}

func (failFetcher) Fetch(context.Context, string) ([]byte, error) {
	return nil, errors.New("offline test fetcher HTTP 404")
}

// metricsFixture is a Client wired to an httptest server impersonating x.com and
// to a ManualReader-backed MeterProvider.
type metricsFixture struct {
	c      *Client
	reader *sdkmetric.ManualReader
	acc    *Account
	now    time.Time
	hits   atomicCounter
}

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (a *atomicCounter) inc() { a.mu.Lock(); a.n++; a.mu.Unlock() }

// snowflakeAt builds a snowflake ID whose embedded time is t (ms precision).
func snowflakeAt(t time.Time) string {
	return strconv.FormatUint(uint64(t.UnixMilli()-twitterEpochMs)<<22, 10)
}

func tweetEntry(id string) string {
	return fmt.Sprintf(`{"entryId":"tweet-%s","content":{"entryType":"TimelineTimelineItem","__typename":"TimelineTimelineItem","itemContent":{"__typename":"TimelineTweet","tweet_results":{"result":{"__typename":"Tweet","rest_id":%q,"legacy":{"full_text":"t","user_id_str":"999"}}}}}}`, id, id)
}

func instructions(ids ...string) string {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, tweetEntry(id))
	}
	return `"instructions":[{"type":"TimelineAddEntries","entries":[` + strings.Join(entries, ",") + `]}]`
}

// newMetricsFixture starts the fake x.com. handler decides the response per
// request (path ends in the operation name).
func newMetricsFixture(t *testing.T, handler http.HandlerFunc, username string) *metricsFixture {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	origBase, origJitter, origBackoff := twitterBase, stealth.DefaultJitter, stealth.DefaultBackoff
	twitterBase = srv.URL + "/i/api/graphql"
	stealth.DefaultJitter.Min, stealth.DefaultJitter.Max = time.Millisecond, 2*time.Millisecond
	stealth.DefaultBackoff.InitialWait, stealth.DefaultBackoff.MaxWait = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { twitterBase, stealth.DefaultJitter, stealth.DefaultBackoff = origBase, origJitter, origBackoff })

	bc, err := stealth.NewClient(stealth.WithoutSSRFGuard()) // test-only: the fake x.com is loopback
	require.NoError(t, err)

	acc := &Account{Username: username, AuthToken: "auth", CT0: "ct0", UserAgent: defaultUserAgent}
	acc.active = true
	acc.rateLimiter = ratelimit.NewLimiter(ratelimit.DefaultConfig)
	acc.HealthTracker = pool.DefaultHealthTracker()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	now := time.Date(2026, 10, 13, 4, 14, 26, 0, time.UTC)
	c := &Client{
		client:  bc,
		pool:    pool.New([]*Account{acc}, pool.DefaultConfig()),
		xtidMgr: xtid.NewManager(failFetcher{}),
		xpffGen: xpff.New("guest", defaultUserAgent),
		cfg:     ClientConfig{SessionDir: t.TempDir(), DisableGuestFallback: true},
		metrics: newClientMetrics(mp),
	}
	c.metrics.now = func() time.Time { return now }
	c.registerPoolGauge(mp)

	return &metricsFixture{c: c, reader: reader, acc: acc, now: now}
}

func (f *metricsFixture) collect(t *testing.T) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, f.reader.Collect(context.Background(), &rm))
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		assert.Equal(t, meterName, sm.Scope.Name)
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func histPoints(t *testing.T, all map[string]metricdata.Metrics, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	m, present := all[name]
	require.True(t, present, "instrument %s recorded nothing", name)
	h, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "%s is not a float64 histogram", name)
	return h.DataPoints
}

func attrMap(s attribute.Set) map[string]string {
	out := map[string]string{}
	for _, kv := range s.ToSlice() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func userTweetsBody(ids ...string) string {
	return `{"data":{"user":{"result":{"timeline":{"timeline":{` + instructions(ids...) + `}}}}}}`
}

func listBody(ids ...string) string {
	return `{"data":{"list":{"tweets_timeline":{"timeline":{` + instructions(ids...) + `}}}}}`
}

// F1 — journey: a consumer installs a MeterProvider, calls GetUserTweets, and
// sees one request.duration point (UserTweets, ok). The legacy MetricsHook keeps
// firing exactly as before.
func TestMetrics_UserTweetsJourney_RecordsDurationOK(t *testing.T) {
	var f *metricsFixture
	f = newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(f.now.Add(-5 * time.Second)))))
	}, "journey_acc")

	type hookCall struct {
		endpoint             string
		success, rateLimited bool
	}
	var calls []hookCall
	f.c.cfg.MetricsHook = func(e string, s, r bool) { calls = append(calls, hookCall{e, s, r}) }

	tweets, err := f.c.GetUserTweets(context.Background(), "999", 20)
	require.NoError(t, err)
	require.Len(t, tweets, 1)

	pts := histPoints(t, f.collect(t), metricRequestDuration)
	require.Len(t, pts, 1, "exactly one request attempt was made")
	assert.Equal(t, map[string]string{attrOperation: "UserTweets", attrOutcome: outcomeOK}, attrMap(pts[0].Attributes))
	assert.Equal(t, uint64(1), pts[0].Count)
	assert.Equal(t, requestDurationBuckets, pts[0].Bounds)

	assert.Equal(t, []hookCall{{"UserTweets", true, false}}, calls, "legacy MetricsHook must be unchanged")
}

// F2 — a 429 is recorded as rate_limited, not ok and not other.
func TestMetrics_429RecordsRateLimited(t *testing.T) {
	f := newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-rate-limit-reset", strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{}`))
	}, "limited_acc")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := f.c.GetUserTweets(ctx, "999", 20)
	require.Error(t, err)

	pts := histPoints(t, f.collect(t), metricRequestDuration)
	require.NotEmpty(t, pts)
	for _, p := range pts {
		a := attrMap(p.Attributes)
		assert.Equal(t, "UserTweets", a[attrOperation])
		assert.Equal(t, outcomeRateLimited, a[attrOutcome], "a 429 must be rate_limited, got %v", a)
	}
}

// F3 — journey: GetListTweets against a fixture whose newest tweet is 5 s older
// than the (injected) clock records freshness ~5 s for ListLatestTweetsTimeline.
func TestMetrics_ListTweetsJourney_RecordsFreshness(t *testing.T) {
	var f *metricsFixture
	f = newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		// newest = now-5s; the older ones must not win.
		_, _ = w.Write([]byte(listBody(
			snowflakeAt(f.now.Add(-300*time.Second)),
			snowflakeAt(f.now.Add(-5*time.Second)),
			snowflakeAt(f.now.Add(-60*time.Second)),
		)))
	}, "list_acc")

	tweets, err := f.c.GetListTweets(context.Background(), "L1", 20)
	require.NoError(t, err)
	require.Len(t, tweets, 3)

	pts := histPoints(t, f.collect(t), metricFreshness)
	require.Len(t, pts, 1)
	assert.Equal(t, map[string]string{attrOperation: "ListLatestTweetsTimeline"}, attrMap(pts[0].Attributes))
	assert.Equal(t, uint64(1), pts[0].Count)
	assert.InDelta(t, 5.0, pts[0].Sum, 0.01, "freshness is the age of the NEWEST tweet")
	assert.Equal(t, freshnessBuckets, pts[0].Bounds)
}

// F4 — no instrument's attributes carry an account handle / user id / list id;
// the attribute key set is exactly the documented one and operation values come
// from the Endpoints registry.
func TestMetrics_AttributesAreBoundedAndAccountFree(t *testing.T) {
	const handle = "secret_handle"
	var f *metricsFixture
	f = newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(f.now.Add(-2 * time.Second)))))
	}, handle)

	_, err := f.c.GetUserTweets(context.Background(), "424242", 20)
	require.NoError(t, err)

	want := map[string][]string{
		metricRequestDuration: {attrOperation, attrOutcome},
		metricFreshness:       {attrOperation},
		metricPoolAccounts:    {attrAccountState},
	}
	got := f.collect(t)
	for name, wantKeys := range want {
		m, ok := got[name]
		require.True(t, ok, "instrument %s missing", name)
		for _, set := range allAttrSets(t, m) {
			var keys []string
			for k, v := range attrMap(set) {
				keys = append(keys, k)
				assert.NotContains(t, v, handle, "%s leaked the account handle in %s", name, k)
				assert.NotContains(t, v, "424242", "%s leaked the user id in %s", name, k)
				if k == attrOperation {
					_, known := Endpoints[v]
					assert.True(t, known, "%s: operation %q is not in the Endpoints registry", name, v)
				}
			}
			sort.Strings(keys)
			wantSorted := append([]string(nil), wantKeys...)
			sort.Strings(wantSorted)
			assert.Equal(t, wantSorted, keys, "%s attribute key set", name)
		}
	}
}

func allAttrSets(t *testing.T, m metricdata.Metrics) []attribute.Set {
	t.Helper()
	var out []attribute.Set
	switch d := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes)
		}
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes)
		}
	default:
		t.Fatalf("unexpected data type %T for %s", m.Data, m.Name)
	}
	require.NotEmpty(t, out, m.Name)
	return out
}

// F5 — SnowflakeTime against a known real tweet and malformed input.
func TestSnowflakeTime(t *testing.T) {
	got, err := SnowflakeTime("2109860265647075443")
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 10, 13, 4, 14, 21, 0, time.UTC), got.Truncate(time.Second))
	assert.Equal(t, time.UTC, got.Location())

	for _, bad := range []string{"", "abc", "-1", "12.5", "99999999999999999999999"} {
		_, err := SnowflakeTime(bad)
		assert.Error(t, err, "input %q", bad)
	}
}

func TestAttemptOutcome(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		err    error
		want   string
	}{
		{"ok", 200, `{"data":{}}`, nil, outcomeOK},
		{"created", 201, ``, nil, outcomeOK},
		{"429", 429, `{}`, nil, outcomeRateLimited},
		{"403 bare", 403, `{}`, nil, outcomeForbidden},
		{"401 bare", 401, `{}`, nil, outcomeAuth},
		{"404", 404, `{}`, nil, outcomeNotFound},
		{"500", 500, `oops`, nil, outcomeOther},
		{"code 32", 401, `{"errors":[{"code":32}]}`, nil, outcomeAuth},
		{"code 353", 403, `{"errors":[{"code":353}]}`, nil, outcomeCSRF},
		{"code 88 in a 200", 200, `{"errors":[{"code":88}]}`, nil, outcomeBanned},
		{"code 64", 200, `{"errors":[{"code":64}]}`, nil, outcomeSuspended},
		{"code 326", 200, `{"errors":[{"code":326}]}`, nil, outcomeLocked},
		{"code 161", 200, `{"errors":[{"code":161}]}`, nil, outcomeBlocked},
		{"code 131", 200, `{"errors":[{"code":131}]}`, nil, outcomeInternal},
		{"transport", 0, ``, errors.New("EOF"), outcomeTransport},
		{"proxy", 0, ``, errors.New("Proxy responded with non 200 code: 407"), outcomeProxy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, attemptOutcome(tc.status, []byte(tc.body), nil, tc.err))
		})
	}
}

// Every errorClass must map to a specific outcome: a new class added to the
// taxonomy without a mapping lands in "other" and fails here.
func TestOutcomeForClass_CoversEveryClass(t *testing.T) {
	seen := map[string]errorClass{}
	for c := errNone; c < errClassCount; c++ {
		o := outcomeForClass(c)
		assert.NotEqual(t, outcomeOther, o, "errorClass %d has no outcome mapping", c)
		if c != errAuthExpired && c != errNotAuthorized {
			if prev, dup := seen[o]; dup {
				t.Errorf("classes %d and %d share outcome %q", prev, c, o)
			}
			seen[o] = c
		}
	}
}

func TestPoolAccountStates(t *testing.T) {
	mk := func(name string) *Account { a := &Account{Username: name}; a.active = true; return a }
	active, backoff, cooling, dead := mk("a"), mk("b"), mk("c"), mk("d")
	now := time.Now()
	backoff.proxyBackoff = now.Add(time.Minute)
	cooling.active, cooling.reactivateAt = false, now.Add(time.Minute)
	dead.active = false
	c := &Client{pool: pool.New([]*Account{active, backoff, cooling, dead}, pool.DefaultConfig())}

	got := c.poolAccountStates(now)
	assert.Equal(t, map[string]int64{
		accountStateActive: 1, accountStateProxyBackoff: 1, accountStateCoolingDown: 1, accountStateDeactivated: 1,
	}, got)
}

func TestMetrics_RelogAndProxyCounters(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	acc := &Account{Username: "p", Proxy: "http://u:p@127.0.0.1:1"}
	c := &Client{
		metrics:     newClientMetrics(mp),
		cfg:         ClientConfig{ProxyBackoffInitial: time.Second, ProxyBackoffMax: time.Minute},
		reloginGate: AutoReloginGateFunc(func(context.Context, string) (bool, string) { return false, "test" }),
	}

	c.markProxyDown(acc)
	require.Error(t, c.relogin(acc))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	sums := map[string]metricdata.Sum[int64]{}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		if s, ok := m.Data.(metricdata.Sum[int64]); ok {
			sums[m.Name] = s
		}
	}
	require.Len(t, sums[metricProxyFailures].DataPoints, 1)
	assert.Equal(t, int64(1), sums[metricProxyFailures].DataPoints[0].Value)
	require.Len(t, sums[metricRelogins].DataPoints, 1)
	assert.Equal(t, map[string]string{attrOutcome: outcomeBreakerOpen}, attrMap(sums[metricRelogins].DataPoints[0].Attributes))
}

// failingMeter errors on every histogram, to prove construction degrades to
// no-op instruments instead of panicking or failing.
type failingMeter struct{ noop.Meter }

func (failingMeter) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return nil, errors.New("boom")
}

type failingProvider struct{ noop.MeterProvider }

func (failingProvider) Meter(string, ...metric.MeterOption) metric.Meter { return failingMeter{} }

func TestNewClientMetrics_InstrumentErrorFallsBackToNoop(t *testing.T) {
	var m *clientMetrics
	require.NotPanics(t, func() { m = newClientMetrics(failingProvider{}) })
	require.NotPanics(t, func() {
		m.recordRequest("UserTweets", time.Second, outcomeOK)
		m.recordFreshness("UserTweets", []*Tweet{{ID: snowflakeAt(time.Now())}})
	})
}

func TestClientMetrics_NilReceiverIsSafe(t *testing.T) {
	var m *clientMetrics
	require.NotPanics(t, func() {
		m.recordRequest("x", time.Second, outcomeOK)
		m.recordFreshness("x", nil)
		m.recordRelogin(outcomeOK)
		m.recordProxyFailure()
	})
}
