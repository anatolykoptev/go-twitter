package twitter

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// promotedEntry is a timeline ad: its tweet ID is minted at serve time, so it is
// always "fresh" and must not mask the staleness of the organic tweets.
func promotedEntry(id string) string {
	return fmt.Sprintf(`{"entryId":"promoted-tweet-%s","content":{"entryType":"TimelineTimelineItem","__typename":"TimelineTimelineItem","itemContent":{"__typename":"TimelineTweet","promotedMetadata":{"advertiser_results":{}},"tweet_results":{"result":{"__typename":"Tweet","rest_id":%q,"legacy":{"full_text":"ad","user_id_str":"1"}}}}}}`, id, id)
}

func TestMetrics_FreshnessIgnoresPromotedTweets(t *testing.T) {
	var f *metricsFixture
	f = newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body := `{"data":{"list":{"tweets_timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[` +
			promotedEntry(snowflakeAt(f.now.Add(-1*time.Second))) + `,` + // ad, newer than everything
			tweetEntry(snowflakeAt(f.now.Add(-40*time.Second))) +
			`]}]}}}}}`
		_, _ = w.Write([]byte(body))
	}, "ads_acc")

	tweets, err := f.c.GetListTweets(context.Background(), "L1", 20)
	require.NoError(t, err)
	require.Len(t, tweets, 2, "promoted tweets are still returned to the caller; only the metric ignores them")

	pts := histPoints(t, f.collect(t), metricFreshness)
	require.Len(t, pts, 1)
	assert.InDelta(t, 40.0, pts[0].Sum, 0.01, "freshness must be the organic tweet's age, not the ad's")
}

// The gauge callback reads account state while the pool deactivates accounts
// under its own lock. Only meaningful under -race.
func TestMetrics_PoolGaugeRacesWithDeactivation(t *testing.T) {
	accs := make([]*Account, 4)
	for i := range accs {
		accs[i] = &Account{Username: fmt.Sprintf("race%d", i)}
		accs[i].active = true
	}
	p := pool.New(accs, pool.DefaultConfig())
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	c := &Client{pool: p}
	c.registerPoolGauge(mp)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			a := accs[i%len(accs)]
			if i%2 == 0 {
				p.SoftDeactivate(a, time.Minute)
			} else {
				p.DeactivateItem(a)
			}
		}
	}()
	for i := 0; i < 200; i++ {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
	}
	close(stop)
	<-done
}

// Close unregisters the gauge callback; it is idempotent and nil-safe.
func TestClient_CloseUnregistersPoolGauge(t *testing.T) {
	f := newMetricsFixture(t, func(http.ResponseWriter, *http.Request) {}, "close_acc")
	_, ok := f.collect(t)[metricPoolAccounts]
	require.True(t, ok, "gauge present before Close")

	require.NoError(t, f.c.Close())
	require.NoError(t, f.c.Close(), "Close must be idempotent")
	_, ok = f.collect(t)[metricPoolAccounts]
	assert.False(t, ok, "gauge must stop reporting after Close")

	var nilClient *Client
	assert.NoError(t, nilClient.Close())
}

// NewClient itself wires the instruments and the gauge. The default proxy points
// at a dead loopback port so the xtid/warm-page bootstrap fails fast without
// touching the real x.com; the request client is then swapped for one that
// reaches the fake server.
func TestNewClient_WiresMetrics(t *testing.T) {
	bc := startFakeX(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now().Add(-3 * time.Second)))))
	})
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	acc := &Account{Username: "wired_acc", AuthToken: "auth", CT0: "ct0", UserAgent: defaultUserAgent}
	c, err := NewClient(ClientConfig{
		Accounts:             []*Account{acc},
		DefaultProxy:         "http://127.0.0.1:1",
		SessionDir:           t.TempDir(),
		DisableGuestFallback: true,
		MeterProvider:        mp,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.client = bc
	c.xtidMgr = xtid.NewManager(failFetcher{})

	_, err = c.GetUserTweets(context.Background(), "999", 20)
	require.NoError(t, err)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	got := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = m
		}
	}
	assert.Len(t, histPoints(t, got, metricRequestDuration), 1)
	assert.Len(t, histPoints(t, got, metricFreshness), 1)
	_, ok := got[metricPoolAccounts]
	assert.True(t, ok, "NewClient must register the pool.accounts gauge")
}
