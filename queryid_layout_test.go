package twitter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anatolykoptev/go-twitter/internal/queryid"
	"github.com/anatolykoptev/go-twitter/internal/telemetry"
)

// qidLayoutSeries collects refreshes as "trigger/result/layout" -> count.
func qidLayoutSeries(t *testing.T, reader sdkmetric.Reader) map[string]int64 {
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
				series[a[telemetry.AttrTrigger]+"/"+a[telemetry.AttrResult]+"/"+a[telemetry.AttrLayout]] = p.Value
			}
		}
	}
	return series
}

func newLayoutReader(t *testing.T) (sdkmetric.Reader, *clientMetrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return reader, newClientMetrics(mp)
}

// A refresh that succeeds reports the layout of the page it read.
//
// Mutation: internal/queryid/refresh.go Run, pass "" instead of seen.Layout at
// the success-path RecordQIDRefresh call => RED (layout reads "unknown").
func TestQIDRefresh_LayoutAttribute_OnSuccess(t *testing.T) {
	r, _ := useQIDRefresher(t)
	reader, cm := newLayoutReader(t)
	w := newQIDWorld(t)
	c, _ := newQIDClient(t, w, 1)
	c.metrics = cm

	require.NoError(t, c.search(ctxTimeout(t)))
	r.WG.Wait()

	assert.Equal(t, map[string]int64{"first/" + queryid.ResultOK + "/" + queryid.LayoutVite: 1}, qidLayoutSeries(t, reader))
}

type noNetFetcher struct{}

func (noNetFetcher) Fetch(context.Context, string) ([]byte, error) {
	return nil, errors.New("no network in this test")
}

// A refresh that reads the logged-out shell (a refused session) fails
// fetch_failed under the LAST KNOWN layout, not "vite": the shell says nothing
// about the signed-in build. A second attempt inside the spacing window is
// counted suppressed under the same layout.
//
// Mutation (failure path): refresh.go Run, pass "" at the err-path
// RecordQIDRefresh call => RED. Mutation (suppressed path): lastLayout returns
// LayoutUnknown unconditionally => RED. Mutation (shell): Build, drop the
// IsLoggedOutShell override of seen.Layout => RED (label reads "vite").
func TestQIDRefresh_LayoutAttribute_OnFailureAndSuppressed(t *testing.T) {
	r, _ := useQIDRefresher(t)
	reader, cm := newLayoutReader(t)
	shell, err := os.ReadFile(filepath.Join("internal", "queryid", "testdata", "entry-inline.html"))
	require.NoError(t, err)

	r.ObservePage([]byte(`<html><script src="https://abs.twimg.com/responsive-web/client-web/main.0123456789abcdefa.js"></script></html>`))
	r.ObservePage(shell)
	r.Run("first", noNetFetcher{}, cm.instruments())
	r.Run("first", noNetFetcher{}, cm.instruments())

	assert.Equal(t, map[string]int64{
		"first/" + queryid.ResultFetchFailed + "/" + queryid.LayoutWebpack: 1,
		"first/" + queryid.ResultSuppressed + "/" + queryid.LayoutWebpack:  1,
	}, qidLayoutSeries(t, reader))
}
