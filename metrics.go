package twitter

import (
	"context"
	"log/slog"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Metrics: see docs/metrics.md. The library depends only on the OpenTelemetry
// metric API; the application installs the SDK/exporter of its choice.

const (
	// meterName is the instrumentation scope name (the module path).
	meterName = "github.com/anatolykoptev/go-twitter"

	// Instrument names.
	metricRequestDuration = "twitter.client.request.duration"
	metricFreshness       = "twitter.client.timeline.freshness"
	metricPoolAccounts    = "twitter.client.pool.accounts"
	metricPoolWait        = "twitter.client.pool.wait.duration"
	metricRelogins        = "twitter.client.relogins"
	metricProxyFailures   = "twitter.client.proxy.failures"

	// Attribute keys.
	attrOperation    = "twitter.operation"
	attrOutcome      = "twitter.outcome"
	attrAccountState = "twitter.account.state"
	attrResult       = "twitter.result"
)

// Request/relogin outcome values (twitter.outcome). Bounded: request outcomes
// are derived from the typed error classification (errorClass) by
// outcomeForClass, never hand-listed per call site.
const (
	outcomeOK          = "ok"
	outcomeRateLimited = "rate_limited"
	outcomeAuth        = "auth"
	outcomeCSRF        = "csrf"
	outcomeBanned      = "banned"
	outcomeSuspended   = "suspended"
	outcomeLocked      = "locked"
	outcomeBlocked     = "blocked"
	outcomeForbidden   = "forbidden"
	outcomeInternal    = "internal"
	outcomeNotFound    = "not_found"
	outcomeEndpoint    = "endpoint"
	outcomeOverloaded  = "overloaded"
	outcomeTransport   = "transport"
	outcomeProxy       = "proxy"
	outcomeOther       = "other"

	// Relogin-only outcomes.
	outcomeFailed      = "failed"
	outcomeBreakerOpen = "breaker_open"
)

// Pool-wait results (twitter.result) reported by
// twitter.client.pool.wait.duration. Exhaustive over the ways a waited
// nextAccountWithWait call can end.
const (
	waitResultAcquired = "acquired" // the wait ended with a usable account
	waitResultGaveUp   = "gave_up"  // waited, then no account could serve the endpoint within budget
	waitResultCtxDone  = "ctx_done" // the caller's context ended the wait
)

// Account states reported by twitter.client.pool.accounts. Mutually exclusive
// and exhaustive: every pool account is in exactly one.
const (
	accountStateActive       = "active"
	accountStateProxyBackoff = "proxy_backoff" // active, but its proxy is in exponential backoff
	accountStateCoolingDown  = "cooling_down"  // soft-deactivated; the pool will auto-reactivate it
	accountStateDeactivated  = "deactivated"   // permanently deactivated (suspended, never reactivated)
)

var accountStates = []string{
	accountStateActive, accountStateProxyBackoff, accountStateCoolingDown, accountStateDeactivated,
}

var (
	// requestDurationBuckets (seconds) resolve a 50 ms - 30 s HTTP call.
	requestDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	// freshnessBuckets (seconds) resolve 0.5 s - 60 s well, with a tail to 1 h.
	freshnessBuckets = []float64{0.5, 1, 2, 3, 5, 8, 12, 20, 30, 45, 60, 120, 300, 900, 3600}
	// poolWaitBuckets (seconds) resolve a 50 ms - 5 min pool wait (poolWaitCap).
	poolWaitBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
)

// clientMetrics holds the instruments. A nil *clientMetrics is valid and
// records nothing, so a bare &Client{} (tests, embedding) never panics.
type clientMetrics struct {
	requestDuration metric.Float64Histogram
	freshness       metric.Float64Histogram
	poolWait        metric.Float64Histogram
	relogins        metric.Int64Counter
	proxyFailures   metric.Int64Counter
	now             func() time.Time // injectable clock for freshness
}

// instrumentationVersion reports this module's version as seen by the
// consuming binary (empty when built from the module itself).
func instrumentationVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if bi.Main.Path == meterName {
		return bi.Main.Version
	}
	for _, d := range bi.Deps {
		if d.Path == meterName {
			return d.Version
		}
	}
	return ""
}

// newClientMetrics builds the instruments on mp (nil = otel global provider).
// An instrument that fails to create degrades to a no-op and is logged once;
// construction never fails and never panics.
func newClientMetrics(mp metric.MeterProvider) *clientMetrics {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(meterName, metric.WithInstrumentationVersion(instrumentationVersion()))

	var once sync.Once
	fail := func(name string, err error) {
		once.Do(func() {
			slog.Warn("twitter: metric instrument creation failed, falling back to no-op",
				slog.String("instrument", name), slog.Any("error", err))
		})
	}

	m := &clientMetrics{now: time.Now}
	var err error
	if m.requestDuration, err = meter.Float64Histogram(metricRequestDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one HTTP request attempt to X, by operation and outcome."),
		metric.WithExplicitBucketBoundaries(requestDurationBuckets...)); err != nil {
		fail(metricRequestDuration, err)
		m.requestDuration = noop.Float64Histogram{}
	}
	if m.freshness, err = meter.Float64Histogram(metricFreshness,
		metric.WithUnit("s"),
		metric.WithDescription("Age of the newest tweet (by snowflake ID) in a successful timeline response."),
		metric.WithExplicitBucketBoundaries(freshnessBuckets...)); err != nil {
		fail(metricFreshness, err)
		m.freshness = noop.Float64Histogram{}
	}
	if m.poolWait, err = meter.Float64Histogram(metricPoolWait,
		metric.WithUnit("s"),
		metric.WithDescription("Time a request spent waiting for a usable pool account, by operation and wait result."),
		metric.WithExplicitBucketBoundaries(poolWaitBuckets...)); err != nil {
		fail(metricPoolWait, err)
		m.poolWait = noop.Float64Histogram{}
	}
	if m.relogins, err = meter.Int64Counter(metricRelogins,
		metric.WithUnit("{relogin}"),
		metric.WithDescription("Automatic relogin attempts, by outcome.")); err != nil {
		fail(metricRelogins, err)
		m.relogins = noop.Int64Counter{}
	}
	if m.proxyFailures, err = meter.Int64Counter(metricProxyFailures,
		metric.WithUnit("{failure}"),
		metric.WithDescription("Proxy-layer failures that put an account's proxy into backoff.")); err != nil {
		fail(metricProxyFailures, err)
		m.proxyFailures = noop.Int64Counter{}
	}
	return m
}

// registerPoolGauge wires the async twitter.client.pool.accounts gauge to the
// client's pool. Failure is logged and leaves the gauge absent.
func (c *Client) registerPoolGauge(mp metric.MeterProvider) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(meterName, metric.WithInstrumentationVersion(instrumentationVersion()))
	g, err := meter.Int64ObservableGauge(metricPoolAccounts,
		metric.WithUnit("{account}"),
		metric.WithDescription("Pool accounts by state."))
	if err != nil {
		slog.Warn("twitter: metric instrument creation failed, falling back to no-op",
			slog.String("instrument", metricPoolAccounts), slog.Any("error", err))
		return
	}
	reg, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		counts := c.poolAccountStates(time.Now())
		for _, st := range accountStates {
			o.ObserveInt64(g, counts[st], metric.WithAttributes(attribute.String(attrAccountState, st)))
		}
		return nil
	}, g)
	if err != nil {
		slog.Warn("twitter: metric callback registration failed",
			slog.String("instrument", metricPoolAccounts), slog.Any("error", err))
		return
	}
	c.mu.Lock()
	c.poolGaugeReg = reg
	c.mu.Unlock()
}

// Close unregisters the client's pool.accounts gauge callback so the client can
// be garbage collected and a replacement client can report on the same
// MeterProvider. It is idempotent and safe on a nil client; it does not stop
// in-flight requests.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	reg := c.poolGaugeReg
	c.poolGaugeReg = nil
	c.mu.Unlock()
	if reg == nil {
		return nil
	}
	return reg.Unregister()
}

// poolAccountStates buckets every pool account into exactly one state.
func (c *Client) poolAccountStates(now time.Time) map[string]int64 {
	counts := make(map[string]int64, len(accountStates))
	if c == nil || c.pool == nil {
		return counts
	}
	for _, acc := range c.pool.Items() {
		counts[accountState(acc, now)]++
	}
	return counts
}

func accountState(acc *Account, now time.Time) string {
	// One locked snapshot: reading the fields separately could pair a stale
	// active flag with a fresh reactivateAt while the pool deactivates it.
	acc.mu.Lock()
	active, reactivateAt, backoff := acc.active, acc.reactivateAt, acc.proxyBackoff
	acc.mu.Unlock()
	if !active {
		if reactivateAt.IsZero() {
			return accountStateDeactivated
		}
		return accountStateCoolingDown
	}
	if backoff.After(now) {
		return accountStateProxyBackoff
	}
	return accountStateActive
}

// recordRequest records one HTTP attempt.
func (m *clientMetrics) recordRequest(endpoint string, d time.Duration, outcome string) {
	if m == nil {
		return
	}
	m.requestDuration.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String(attrOperation, endpoint),
		attribute.String(attrOutcome, outcome),
	))
}

// recordFreshness records the age of the newest tweet in a successful timeline
// response. See docs/metrics.md for the semantics.
func (m *clientMetrics) recordFreshness(endpoint string, tweets []*Tweet) {
	if m == nil {
		return
	}
	var newest time.Time
	for _, t := range tweets {
		if t == nil || t.promoted {
			continue
		}
		ts, err := SnowflakeTime(t.ID)
		if err != nil {
			continue
		}
		if ts.After(newest) {
			newest = ts
		}
	}
	if newest.IsZero() {
		return
	}
	age := m.now().Sub(newest).Seconds()
	if age < 0 {
		age = 0 // local clock behind X's: clamp rather than record a negative age
	}
	m.freshness.Record(context.Background(), age, metric.WithAttributes(attribute.String(attrOperation, endpoint)))
}

// recordPoolWait records the total time one call spent inside
// nextAccountWithWait actually waiting (sleeping for a usable account), by
// operation and wait result. Calls that never waited record nothing.
func (m *clientMetrics) recordPoolWait(endpoint string, d time.Duration, result string) {
	if m == nil {
		return
	}
	m.poolWait.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String(attrOperation, endpoint),
		attribute.String(attrResult, result),
	))
}

func (m *clientMetrics) recordRelogin(outcome string) {
	if m == nil {
		return
	}
	m.relogins.Add(context.Background(), 1, metric.WithAttributes(attribute.String(attrOutcome, outcome)))
}

func (m *clientMetrics) recordProxyFailure() {
	if m == nil {
		return
	}
	m.proxyFailures.Add(context.Background(), 1)
}

// twitterEpochMs is the Twitter snowflake epoch (2010-11-04T01:42:54.657Z).
const twitterEpochMs = 1288834974657

// SnowflakeTime returns the creation time encoded in a Twitter/X snowflake ID
// (a tweet ID): (id >> 22) + 1288834974657 ms. Consumers use it to compute
// their own publication-to-first-seen lag.
func SnowflakeTime(id string) (time.Time, error) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return time.Time{}, &snowflakeError{id: id, err: err}
	}
	return time.UnixMilli(int64(n>>22) + twitterEpochMs).UTC(), nil
}

type snowflakeError struct {
	id  string
	err error
}

func (e *snowflakeError) Error() string {
	return "twitter: invalid snowflake id " + strconv.Quote(e.id) + ": " + e.err.Error()
}

func (e *snowflakeError) Unwrap() error { return e.err }

// attemptOutcome classifies (proxied: the attempt went through a per-account
// proxy; only then can a transport failure be attributed to the proxy, the same
// gate attributeTransportError uses for proxy.failures) one HTTP attempt into a bounded twitter.outcome
// value using the same classifier the retry loop uses (classifyError via
// newAPIError), so the enum cannot drift from the real error taxonomy.
func attemptOutcome(status int, body []byte, hdrs map[string]string, err error, proxied bool) string {
	if err != nil {
		if proxied && isProxyError(err) {
			return outcomeProxy
		}
		return outcomeTransport
	}
	ae := newAPIError(status, body, hdrs)
	// A 404 keeps its historical not_found label: dashboards and alerts keyed
	// on twitter.outcome=not_found track the raw 404 rate regardless of the
	// errEndpoint classification the retry loop now gives it. GraphQL-shaped
	// 400s — the other endpoint-fault source — take the new "endpoint" value.
	if ae.Class == errEndpoint && status == 404 {
		return outcomeNotFound
	}
	if ae.Class != errNone {
		return outcomeForClass(ae.Class)
	}
	switch {
	case status >= 200 && status < 300:
		return outcomeOK
	case status == 401:
		return outcomeAuth
	}
	return outcomeOther
}

// outcomeForClass maps the typed error classification to an outcome value.
// errClassCount (errors.go) plus TestOutcomeForClass_CoversEveryClass make a
// new class without a mapping fail CI.
func outcomeForClass(c errorClass) string {
	switch c {
	case errNone:
		return outcomeOK
	case errBanned:
		return outcomeBanned
	case errRateLimited:
		return outcomeRateLimited
	case errSuspended:
		return outcomeSuspended
	case errLocked:
		return outcomeLocked
	case errCSRF:
		return outcomeCSRF
	case errAuthExpired, errNotAuthorized:
		return outcomeAuth
	case errBlocked:
		return outcomeBlocked
	case errInternal:
		return outcomeInternal
	case errForbidden:
		return outcomeForbidden
	case errEndpoint:
		return outcomeEndpoint
	case errOverloaded:
		return outcomeOverloaded
	}
	return outcomeOther
}
