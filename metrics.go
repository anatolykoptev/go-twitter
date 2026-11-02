package twitter

import (
	"strconv"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/anatolykoptev/go-twitter/internal/telemetry"
)

// Metrics: see docs/metrics.md. The instruments, metric names and label values
// live in internal/telemetry; this file adapts them to the root types (Tweet,
// rateLimitObs, Account, the error taxonomy).

// clientMetrics is the root-side handle on the telemetry instruments. A nil
// *clientMetrics is valid and records nothing, so a bare &Client{} (tests,
// embedding) never panics.
type clientMetrics struct {
	t   *telemetry.Metrics
	now func() time.Time // injectable clock for freshness
}

// newClientMetrics builds the instruments on mp (nil = otel global provider).
// An instrument that fails to create degrades to a no-op and is logged once;
// construction never fails and never panics.
func newClientMetrics(mp metric.MeterProvider) *clientMetrics {
	return &clientMetrics{t: telemetry.New(mp), now: time.Now}
}

// registerPoolGauge wires the async twitter.client.pool.accounts gauge to the
// client's pool. Failure is logged and leaves the gauge absent.
func (c *Client) registerPoolGauge(mp metric.MeterProvider) {
	reg := telemetry.RegisterPoolGauge(mp, c.poolAccountStates, func() int64 {
		var inFlight int64
		for _, acc := range c.pool.Items() {
			inFlight += int64(acc.InFlight())
		}
		return inFlight
	})
	if reg == nil {
		return
	}
	c.mu.Lock()
	c.poolGaugeReg = reg
	c.mu.Unlock()
}

// Close closes the idle connections of every HTTP client this Client built (the
// shared one, each account's, and the open-account login client) and
// unregisters the pool.accounts gauge callback so the client can be garbage
// collected and a replacement client can report on the same MeterProvider. It
// is idempotent and safe on a nil client.
//
// It does not stop in-flight requests or background xtid / queryId refreshes,
// and it does not close the HTTP clients: the Client stays usable, a connection
// opened after Close idles out on its own, and the process-wide xtid manager,
// pacer and fetch client of the *WithSocial path are left alone.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.owned.closeIdle()
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
	counts := make(map[string]int64, len(telemetry.AccountStates))
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
	proxyClientFailed := acc.proxyClientFailed
	acc.mu.Unlock()
	if proxyClientFailed {
		return telemetry.AccountStateDeactivated // permanent: never reactivated
	}
	if !active {
		if reactivateAt.IsZero() {
			return telemetry.AccountStateDeactivated
		}
		return telemetry.AccountStateCoolingDown
	}
	if backoff.After(now) {
		return telemetry.AccountStateProxyBackoff
	}
	return telemetry.AccountStateActive
}

// recordRequest records one HTTP attempt.
func (m *clientMetrics) recordRequest(endpoint string, d time.Duration, outcome string) {
	if m == nil {
		return
	}
	m.t.RecordRequest(endpoint, d, outcome)
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
	m.t.RecordFreshness(endpoint, age)
}

// recordPoolWait records the total time one call spent inside
// nextAccountWithWait actually waiting (sleeping for a usable account), by
// operation and wait result. Calls that never waited record nothing.
func (m *clientMetrics) recordPoolWait(endpoint string, d time.Duration, result string) {
	if m == nil {
		return
	}
	m.t.RecordPoolWait(endpoint, d, result)
}

// recordRateLimit records what one response said about its endpoint's window:
// remaining and limit, each only when the response carried a parseable header.
func (m *clientMetrics) recordRateLimit(endpoint string, obs rateLimitObs) {
	if m == nil {
		return
	}
	m.t.RecordRateLimit(endpoint, obs.remaining, obs.limit)
}

// recordHold counts one proactive hold: an endpoint taken out of rotation on an
// account because a response showed its window spent.
func (m *clientMetrics) recordHold(endpoint string) {
	if m == nil {
		return
	}
	m.t.RecordHold(endpoint)
}

// recordQIDRefresh counts one refresh attempt (see docs/metrics.md).
func (m *clientMetrics) recordQIDRefresh(trigger, result string) {
	if m == nil {
		return
	}
	m.t.RecordQIDRefresh(trigger, result)
}

// recordQIDRefreshDuration records how long one refresh attempt took.
func (m *clientMetrics) recordQIDRefreshDuration(trigger, result string, d time.Duration) {
	if m == nil {
		return
	}
	m.t.RecordQIDRefreshDuration(trigger, result, d)
}

// recordQIDChanged records how many ids the last successful refresh changed
// against the compiled set.
func (m *clientMetrics) recordQIDChanged(n int) {
	if m == nil {
		return
	}
	m.t.RecordQIDChanged(n)
}

// recordQIDAge samples the age of the active queryId map (seconds).
func (m *clientMetrics) recordQIDAge(seconds float64) {
	if m == nil {
		return
	}
	m.t.RecordQIDAge(seconds)
}

func (m *clientMetrics) recordRelogin(outcome string) {
	if m == nil {
		return
	}
	m.t.RecordRelogin(outcome)
}

func (m *clientMetrics) recordProxyClientFailure() {
	if m == nil {
		return
	}
	m.t.RecordProxyClientFailure()
}

// recordXtidFailure counts one request sent without x-client-transaction-id.
func (m *clientMetrics) recordXtidFailure() {
	if m == nil {
		return
	}
	m.t.RecordXtidFailure()
}

func (m *clientMetrics) recordProxyFailure() {
	if m == nil {
		return
	}
	m.t.RecordProxyFailure()
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
			return telemetry.OutcomeProxy
		}
		return telemetry.OutcomeTransport
	}
	ae := newAPIError(status, body, hdrs)
	// A 404 keeps its historical not_found label: dashboards and alerts keyed
	// on twitter.outcome=not_found track the raw 404 rate regardless of the
	// errEndpoint classification the retry loop now gives it. GraphQL-shaped
	// 400s — the other endpoint-fault source — take the new "endpoint" value.
	if ae.Class == errEndpoint && status == 404 {
		return telemetry.OutcomeNotFound
	}
	if ae.Class != errNone {
		return outcomeForClass(ae.Class)
	}
	switch {
	case status >= 200 && status < 300:
		return telemetry.OutcomeOK
	case status == 401:
		return telemetry.OutcomeAuth
	}
	return telemetry.OutcomeOther
}

// outcomeForClass maps the typed error classification to an outcome value.
// errClassCount (errors.go) plus TestOutcomeForClass_CoversEveryClass make a
// new class without a mapping fail CI.
func outcomeForClass(c errorClass) string {
	switch c {
	case errNone:
		return telemetry.OutcomeOK
	case errBanned:
		return telemetry.OutcomeBanned
	case errRateLimited:
		return telemetry.OutcomeRateLimited
	case errSuspended:
		return telemetry.OutcomeSuspended
	case errLocked:
		return telemetry.OutcomeLocked
	case errCSRF:
		return telemetry.OutcomeCSRF
	case errAuthExpired, errNotAuthorized:
		return telemetry.OutcomeAuth
	case errBlocked:
		return telemetry.OutcomeBlocked
	case errInternal:
		return telemetry.OutcomeInternal
	case errForbidden:
		return telemetry.OutcomeForbidden
	case errEndpoint:
		return telemetry.OutcomeEndpoint
	case errOverloaded:
		return telemetry.OutcomeOverloaded
	case errBadRequest:
		return telemetry.OutcomeBadRequest
	}
	return telemetry.OutcomeOther
}
