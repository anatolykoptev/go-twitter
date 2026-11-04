// Package telemetry holds the OpenTelemetry instruments, their names and label
// values for go-twitter. The metric names, units, descriptions, bucket
// boundaries and label sets here are the library's public monitoring contract:
// see docs/metrics.md.
package telemetry

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// The library depends only on the OpenTelemetry metric API; the application
// installs the SDK/exporter of its choice.

const (
	// MeterName is the instrumentation scope name (the module path).
	MeterName = "github.com/anatolykoptev/go-twitter"

	// Instrument names.
	MetricRequestDuration     = "twitter.client.request.duration"
	MetricFreshness           = "twitter.client.timeline.freshness"
	MetricPoolAccounts        = "twitter.client.pool.accounts"
	MetricPoolInflight        = "twitter.client.pool.inflight"
	MetricPoolWait            = "twitter.client.pool.wait.duration"
	MetricRelogins            = "twitter.client.relogins"
	MetricProxyFailures       = "twitter.client.proxy.failures"
	MetricProxyClientFailures = "twitter.client.proxy.client_failures"
	MetricRateLimitRemain     = "twitter.client.ratelimit.remaining"
	MetricRateLimitLimit      = "twitter.client.ratelimit.limit"
	MetricRateLimitHolds      = "twitter.client.ratelimit.holds"
	MetricQIDRefreshes        = "twitter.client.queryid.refreshes"
	MetricQIDChanged          = "twitter.client.queryid.changed"
	MetricQIDAge              = "twitter.client.queryid.age"
	MetricQIDWalk             = "twitter.client.queryid.refresh.duration"
	MetricXtidFailures        = "twitter.client.xtid.failures"
	MetricUsageReportsDropped = "twitter.client.usage_report.dropped"
	MetricParseDropped        = "twitter.client.parse.dropped"

	// Attribute keys.
	AttrOperation    = "twitter.operation"
	AttrOutcome      = "twitter.outcome"
	AttrAccountState = "twitter.account.state"
	AttrResult       = "twitter.result"
	AttrTrigger      = "twitter.trigger"
	AttrReason       = "twitter.reason"
)

// Request/relogin outcome values (twitter.outcome). Bounded: request outcomes
// are derived from the typed error classification (errorClass) by
// the root package's outcomeForClass, never hand-listed per call site.
const (
	OutcomeOK          = "ok"
	OutcomeRateLimited = "rate_limited"
	OutcomeAuth        = "auth"
	OutcomeCSRF        = "csrf"
	OutcomeBanned      = "banned"
	OutcomeSuspended   = "suspended"
	OutcomeLocked      = "locked"
	OutcomeBlocked     = "blocked"
	OutcomeForbidden   = "forbidden"
	OutcomeInternal    = "internal"
	OutcomeNotFound    = "not_found"
	OutcomeEndpoint    = "endpoint"
	OutcomeOverloaded  = "overloaded"
	OutcomeBadRequest  = "bad_request"
	OutcomeUnexpected  = "unexpected"
	OutcomeTransport   = "transport"
	OutcomeProxy       = "proxy"
	OutcomeOther       = "other"

	// Relogin-only outcomes.
	OutcomeFailed      = "failed"
	OutcomeBreakerOpen = "breaker_open"
)

// Pool-wait results (twitter.result) reported by
// twitter.client.pool.wait.duration. Exhaustive over the ways a waited
// nextAccountWithWait call can end.
const (
	WaitResultAcquired = "acquired" // the wait ended with a usable account
	WaitResultGaveUp   = "gave_up"  // waited, then no account could serve the endpoint within budget
	WaitResultCtxDone  = "ctx_done" // the caller's context ended the wait
)

// Account states reported by twitter.client.pool.accounts. Mutually exclusive
// and exhaustive: every pool account is in exactly one.
const (
	AccountStateActive       = "active"
	AccountStateProxyBackoff = "proxy_backoff" // active, but its proxy is in exponential backoff
	AccountStateCoolingDown  = "cooling_down"  // soft-deactivated; the pool will auto-reactivate it
	AccountStateDeactivated  = "deactivated"   // permanently deactivated (suspended, never reactivated)
)

var AccountStates = []string{
	AccountStateActive, AccountStateProxyBackoff, AccountStateCoolingDown, AccountStateDeactivated,
}

var (
	// RequestDurationBuckets (seconds) resolve a 50 ms - 30 s HTTP call.
	RequestDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	// FreshnessBuckets (seconds) resolve 0.5 s - 60 s well, with a tail to 1 h.
	FreshnessBuckets = []float64{0.5, 1, 2, 3, 5, 8, 12, 20, 30, 45, 60, 120, 300, 900, 3600}
	// PoolWaitBuckets (seconds) resolve a 50 ms - 5 min pool wait (poolWaitCap).
	PoolWaitBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	// rateLimitBuckets (requests) resolve X's per-endpoint windows, which run
	// from tens to ~500 requests, with fine steps near exhaustion.
	rateLimitBuckets = []float64{0, 1, 2, 5, 10, 25, 50, 100, 150, 200, 300, 500}
)

// Metrics holds the instruments. A nil *Metrics is valid and records nothing.
type Metrics struct {
	requestDuration metric.Float64Histogram
	freshness       metric.Float64Histogram
	poolWait        metric.Float64Histogram
	relogins        metric.Int64Counter
	proxyFailures   metric.Int64Counter
	proxyClientFail metric.Int64Counter
	rlRemaining     metric.Int64Histogram
	rlLimit         metric.Int64Histogram
	rlHolds         metric.Int64Counter
	qidRefreshes    metric.Int64Counter
	qidChanged      metric.Int64Gauge
	qidAge          metric.Float64Gauge
	qidWalk         metric.Float64Histogram
	xtidFailures    metric.Int64Counter
	reportsDropped  metric.Int64Counter
	parseDropped    metric.Int64Counter
}

// instrumentationVersion reports this module's version as seen by the
// consuming binary (empty when built from the module itself).
func instrumentationVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if bi.Main.Path == MeterName {
		return bi.Main.Version
	}
	for _, d := range bi.Deps {
		if d.Path == MeterName {
			return d.Version
		}
	}
	return ""
}

// New builds the instruments on mp (nil = otel global provider).
// An instrument that fails to create degrades to a no-op and is logged once;
// construction never fails and never panics.
func New(mp metric.MeterProvider) *Metrics {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(MeterName, metric.WithInstrumentationVersion(instrumentationVersion()))

	var once sync.Once
	fail := func(name string, err error) {
		once.Do(func() {
			slog.Warn("twitter: metric instrument creation failed, falling back to no-op",
				slog.String("instrument", name), slog.Any("error", err))
		})
	}

	m := &Metrics{}
	var err error
	if m.requestDuration, err = meter.Float64Histogram(MetricRequestDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one HTTP request attempt to X, by operation and outcome."),
		metric.WithExplicitBucketBoundaries(RequestDurationBuckets...)); err != nil {
		fail(MetricRequestDuration, err)
		m.requestDuration = noop.Float64Histogram{}
	}
	if m.freshness, err = meter.Float64Histogram(MetricFreshness,
		metric.WithUnit("s"),
		metric.WithDescription("Age of the newest tweet (by snowflake ID) in a successful timeline response."),
		metric.WithExplicitBucketBoundaries(FreshnessBuckets...)); err != nil {
		fail(MetricFreshness, err)
		m.freshness = noop.Float64Histogram{}
	}
	if m.poolWait, err = meter.Float64Histogram(MetricPoolWait,
		metric.WithUnit("s"),
		metric.WithDescription("Time a request spent waiting for a usable pool account, by operation and wait result."),
		metric.WithExplicitBucketBoundaries(PoolWaitBuckets...)); err != nil {
		fail(MetricPoolWait, err)
		m.poolWait = noop.Float64Histogram{}
	}
	if m.relogins, err = meter.Int64Counter(MetricRelogins,
		metric.WithUnit("{relogin}"),
		metric.WithDescription("Automatic relogin attempts, by outcome.")); err != nil {
		fail(MetricRelogins, err)
		m.relogins = noop.Int64Counter{}
	}
	if m.proxyFailures, err = meter.Int64Counter(MetricProxyFailures,
		metric.WithUnit("{failure}"),
		metric.WithDescription("Proxy-layer failures that put an account's proxy into backoff.")); err != nil {
		fail(MetricProxyFailures, err)
		m.proxyFailures = noop.Int64Counter{}
	}
	if m.proxyClientFail, err = meter.Int64Counter(MetricProxyClientFailures,
		metric.WithUnit("{failure}"),
		metric.WithDescription("Accounts disabled because their per-account proxy client could not be built.")); err != nil {
		fail(MetricProxyClientFailures, err)
		m.proxyClientFail = noop.Int64Counter{}
	}
	if m.rlRemaining, err = meter.Int64Histogram(MetricRateLimitRemain,
		metric.WithUnit("{request}"),
		metric.WithDescription("Requests X reports as remaining in the rate-limit window (x-rate-limit-remaining), by operation."),
		metric.WithExplicitBucketBoundaries(rateLimitBuckets...)); err != nil {
		fail(MetricRateLimitRemain, err)
		m.rlRemaining = noop.Int64Histogram{}
	}
	if m.rlLimit, err = meter.Int64Histogram(MetricRateLimitLimit,
		metric.WithUnit("{request}"),
		metric.WithDescription("Per-window request ceiling X reports (x-rate-limit-limit), by operation."),
		metric.WithExplicitBucketBoundaries(rateLimitBuckets...)); err != nil {
		fail(MetricRateLimitLimit, err)
		m.rlLimit = noop.Int64Histogram{}
	}
	if m.rlHolds, err = meter.Int64Counter(MetricRateLimitHolds,
		metric.WithUnit("{hold}"),
		metric.WithDescription("Times an endpoint was held on an account until its window reset because x-rate-limit-remaining showed the window spent, by operation.")); err != nil {
		fail(MetricRateLimitHolds, err)
		m.rlHolds = noop.Int64Counter{}
	}
	if m.qidRefreshes, err = meter.Int64Counter(MetricQIDRefreshes,
		metric.WithUnit("{refresh}"),
		metric.WithDescription("Runtime queryId/feature refresh attempts, by trigger and result.")); err != nil {
		fail(MetricQIDRefreshes, err)
		m.qidRefreshes = noop.Int64Counter{}
	}
	if m.qidChanged, err = meter.Int64Gauge(MetricQIDChanged,
		metric.WithUnit("{id}"),
		metric.WithDescription("Operation queryIds that differed from the compiled set on the last successful refresh.")); err != nil {
		fail(MetricQIDChanged, err)
		m.qidChanged = noop.Int64Gauge{}
	}
	if m.qidAge, err = meter.Float64Gauge(MetricQIDAge,
		metric.WithUnit("s"),
		metric.WithDescription("Age of the active queryId map: since the last successful refresh, or process start while the compiled ids are active.")); err != nil {
		fail(MetricQIDAge, err)
		m.qidAge = noop.Float64Gauge{}
	}
	if m.qidWalk, err = meter.Float64Histogram(MetricQIDWalk,
		metric.WithUnit("s"),
		metric.WithDescription("Wall time of one queryId refresh attempt (page + chunk walk), by trigger and result."),
		metric.WithExplicitBucketBoundaries(qidWalkBuckets...)); err != nil {
		fail(MetricQIDWalk, err)
		m.qidWalk = noop.Float64Histogram{}
	}
	if m.xtidFailures, err = meter.Int64Counter(MetricXtidFailures,
		metric.WithUnit("{failure}"),
		metric.WithDescription("Requests sent without x-client-transaction-id because generating it failed.")); err != nil {
		fail(MetricXtidFailures, err)
		m.xtidFailures = noop.Int64Counter{}
	}
	if m.reportsDropped, err = meter.Int64Counter(MetricUsageReportsDropped,
		metric.WithUnit("{report}"),
		metric.WithDescription("Credential-service usage reports dropped because too many were already in flight.")); err != nil {
		fail(MetricUsageReportsDropped, err)
		m.reportsDropped = noop.Int64Counter{}
	}
	if m.parseDropped, err = meter.Int64Counter(MetricParseDropped,
		metric.WithUnit("{entry}"),
		metric.WithDescription("Timeline entries the parser dropped instead of returning as tweets, by operation and reason.")); err != nil {
		fail(MetricParseDropped, err)
		m.parseDropped = noop.Int64Counter{}
	}
	return m
}

// RegisterPoolGauge wires the async twitter.client.pool.accounts and
// twitter.client.pool.inflight gauges to the caller's pool. states returns the
// account count per AccountStates value, inFlight the requests in flight across
// the pool. Failure is logged and leaves the gauges absent; the returned
// registration is then nil.
func RegisterPoolGauge(mp metric.MeterProvider, states func(now time.Time) map[string]int64, inFlight func() int64) metric.Registration {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(MeterName, metric.WithInstrumentationVersion(instrumentationVersion()))
	g, err := meter.Int64ObservableGauge(MetricPoolAccounts,
		metric.WithUnit("{account}"),
		metric.WithDescription("Pool accounts by state."))
	if err != nil {
		slog.Warn("twitter: metric instrument creation failed, falling back to no-op",
			slog.String("instrument", MetricPoolAccounts), slog.Any("error", err))
		return nil
	}
	ifg, err := meter.Int64ObservableGauge(MetricPoolInflight,
		metric.WithUnit("{request}"),
		metric.WithDescription("Requests currently in flight across pool accounts (per-account cap: ClientConfig.MaxInFlightPerAccount)."))
	if err != nil {
		slog.Warn("twitter: metric instrument creation failed, falling back to no-op",
			slog.String("instrument", MetricPoolInflight), slog.Any("error", err))
		return nil
	}
	reg, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		counts := states(time.Now())
		for _, st := range AccountStates {
			o.ObserveInt64(g, counts[st], metric.WithAttributes(attribute.String(AttrAccountState, st)))
		}
		o.ObserveInt64(ifg, inFlight())
		return nil
	}, g, ifg)
	if err != nil {
		slog.Warn("twitter: metric callback registration failed",
			slog.String("instrument", MetricPoolAccounts), slog.Any("error", err))
		return nil
	}
	return reg
}

// RecordRequest records one HTTP attempt.
func (m *Metrics) RecordRequest(endpoint string, d time.Duration, outcome string) {
	if m == nil {
		return
	}
	m.requestDuration.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String(AttrOperation, endpoint),
		attribute.String(AttrOutcome, outcome),
	))
}

// RecordFreshness records the age, in seconds, of the newest tweet in a
// successful timeline response. The caller derives the age; see
// docs/metrics.md for the semantics.
func (m *Metrics) RecordFreshness(endpoint string, ageSeconds float64) {
	if m == nil {
		return
	}
	m.freshness.Record(context.Background(), ageSeconds, metric.WithAttributes(attribute.String(AttrOperation, endpoint)))
}

// RecordPoolWait records the total time one call spent inside
// nextAccountWithWait actually waiting (sleeping for a usable account), by
// operation and wait result. Calls that never waited record nothing.
func (m *Metrics) RecordPoolWait(endpoint string, d time.Duration, result string) {
	if m == nil {
		return
	}
	m.poolWait.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String(AttrOperation, endpoint),
		attribute.String(AttrResult, result),
	))
}

// RecordRateLimit records what one response said about its endpoint's window:
// remaining and limit, each only when the response carried a parseable header
// (remaining >= 0, limit > 0).
func (m *Metrics) RecordRateLimit(endpoint string, remaining, limit int) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String(AttrOperation, endpoint))
	if remaining >= 0 {
		m.rlRemaining.Record(context.Background(), int64(remaining), attrs)
	}
	if limit > 0 {
		m.rlLimit.Record(context.Background(), int64(limit), attrs)
	}
}

// RecordHold counts one proactive hold: an endpoint taken out of rotation on an
// account because a response showed its window spent.
func (m *Metrics) RecordHold(endpoint string) {
	if m == nil {
		return
	}
	m.rlHolds.Add(context.Background(), 1, metric.WithAttributes(attribute.String(AttrOperation, endpoint)))
}

// RecordQIDRefresh counts one refresh attempt (see docs/metrics.md).
func (m *Metrics) RecordQIDRefresh(trigger, result string) {
	if m == nil {
		return
	}
	m.qidRefreshes.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String(AttrTrigger, trigger),
		attribute.String(AttrResult, result),
	))
}

// qidWalkBuckets (seconds) resolve a refresh from 1 s to the 3-minute timeout.
var qidWalkBuckets = []float64{1, 2, 5, 10, 20, 30, 60, 90, 120, 180}

// RecordQIDRefreshDuration records how long one refresh attempt took.
func (m *Metrics) RecordQIDRefreshDuration(trigger, result string, d time.Duration) {
	if m == nil {
		return
	}
	m.qidWalk.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String(AttrTrigger, trigger),
		attribute.String(AttrResult, result),
	))
}

// RecordQIDChanged records how many ids the last successful refresh changed
// against the compiled set.
func (m *Metrics) RecordQIDChanged(n int) {
	if m == nil {
		return
	}
	m.qidChanged.Record(context.Background(), int64(n))
}

// RecordQIDAge samples the age of the active queryId map (seconds).
func (m *Metrics) RecordQIDAge(seconds float64) {
	if m == nil {
		return
	}
	m.qidAge.Record(context.Background(), seconds)
}

func (m *Metrics) RecordRelogin(outcome string) {
	if m == nil {
		return
	}
	m.relogins.Add(context.Background(), 1, metric.WithAttributes(attribute.String(AttrOutcome, outcome)))
}

func (m *Metrics) RecordProxyClientFailure() {
	if m == nil {
		return
	}
	m.proxyClientFail.Add(context.Background(), 1)
}

// RecordXtidFailure counts one request sent without x-client-transaction-id.
func (m *Metrics) RecordXtidFailure() {
	if m == nil {
		return
	}
	m.xtidFailures.Add(context.Background(), 1)
}

// RecordUsageReportDropped counts one *WithSocial usage report dropped at the
// in-flight cap.
func (m *Metrics) RecordUsageReportDropped() {
	if m == nil {
		return
	}
	m.reportsDropped.Add(context.Background(), 1)
}

func (m *Metrics) RecordProxyFailure() {
	if m == nil {
		return
	}
	m.proxyFailures.Add(context.Background(), 1)
}

// RecordParseDrop counts one timeline entry the parser dropped. reason is one
// of the fixed parse.Drop* values, so the label stays low-cardinality.
func (m *Metrics) RecordParseDrop(endpoint, reason string) {
	if m == nil {
		return
	}
	m.parseDropped.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String(AttrOperation, endpoint),
		attribute.String(AttrReason, reason),
	))
}
