package twitter

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anatolykoptev/go-twitter/internal/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// flushReports waits for the background usage reports queued so far; rig
// assertions on r.reports read it after the *WithSocial call has returned.
func flushReports(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, FlushSocialReports(ctx), "usage reports still in flight")
}

func (r *socialRig) reportList() []string {
	r.reportsMu.Lock()
	defer r.reportsMu.Unlock()
	return append([]string(nil), r.reports...)
}

// U1 — journey: a usage endpoint that takes 2 s must not delay the caller's
// result; the report still arrives later.
func TestWithSocial_SlowUsageEndpoint_DoesNotDelayResult(t *testing.T) {
	r := newSocialRig(t)
	r.reportHook = func(*http.Request) { time.Sleep(2 * time.Second) }

	start := time.Now()
	tweets, err := GetUserTweetsWithSocial(context.Background(), r.sc, "tesla", 5)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.NotEmpty(t, tweets)
	assert.Less(t, elapsed, 1500*time.Millisecond,
		"the result waited for the usage report round trip (took %v)", elapsed)

	flushReports(t)
	assert.Equal(t, []string{"success"}, r.reportList(), "the report must still arrive")
}

// U2 — a caller that cancels right after the call returned must not drop the
// report: it runs detached from the caller's ctx.
func TestWithSocial_CallerCancelAfterReturn_ReportStillLands(t *testing.T) {
	r := newSocialRig(t)
	r.reportHook = func(*http.Request) { time.Sleep(300 * time.Millisecond) }

	ctx, cancel := context.WithCancel(context.Background())
	_, err := GetUserTweetsWithSocial(ctx, r.sc, "tesla", 5)
	require.NoError(t, err)
	cancel() // the report is still pending behind the slow endpoint

	flushReports(t)
	assert.Equal(t, []string{"success"}, r.reportList(), "a cancelled caller dropped the usage report")
}

// U3 — a stuck usage endpoint plus many calls must not grow goroutines without
// bound: reports in flight stay at the cap (16) and the overflow is dropped.
func TestWithSocial_StuckUsageEndpoint_InflightReportsAreCapped(t *testing.T) {
	r := newSocialRig(t)
	release := make(chan struct{})
	var inflight, peak atomic.Int32
	var once sync.Once
	r.reportHook = func(*http.Request) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
	}
	t.Cleanup(func() { once.Do(func() { close(release) }); flushReports(t) })

	droppedBefore := socialReports.dropped.Load()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 60; i++ {
			if _, err := GetUserTweetsWithSocial(context.Background(), r.sc, "tesla", 5); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("calls blocked behind the stuck usage endpoint")
	}
	time.Sleep(200 * time.Millisecond) // let queued reports reach the endpoint

	assert.LessOrEqual(t, peak.Load(), int32(16), "usage reports in flight exceeded the cap")
	assert.Positive(t, socialReports.dropped.Load()-droppedBefore, "overflow reports must be counted as dropped")
}

// F1 — Flush must be safe while submit keeps running (shutdown with calls still
// in flight). A sync.WaitGroup Add racing a Wait from a zero counter panics
// ("WaitGroup is reused before previous Wait has returned").
func TestReportSink_ConcurrentSubmitAndFlush(t *testing.T) {
	s := newReportSink(4)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.submit(func() { time.Sleep(time.Microsecond) })
				}
			}
		}()
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.wait(ctx) // either outcome is fine; it must not panic or hang
		cancel()
	}
	close(stop)
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.wait(ctx), "sink must drain once submitters stop")
}

// Overflow drops are exported as twitter.client.usage_report.dropped.
func TestWithSocial_ReportOverflow_IncrementsDroppedCounter(t *testing.T) {
	r := newSocialRig(t)
	SetSocialMeterProvider(r.mp)
	release := make(chan struct{})
	var once sync.Once
	r.reportHook = func(*http.Request) { <-release }
	t.Cleanup(func() { once.Do(func() { close(release) }); flushReports(t) })

	for i := 0; i < 40; i++ {
		_, err := GetUserTweetsWithSocial(context.Background(), r.sc, "tesla", 5)
		require.NoError(t, err, "call %d", i)
	}
	m, ok := r.collect(t)[telemetry.MetricUsageReportsDropped]
	require.True(t, ok, "usage_report.dropped must be reported after an overflow")
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	assert.Positive(t, total, "overflowed reports must increment the counter")
}

// A flush that runs while a submit is between its two steps must wait for that
// report: it is already committed to run, and the process would exit before it
// starts if the flush returned.
//
// Mutation: social_report.go submit — move the `s.n++` registration (and the
// afterFirstStep call) below the `select { case s.sem <- ... }` slot reservation
// (the old order) => RED: flush returns nil while the report is still pending.
func TestReportSink_FlushDuringSubmit_WaitsForReport(t *testing.T) {
	s := newReportSink(4)
	reached := make(chan struct{})
	resume := make(chan struct{})
	s.afterFirstStep = func() {
		close(reached)
		<-resume
	}
	finish := make(chan struct{})
	var finished atomic.Bool
	submitted := make(chan struct{})
	go func() {
		defer close(submitted)
		s.submit(func() {
			<-finish
			finished.Store(true)
		})
	}()
	<-reached // submit is mid-way: one step done, the other not

	flushed := make(chan error, 1)
	go func() { flushed <- s.wait(context.Background()) }()
	select {
	case err := <-flushed:
		t.Fatalf("flush returned %v while a report was being submitted", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(resume)
	<-submitted
	select {
	case err := <-flushed:
		t.Fatalf("flush returned %v before the report finished", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	require.NoError(t, <-flushed)
	assert.True(t, finished.Load(), "flush returned before the report finished")
}

// A report dropped at the cap leaves nothing in flight: a flush must not hang.
func TestReportSink_DroppedAtCap_DoesNotHoldFlush(t *testing.T) {
	s := newReportSink(1)
	block := make(chan struct{})
	require.True(t, s.submit(func() { <-block }))
	require.False(t, s.submit(func() {}))
	close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, s.wait(ctx))
}
