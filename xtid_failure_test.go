package twitter

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anatolykoptev/go-twitter/internal/telemetry"
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
