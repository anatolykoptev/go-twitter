package twitter

import (
	"context"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anatolykoptev/go-twitter/internal/bundle"
	"github.com/anatolykoptev/go-twitter/internal/gqlextract"
)

// F9 — a later refresh whose bundle lacks an op (a flaky chunk) but still passes
// the known-ops floor must not send an earlier-healed op back to its retired
// compiled id.
//
// Mutation: in queryid_refresh.go run() delete the
// `carried := overlaySnapshot(prev, snap)` call (store only this walk's ids)
// => RED.
func TestQIDRefresh_F9_LaterPartialRefreshKeepsHealedOps(t *testing.T) {
	r, clk := useQIDRefresher(t)
	w := newQIDWorld(t)
	w.retire("SearchTimeline", qidNewSearch)
	c, _ := newQIDClient(t, w, 1)
	ctx := ctxTimeout(t)

	_ = c.search(ctx) // refresh 1 heals SearchTimeline
	r.wg.Wait()
	require.NoError(t, c.search(ctx))

	// Refresh 2: the bundle lacks SearchTimeline but carries 10 other known ops.
	var others []string
	for op := range Endpoints {
		if op != "SearchTimeline" {
			others = append(others, op)
		}
	}
	sort.Strings(others)
	w.set(func(w *qidWorld) {
		w.advertised = map[string]string{}
		for _, op := range others[:10] {
			w.advertised[op] = Endpoints[op].ID
		}
	})
	clk.Advance(qidRefreshTTL + time.Minute)
	require.NoError(t, c.search(ctx)) // triggers the ttl refresh
	r.wg.Wait()
	require.EqualValues(t, 2, r.attempts.Load(), "the partial refresh must have run and been accepted")

	assert.Equal(t, qidNewSearch, activeQID.Load().ids["SearchTimeline"], "the healed id is carried forward")
	require.NoError(t, c.search(ctx), "the call after the partial refresh still uses the healed id")
}

// F10 — consecutive failures space the next allowed attempt further apart
// (minInterval, then doubling, capped), the spacing also gates the fault
// trigger, and a success resets it.
//
// Mutation: in queryid_refresh.go backoff() replace the body with
// `return r.cfg.minInterval` (no backoff) => RED.
func TestQIDRefresh_F10_FailuresBackOffExponentially(t *testing.T) {
	r, clk := useQIDRefresher(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	w := newQIDWorld(t)
	w.set(func(w *qidWorld) { w.failFetch = true })
	c, _ := newQIDClient(t, w, 1)
	c.metrics = newClientMetrics(mp)
	ctx := ctxTimeout(t)

	wantWaits := []time.Duration{qidMinInterval, 2 * qidMinInterval, 4 * qidMinInterval}
	for i, want := range wantWaits {
		require.NoError(t, c.search(ctx))
		r.wg.Wait()
		require.EqualValues(t, i+1, r.attempts.Load(), "attempt %d must run", i+1)
		gap := time.Unix(0, r.nextAllowedNS.Load()).Sub(clk.Now())
		assert.Equal(t, want, gap, "after failure %d the next attempt is allowed %s later", i+1, want)

		// One minute short of the wait: still refused.
		clk.Advance(want - time.Minute)
		require.NoError(t, c.search(ctx))
		r.wg.Wait()
		require.EqualValues(t, i+1, r.attempts.Load(), "inside the backoff no attempt may run")
		clk.Advance(2 * time.Minute)
	}

	// A fault storm cannot defeat the backoff either.
	for i := 0; i < 3*qidFaultThreshold; i++ {
		r.onEndpointFault("SearchTimeline", func() bundle.Fetcher { return nil }, c.metrics)
	}
	require.EqualValues(t, len(wantWaits), r.attempts.Load())

	// Cap.
	assert.Equal(t, qidMaxBackoff, r.backoff(40))

	// A success resets the spacing.
	w.set(func(w *qidWorld) { w.failFetch = false })
	clk.Advance(qidMaxBackoff)
	require.NoError(t, c.search(ctx))
	r.wg.Wait()
	gap := time.Unix(0, r.nextAllowedNS.Load()).Sub(clk.Now())
	assert.Equal(t, qidMinInterval, gap, "success resets the spacing to the minimum interval")

	// The suppressed attempts were counted.
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var suppressed int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == metricQIDRefreshes {
				for _, p := range sum.DataPoints {
					if attrMap(p.Attributes)[attrResult] == qidResultSuppressed {
						suppressed += p.Value
					}
				}
			}
		}
	}
	assert.Positive(t, suppressed, "a due attempt refused by the spacing is counted as suppressed")
}

// A fault threshold hit that the spacing refuses must keep the op's fault
// history, so the trigger fires as soon as it is allowed.
//
// Mutation: in queryid_refresh.go onEndpointFault move `r.clearFaults(op)`
// before the `intervalOK` check => RED.
func TestQIDRefresh_SuppressedFaultTriggerKeepsHistory(t *testing.T) {
	r, clk := useQIDRefresher(t)
	w := newQIDWorld(t)
	w.set(func(w *qidWorld) { w.failFetch = true })
	f := instantFailFetcher{}
	mk := func() bundle.Fetcher { return f }

	r.nextAllowedNS.Store(clk.Now().Add(30 * time.Second).UnixNano()) // lifts inside the fault window
	for i := 0; i < qidFaultThreshold; i++ {
		r.onEndpointFault("SearchTimeline", mk, nil)
	}
	r.wg.Wait()
	require.EqualValues(t, 0, r.attempts.Load(), "the spacing refuses the trigger")

	clk.Advance(time.Minute)
	r.onEndpointFault("SearchTimeline", mk, nil) // one more fault: history was kept
	r.wg.Wait()
	assert.EqualValues(t, 1, r.attempts.Load(), "the trigger fires on the first fault after the spacing lifts")
}

// A panic while parsing a network-supplied body must not crash the process: it
// is recovered into a failed refresh ("panic") with the usual one alert, and the
// map is untouched.
//
// Mutation: in queryid_refresh.go attempt() delete the deferred recover => the
// test binary crashes (RED).
func TestQIDRefresh_PanicInExtractionIsRecovered(t *testing.T) {
	r, _ := useQIDRefresher(t)
	var alerts atomic.Int32
	var gotResult atomic.Value
	r.setAlertHook(func(_ string, payload any) {
		alerts.Add(1)
		gotResult.Store(payload.(map[string]any)["result"])
	})
	orig := qidConsume
	qidConsume = func(*gqlextract.Extractor, string, []byte) { panic("hostile bundle") }
	t.Cleanup(func() { qidConsume = orig })

	w := newQIDWorld(t)
	c, _ := newQIDClient(t, w, 1)
	require.NoError(t, c.search(ctxTimeout(t)))
	r.wg.Wait()

	assert.EqualValues(t, 1, r.attempts.Load())
	assert.Nil(t, activeQID.Load())
	assert.EqualValues(t, 1, alerts.Load())
	assert.Equal(t, qidResultPanic, gotResult.Load())
}
