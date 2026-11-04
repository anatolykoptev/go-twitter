package twitter

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
