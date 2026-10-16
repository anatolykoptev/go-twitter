package twitter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anatolykoptev/go-twitter/xpff"
	"github.com/anatolykoptev/go-twitter/xtid"
)

const rlEndpoint = "SearchTimeline" // requiresAuth, POST, not in endpointDefaultLimit

// rlFakeClock is a deterministic clock for Account.clock.
type rlFakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *rlFakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *rlFakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// rlWindow is the x-rate-limit-* triple a fake X attaches to a response; a zero
// field is omitted so tests can send partial or no headers.
type rlWindow struct {
	limit, remaining int
	reset            time.Time
	omitRemaining    bool
	status           int
}

// rlFakeX is an httptest X that identifies the calling account by its auth_token
// cookie and answers with the window its `next` func picks for that account.
type rlFakeX struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits map[string]int
	n429 int
	next func(token string, nth int) rlWindow
}

func newRLFakeX(t *testing.T, next func(token string, nth int) rlWindow) *rlFakeX {
	t.Helper()
	f := &rlFakeX{hits: map[string]int{}, next: next}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := ""
		for _, part := range strings.Split(r.Header.Get("cookie"), ";") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(part), "auth_token="); ok {
				tok = v
			}
		}
		f.mu.Lock()
		f.hits[tok]++
		win := f.next(tok, f.hits[tok])
		if win.status == http.StatusTooManyRequests {
			f.n429++
		}
		f.mu.Unlock()
		if win.limit > 0 {
			w.Header().Set("x-rate-limit-limit", strconv.Itoa(win.limit))
		}
		if !win.omitRemaining {
			w.Header().Set("x-rate-limit-remaining", strconv.Itoa(win.remaining))
		}
		if !win.reset.IsZero() {
			w.Header().Set("x-rate-limit-reset", strconv.FormatInt(win.reset.Unix(), 10))
		}
		if win.status == 0 {
			win.status = http.StatusOK
		}
		w.WriteHeader(win.status)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *rlFakeX) url() string { return f.srv.URL + "/i/api/graphql/x/" + rlEndpoint }
func (f *rlFakeX) hitsFor(tok string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[tok]
}

// newRLClient builds a Client of n accounts (tokens tokA, tokB, ...) over a real
// stealth transport. perEndpointCap is the cold-start local counter cap. A
// non-nil clk drives both the accounts and their local limiters, so the whole
// eligibility decision runs on fake time.
func newRLClient(t *testing.T, n, perEndpointCap int, clk *rlFakeClock) (*Client, []*Account) {
	t.Helper()
	origJitter, origBackoff := stealth.DefaultJitter, stealth.DefaultBackoff
	stealth.DefaultJitter = stealth.Jitter{}
	stealth.DefaultBackoff = stealth.BackoffConfig{InitialWait: time.Millisecond, MaxWait: time.Millisecond, Multiplier: 1}
	t.Cleanup(func() { stealth.DefaultJitter, stealth.DefaultBackoff = origJitter, origBackoff })

	bc, err := stealth.NewClient(stealth.WithoutSSRFGuard())
	require.NoError(t, err)
	accs := make([]*Account, n)
	for i := range accs {
		name := string(rune('A' + i))
		accs[i] = newRateTestAccount(name, ratelimit.Config{RequestsPerWindow: perEndpointCap, WindowDuration: 15 * time.Minute})
		if clk != nil {
			accs[i].clock = clk.Now
			accs[i].rateLimiter = ratelimit.NewLimiter(
				ratelimit.Config{RequestsPerWindow: perEndpointCap, WindowDuration: 15 * time.Minute}, ratelimit.WithClock(clk.Now))
		}
		accs[i].AuthToken, accs[i].CT0 = "tok"+name, "ct0"
	}
	return &Client{
		client:  bc,
		pool:    pool.New(accs, pool.DefaultConfig()),
		xtidMgr: xtid.NewManager(&instantFailFetcher{}),
		xpffGen: xpff.New("test-guest-id", defaultUserAgent),
		cfg:     ClientConfig{DisableGuestFallback: true, BanCooldown: time.Hour, AuthCooldown: time.Hour},
	}, accs
}

func (c *Client) rlCall(ctx context.Context, f *rlFakeX) error {
	_, _, err := c.doPoolRequest(ctx, "POST", rlEndpoint, f.url(), []byte(`{}`))
	return err
}

// F1 — journey: a customer polls SearchTimeline. The first account's 200 says it
// is spent (remaining 0, reset in 60s); every later poll goes to the other
// account, the spent one is not touched again, and X never answers 429.
//
// Mutation: request.go observeRateLimit, drop the acc.holdIfExhausted(endpoint)
// call => RED (round-robin sends poll 3 back to the spent account).
func TestRateLimitHeaders_ExhaustedWindowRotatesWithout429(t *testing.T) {
	reset := time.Now().Add(60 * time.Second)
	spent := ""
	f := newRLFakeX(t, func(tok string, nth int) rlWindow {
		if spent == "" {
			spent = tok
		}
		if tok == spent {
			return rlWindow{limit: 50, remaining: 0, reset: reset}
		}
		return rlWindow{limit: 50, remaining: 40, reset: reset}
	})
	c, _ := newRLClient(t, 2, 50, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := range 5 {
		require.NoError(t, c.rlCall(ctx, f), "poll %d", i+1)
	}

	other := "tokA"
	if spent == "tokA" {
		other = "tokB"
	}
	assert.Equal(t, 1, f.hitsFor(spent), "the account whose 200 said remaining=0 must not be used again before its reset")
	assert.Equal(t, 4, f.hitsFor(other), "every later poll must go to the other account")
	assert.Zero(t, f.n429, "no request may draw a 429")
}

// F2 — journey: the spent account's window resets. It is eligible again at
// resetAt, not a local 15 minutes later. One account, fake clock.
//
// Mutation: ratelimit_state.go holdIfExhausted, pass now.Add(15*time.Minute)
// instead of st.resetAt to MarkEndpointRateLimited => RED (still blocked 61s on).
func TestRateLimitHeaders_ResetLiftsTheHoldAtResetAt(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	reset := clk.Now().Add(60 * time.Second)
	f := newRLFakeX(t, func(string, int) rlWindow { return rlWindow{limit: 50, remaining: 0, reset: reset} })
	c, accs := newRLClient(t, 1, 50, clk)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, c.rlCall(ctx, f))
	assert.True(t, accs[0].IsEndpointRateLimited(rlEndpoint), "spent window must hold the endpoint")
	assert.Equal(t, reset.Unix(), accs[0].EndpointAvailableAt(rlEndpoint).Unix(), "hold must end at the header reset")

	clk.Advance(30 * time.Second)
	assert.True(t, accs[0].IsEndpointRateLimited(rlEndpoint), "still inside the window")

	clk.Advance(31 * time.Second) // past reset
	assert.False(t, accs[0].IsEndpointRateLimited(rlEndpoint), "eligible again once now >= resetAt")
	require.NoError(t, c.rlCall(ctx, f), "the call after the reset must be served, not wait out a local window")
	assert.Equal(t, 2, f.hitsFor("tokA"))
}

// F3 — race rule: a late response from the same window with a HIGHER remaining
// must not raise the stored value; an older window is ignored; a newer window
// replaces. Driven through the real request path.
//
// Mutation: ratelimit_state.go recordWindow, make the equal-reset case
// overwrite (cur.remaining = obs.remaining unconditionally) => RED.
func TestRateLimitHeaders_OlderResponseNeverRaisesRemaining(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	w1 := clk.Now().Add(10 * time.Minute)
	steps := []rlWindow{
		{limit: 50, remaining: 10, reset: w1},                                            // fresh
		{limit: 50, remaining: 40, reset: w1},                                            // late response, same window: ignored
		{limit: 50, remaining: 99, reset: w1.Add(-5 * time.Minute)},                      // older window: ignored
		{limit: 50, remaining: 45, reset: w1.Add(15 * time.Minute)},                      // newer window: replaces
		{limit: 50, remaining: 40, reset: w1.Add(15 * time.Minute)},                      // same window, lower than the estimate: taken
		{limit: 50, remaining: 40, reset: w1.Add(15 * time.Minute), omitRemaining: true}, // no remaining: untouched
	}
	// Each pick under header authority spends one slot of the estimate (see
	// AllowRequest), so a response that merely echoes an old value shows the
	// decremented estimate, never a raised one.
	want := []int{10, 9, 8, 45, 40, 39}
	f := newRLFakeX(t, func(_ string, nth int) rlWindow { return steps[nth-1] })
	c, accs := newRLClient(t, 1, 50, clk)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i, wantRemaining := range want {
		require.NoError(t, c.rlCall(ctx, f), "call %d", i+1)
		_, remaining, _, ok := accs[0].rateLimitWindow(rlEndpoint)
		require.True(t, ok)
		assert.Equal(t, wantRemaining, remaining, "after response %d", i+1)
	}
}

// F4 — a 429 carrying x-rate-limit-reset blocks the endpoint until that reset,
// not now+15min.
//
// Mutation: request.go doPoolRequest 429 arm, replace
// parseRateLimitReset(respHdrs["x-rate-limit-reset"]) with
// time.Now().Add(15*time.Minute) => RED.
func TestRateLimitHeaders_429BlocksUntilHeaderReset(t *testing.T) {
	reset := time.Now().Add(2 * time.Minute)
	f := newRLFakeX(t, func(string, int) rlWindow {
		// Only the reset header: isolates the 429 arm from the header-state hold.
		return rlWindow{status: http.StatusTooManyRequests, omitRemaining: true, reset: reset}
	})
	c, accs := newRLClient(t, 1, 50, nil)

	// The ctx only bounds the pool wait that follows the 429 (the single
	// account is blocked for ~2m). It must outlast the first request itself:
	// the caller ctx now reaches the transport, so a deadline shorter than the
	// request would abort it before the 429 arm ever runs.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	require.Error(t, c.rlCall(ctx, f))

	got := accs[0].EndpointAvailableAt(rlEndpoint)
	assert.WithinDuration(t, reset, got, 2*time.Second, "429 must block until the header reset (not now+15min)")
	assert.Equal(t, 1, f.hitsFor("tokA"))
}

// F5 — responses without rate-limit headers keep today's behaviour: the local
// counter decides. Two accounts with a local cap of 2 serve exactly 4 calls.
//
// Mutation (bites both ways): ratelimit_state.go recordWindow, in the
// missing-header early return store a state instead — remaining huge => the
// counter is bypassed and a 5th call is served; remaining 0 => the endpoint is
// held after one call and fewer than 4 are served. The "unlimited" variant is the
// one run for the report.
func TestRateLimitHeaders_NoHeadersKeepsLocalCounter(t *testing.T) {
	f := newRLFakeX(t, func(string, int) rlWindow { return rlWindow{omitRemaining: true} })
	c, _ := newRLClient(t, 2, 2, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := range 4 {
		require.NoError(t, c.rlCall(ctx, f), "call %d is within the local cap", i+1)
	}
	require.Error(t, c.rlCall(ctx, f), "call 5 exceeds the local counter on both accounts")
	assert.Equal(t, 2, f.hitsFor("tokA"))
	assert.Equal(t, 2, f.hitsFor("tokB"))
}

// An expired header window stops being authoritative: if X later stops sending
// headers for the operation, the local counter applies again instead of the
// account being unlimited until a 429.
//
// Mutation: account.go endpointGate, return `ok` instead of
// `ok && now.Before(st.resetAt)` => RED (the third header-less call is served).
func TestRateLimitHeaders_ExpiredWindowFallsBackToLocalCounter(t *testing.T) {
	clk := &rlFakeClock{t: time.Now()}
	reset := clk.Now().Add(60 * time.Second)
	var headerless atomic.Bool
	f := newRLFakeX(t, func(string, int) rlWindow {
		if headerless.Load() {
			return rlWindow{omitRemaining: true}
		}
		return rlWindow{remaining: 30, reset: reset}
	})
	c, _ := newRLClient(t, 1, 3, clk) // local cap 3: the cold-start call below uses one slot

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := range 4 { // live window: the local cap of 3 is bypassed after call 1
		require.NoError(t, c.rlCall(ctx, f), "call %d inside the live window", i+1)
	}

	clk.Advance(61 * time.Second) // window expired, X stops sending headers
	headerless.Store(true)
	require.NoError(t, c.rlCall(ctx, f), "header-less call 1 within the local cap")
	require.NoError(t, c.rlCall(ctx, f), "header-less call 2 within the local cap")
	require.Error(t, c.rlCall(ctx, f), "header-less call 3 exceeds the local cap (3, one used at cold start): the counter decides again")
}

// F7 — the metric: one 200 carrying headers records remaining and limit for the
// operation.
//
// Mutation: request.go observeRateLimit, remove the c.metrics.recordRateLimit
// call => RED.
func TestRateLimitHeaders_MetricRecordsRemainingAndLimit(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
	f := newMetricsFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-rate-limit-limit", "50")
		w.Header().Set("x-rate-limit-remaining", "37")
		w.Header().Set("x-rate-limit-reset", reset)
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now().Add(-5 * time.Second)))))
	}, "rl_metric_acc")

	_, err := f.c.GetUserTweets(context.Background(), "999", 20)
	require.NoError(t, err)

	got := f.collect(t)
	for name, want := range map[string]int64{metricRateLimitRemain: 37, metricRateLimitLimit: 50} {
		m, ok := got[name]
		require.True(t, ok, "%s recorded nothing", name)
		h, isHist := m.Data.(metricdata.Histogram[int64])
		require.True(t, isHist, "%s is not an int64 histogram", name)
		pts := h.DataPoints
		require.Len(t, pts, 1, name)
		assert.Equal(t, want, pts[0].Sum, name)
		assert.Equal(t, uint64(1), pts[0].Count, name)
		assert.Equal(t, map[string]string{attrOperation: "UserTweets"}, attrMap(pts[0].Attributes), name)
	}
}

// F6 — journey on the *WithSocial path: a customer calls GetUserTweetsWithSocial
// against a fake go-social and a fake X. The acquire request carries
// endpoint=UserTweets, and the report that reaches go-social carries the window
// of that endpoint exactly as X sent it (not the earlier UserByScreenName hop).
//
// Mutation: social.go tryWithAccount, in the report closure call
// sc.ReportUsage(ctx, "twitter", creds.ID, status) instead of
// sc.ReportUsageWithRateLimit(...) => RED.
func TestRateLimitHeaders_SocialReportCarriesObservedWindow(t *testing.T) {
	reset := time.Now().Add(7 * time.Minute).Unix()
	r := newSocialRigX(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "UserByScreenName") {
			w.Header().Set("x-rate-limit-limit", "150")
			w.Header().Set("x-rate-limit-remaining", "149")
			w.Header().Set("x-rate-limit-reset", strconv.FormatInt(reset-300, 10))
			_, _ = w.Write([]byte(socialUserBody))
			return
		}
		w.Header().Set("x-rate-limit-limit", "50")
		w.Header().Set("x-rate-limit-remaining", "41")
		w.Header().Set("x-rate-limit-reset", strconv.FormatInt(reset, 10))
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now().Add(-5 * time.Second)))))
	})

	_, err := GetUserTweetsWithSocial(context.Background(), r.sc, "tesla", 5)
	require.NoError(t, err)

	r.reportsMu.Lock()
	defer r.reportsMu.Unlock()
	require.Equal(t, []string{"endpoint=UserTweets"}, r.acquireQueries, "acquire must hint the operation")
	require.Len(t, r.reportBodies, 1)
	assert.Equal(t, "success", r.reportBodies[0]["status"])
	assert.Equal(t, map[string]any{
		"endpoint": "UserTweets", "limit": float64(50), "remaining": float64(41), "reset": float64(reset),
	}, r.reportBodies[0]["rate_limit"], "report must carry the window fake X sent")
}

// A report with no observed headers is the legacy body: no rate_limit key.
func TestRateLimitHeaders_SocialReportWithoutHeadersIsLegacyBody(t *testing.T) {
	r := newSocialRig(t)
	_, err := GetUserWithSocial(context.Background(), r.sc, "tesla")
	require.NoError(t, err)

	r.reportsMu.Lock()
	defer r.reportsMu.Unlock()
	require.Len(t, r.reportBodies, 1)
	assert.Equal(t, map[string]any{"status": "success"}, r.reportBodies[0])
}

// A malformed reset (far-future sentinel, milliseconds read as seconds, +100d)
// is treated as unknown: it neither holds the account nor makes a bogus window
// authoritative. The account stays usable and a later genuine window is taken.
//
// Mutation: ratelimit_state.go parseRateLimitHeaders, drop the
// `at.Sub(time.Now()) <= maxRateLimitReset` condition => RED.
func TestRateLimitHeaders_MalformedResetIsIgnored(t *testing.T) {
	now := time.Now()
	for name, reset := range map[string]string{
		"far-future sentinel": "9999999999",
		"milliseconds":        strconv.FormatInt(now.Add(time.Minute).UnixMilli(), 10),
		"plus 100 days":       strconv.FormatInt(now.Add(100*24*time.Hour).Unix(), 10),
	} {
		t.Run(name, func(t *testing.T) {
			acc := newRateTestAccount("A", ratelimit.Config{RequestsPerWindow: 3, WindowDuration: 15 * time.Minute})
			acc.recordWindow(rlEndpoint, parseRateLimitHeaders(map[string]string{
				xRateLimitRemaining: "0", xRateLimitReset: reset,
			}))
			acc.holdIfExhausted(rlEndpoint)
			assert.False(t, acc.IsEndpointRateLimited(rlEndpoint), "a malformed reset must not hold the account")
			_, _, _, ok := acc.rateLimitWindow(rlEndpoint)
			assert.False(t, ok, "a malformed reset must not create authoritative state")

			// A genuine window afterwards is still accepted.
			genuine := now.Add(10 * time.Minute)
			acc.recordWindow(rlEndpoint, parseRateLimitHeaders(map[string]string{
				xRateLimitRemaining: "7", xRateLimitReset: strconv.FormatInt(genuine.Unix(), 10),
			}))
			_, remaining, _, ok := acc.rateLimitWindow(rlEndpoint)
			assert.True(t, ok)
			assert.Equal(t, 7, remaining)
		})
	}
}

// The 429 path applies the same ceiling and falls back to its default.
//
// Mutation: errors.go parseRateLimitReset, drop the maxRateLimitReset condition => RED.
func TestParseRateLimitReset_CeilingFallsBackToDefault(t *testing.T) {
	got := parseRateLimitReset("9999999999")
	assert.WithinDuration(t, time.Now().Add(15*time.Minute), got, 2*time.Second)
	near := time.Now().Add(5 * time.Minute)
	assert.Equal(t, near.Unix(), parseRateLimitReset(strconv.FormatInt(near.Unix(), 10)).Unix())
}

// Reset jitter: a stale response whose reset is a second later is the same
// window and must not raise remaining; a reset a whole window later is new.
//
// Mutation: ratelimit_state.go recordWindow, set resetJitterTolerance to 0 => RED.
func TestRateLimitHeaders_ResetJitterIsSameWindow(t *testing.T) {
	acc := newRateTestAccount("A", ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute})
	base := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	rec := func(remaining int, reset time.Time) {
		acc.recordWindow(rlEndpoint, rateLimitObs{remaining: remaining, resetAt: reset})
	}
	rec(10, base)
	rec(40, base.Add(time.Second)) // stale response, reset wobbled +1s
	_, remaining, resetAt, _ := acc.rateLimitWindow(rlEndpoint)
	assert.Equal(t, 10, remaining, "a stale response with a 1s-later reset must not raise remaining")
	assert.Equal(t, base.Add(time.Second), resetAt, "the later reset is kept")

	rec(45, base.Add(15*time.Minute)) // a genuine next window
	_, remaining, _, _ = acc.rateLimitWindow(rlEndpoint)
	assert.Equal(t, 45, remaining)
}

// Holds are observable: a response that spends the window counts one hold for
// the operation; a response with budget left counts none.
//
// Mutation: request.go observeRateLimit, remove the c.metrics.recordHold call => RED.
func TestRateLimitHeaders_HoldIsCounted(t *testing.T) {
	var remaining atomic.Int32
	reset := strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
	f := newMetricsFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-rate-limit-remaining", strconv.Itoa(int(remaining.Load())))
		w.Header().Set("x-rate-limit-reset", reset)
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now().Add(-5 * time.Second)))))
	}, "rl_hold_acc")

	remaining.Store(30)
	_, err := f.c.GetUserTweets(context.Background(), "999", 20)
	require.NoError(t, err)
	_, present := f.collect(t)[metricRateLimitHolds]
	assert.False(t, present, "a response with budget left must not count a hold")

	remaining.Store(0)
	_, err = f.c.GetUserTweets(context.Background(), "999", 20)
	require.NoError(t, err)
	m, ok := f.collect(t)[metricRateLimitHolds]
	require.True(t, ok, "a spent window must count a hold")
	sum, isSum := m.Data.(metricdata.Sum[int64])
	require.True(t, isSum)
	require.Len(t, sum.DataPoints, 1)
	assert.EqualValues(t, 1, sum.DataPoints[0].Value)
	assert.Equal(t, map[string]string{attrOperation: "UserTweets"}, attrMap(sum.DataPoints[0].Attributes))
}

// In-flight accounting: with header state remaining=10 and headroom 2, a burst of
// 20 concurrent picks admits at most 8 before any response lands, instead of all
// 20 on the strength of one stale response.
//
// Mutation: account.go AllowRequest, drop the `st.remaining--` decrement => RED.
func TestRateLimitHeaders_ConcurrentPicksSpendTheEstimate(t *testing.T) {
	acc := newRateTestAccount("A", ratelimit.Config{RequestsPerWindow: 3, WindowDuration: 15 * time.Minute})
	acc.recordWindow(rlEndpoint, rateLimitObs{limit: 50, remaining: 10, resetAt: time.Now().Add(10 * time.Minute)})

	const callers = 20
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if acc.AllowRequest(rlEndpoint) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.EqualValues(t, 10-rateLimitHeadroom, allowed.Load(), "only remaining-headroom picks may pass before a response lands")
	assert.True(t, acc.IsEndpointRateLimited(rlEndpoint), "the spent estimate holds the endpoint until reset")
}
