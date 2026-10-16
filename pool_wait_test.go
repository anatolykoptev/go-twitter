package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/pool"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anatolykoptev/go-twitter/xpff"
	"github.com/anatolykoptev/go-twitter/xtid"
)

const poolWaitEndpoint = "UserTweets" // requiresAuth: goes through nextAccountWithWait

// poolWaitFake is an httptest X that counts hits and answers every request with
// the configured status, headers and body.
type poolWaitFake struct {
	srv    *httptest.Server
	hits   atomic.Int32
	first  atomic.Int64 // UnixNano of the first hit
	status atomic.Int32
	hdrs   map[string]string
	body   string
}

func newPoolWaitFake(t *testing.T) *poolWaitFake {
	t.Helper()
	f := &poolWaitFake{body: `{"data":{"ok":true}}`}
	f.status.Store(http.StatusOK)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		f.first.CompareAndSwap(0, time.Now().UnixNano())
		for k, v := range f.hdrs {
			w.Header().Set(k, v)
		}
		w.WriteHeader(int(f.status.Load()))
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *poolWaitFake) url() string { return f.srv.URL + "/i/api/graphql/x/" + poolWaitEndpoint }

// instantFailFetcher fails with a permanent (4xx-shaped) error so the xtid
// manager gives up without its retry backoff (~1.3s per request with the
// retrying failingFetcher), keeping these timing tests about the pool wait.
type instantFailFetcher struct{}

func (instantFailFetcher) Fetch(context.Context, string) ([]byte, error) {
	return nil, errors.New("test fetcher: offline HTTP 403")
}

// newPoolWaitClient builds a Client with n healthy accounts over a real stealth
// transport pointed at the fake. Retry backoff and anti-fingerprint jitter are
// zeroed so the timings under test are the pool-wait ones.
func newPoolWaitClient(t *testing.T, n int) (*Client, []*Account) {
	t.Helper()
	origJitter, origBackoff := stealth.DefaultJitter, stealth.DefaultBackoff
	stealth.DefaultJitter = stealth.Jitter{}
	stealth.DefaultBackoff = stealth.BackoffConfig{InitialWait: time.Millisecond, MaxWait: time.Millisecond, Multiplier: 1}
	t.Cleanup(func() { stealth.DefaultJitter, stealth.DefaultBackoff = origJitter, origBackoff })

	bc, err := stealth.NewClient(stealth.WithoutSSRFGuard())
	require.NoError(t, err)

	accs := make([]*Account, n)
	for i := range accs {
		accs[i] = newRateTestAccount("acc"+strconv.Itoa(i), ratelimit.Config{RequestsPerWindow: 50, WindowDuration: 15 * time.Minute})
		accs[i].AuthToken, accs[i].CT0 = "auth", "ct0"
	}
	c := &Client{
		client:  bc,
		pool:    pool.New(accs, pool.DefaultConfig()),
		xtidMgr: xtid.NewManager(&instantFailFetcher{}),
		xpffGen: xpff.New("test-guest-id", defaultUserAgent),
		cfg: ClientConfig{
			DisableGuestFallback: true,
			BanCooldown:          time.Hour,
			AuthCooldown:         time.Hour,
		},
	}
	return c, accs
}

// F1 (journey: a consumer calls GetUserTweets-class endpoints under burst load):
// every account is active but rate-limited on the endpoint for a short while.
// The call must wait for the window and then succeed instead of failing with
// "pool exhausted".
//
// Mutation: in request.go doPoolRequest, call c.pool.NextWithWait(ctx, filter,
// 5*time.Minute) instead of c.nextAccountWithWait => RED.
func TestPoolWait_AllAccountsEndpointLimited_WaitsAndSucceeds(t *testing.T) {
	fake := newPoolWaitFake(t)
	c, accs := newPoolWaitClient(t, 3)
	const limitedFor = 200 * time.Millisecond
	start := time.Now()
	for _, a := range accs {
		a.MarkEndpointRateLimited(poolWaitEndpoint, start.Add(limitedFor))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, _, err := c.doGET(ctx, poolWaitEndpoint, fake.url())

	require.NoError(t, err, "all accounts are only limited for %s; the call must wait, not fail", limitedFor)
	assert.Contains(t, string(body), `"ok":true`)
	assert.GreaterOrEqual(t, time.Unix(0, fake.first.Load()).Sub(start), limitedFor-10*time.Millisecond,
		"the request must reach X only after the endpoint limit lifted")
	assert.EqualValues(t, 1, fake.hits.Load())
}

// F2: the wait honours the caller's context.
//
// Mutation: in request.go nextAccountWithWait replace the select on t.C /
// ctx.Done() with time.Sleep(wait) => RED (returns after ~10s, not ~50ms).
func TestPoolWait_HonoursContextDeadline(t *testing.T) {
	fake := newPoolWaitFake(t)
	c, accs := newPoolWaitClient(t, 2)
	for _, a := range accs {
		a.MarkEndpointRateLimited(poolWaitEndpoint, time.Now().Add(10*time.Second))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := c.doGET(ctx, poolWaitEndpoint, fake.url())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "want ctx deadline error, got %v", err)
	assert.Less(t, elapsed, time.Second, "must return near the 50ms deadline, not after the 10s limit")
	assert.EqualValues(t, 0, fake.hits.Load())
}

// F3: with no active account nothing is waited for; the error is the original
// "pool exhausted" one.
func TestPoolWait_NoActiveAccounts_FailsFast(t *testing.T) {
	fake := newPoolWaitFake(t)
	c, accs := newPoolWaitClient(t, 2)
	for _, a := range accs {
		c.pool.DeactivateItem(a) // permanent: no reactivation time either
		a.MarkEndpointRateLimited(poolWaitEndpoint, time.Now().Add(time.Minute))
	}

	start := time.Now()
	_, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pool exhausted for "+poolWaitEndpoint+" (requires auth)")
	assert.Less(t, time.Since(start), 500*time.Millisecond, "no active account: must not wait")
	assert.EqualValues(t, 0, fake.hits.Load())
}

// A wake-up beyond the 5 minute cap is not waited for.
func TestPoolWait_BeyondCap_FailsFast(t *testing.T) {
	fake := newPoolWaitFake(t)
	c, accs := newPoolWaitClient(t, 1)
	accs[0].MarkEndpointRateLimited(poolWaitEndpoint, time.Now().Add(poolWaitCap+time.Minute))

	start := time.Now()
	_, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pool exhausted")
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}

// The wait budget is per REQUEST, not per attempt: doPoolRequest calls
// nextAccountWithWait once per attempt (maxRetries), so a per-call cap would let
// one request block maxRetries x poolWaitCap.
//
// Cap 1.5s; X answers 429 and blocks the endpoint for ~1s each time. Attempt 1
// waits ~1s (fits), attempt 2 would need another ~1s with only ~0.5s of budget
// left and must give up.
//
// Mutation: in request.go nextAccountWithWait take the deadline as
// time.Now().Add(poolWaitCap) inside the helper instead of the parameter => RED
// (3 hits).
func TestPoolWait_BudgetSharedAcrossAttempts(t *testing.T) {
	orig := poolWaitCap
	poolWaitCap = 1500 * time.Millisecond
	t.Cleanup(func() { poolWaitCap = orig })

	fake := newPoolWaitFake(t)
	c, _ := newPoolWaitClient(t, 1)
	// Warm the transport (first request pays a ~1s+ one-off setup that would eat
	// the budget) before the measured call.
	_, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())
	require.NoError(t, err)
	fake.hits.Store(0)

	fake.status.Store(http.StatusTooManyRequests)
	fake.hdrs = map[string]string{xRateLimitReset: strconv.FormatInt(time.Now().Add(time.Second).Unix(), 10)}
	_, _, err = c.doGET(context.Background(), poolWaitEndpoint, fake.url())

	require.Error(t, err)
	assert.EqualValues(t, 2, fake.hits.Load(), "one wait budget per request: attempt 3 must not get a fresh cap")
}

// The wake-up jitter spreads waiters parked on one freed account, but it is
// bounded by the same budget as the wait itself: the jitter is added before
// the deadline clamp, never after, so a sleep cannot park past poolWaitCap
// (the ctx select bounds it by the caller's context separately).
//
// Mutation: in request.go nextAccountWithWait drop the `wait > budget` clamp
// after adding the jitter => RED (the call sleeps ~wait+jitter ≈ 700ms, not
// ~cap).
func TestPoolWait_JitterStaysInsideBudget(t *testing.T) {
	origCap, origJitter := poolWaitCap, poolWakeJitter
	poolWaitCap = 300 * time.Millisecond
	poolWakeJitter = func() time.Duration { return 500 * time.Millisecond } // deliberately larger than the cap
	t.Cleanup(func() { poolWaitCap, poolWakeJitter = origCap, origJitter })

	fake := newPoolWaitFake(t)
	c, accs := newPoolWaitClient(t, 1)
	// Warm the transport: the first request pays a ~1s one-off setup that
	// would otherwise eat the measured window.
	_, _, err := c.doGET(context.Background(), poolWaitEndpoint, fake.url())
	require.NoError(t, err)

	accs[0].MarkEndpointRateLimited(poolWaitEndpoint, time.Now().Add(200*time.Millisecond))
	start := time.Now()
	_, _, err = c.doGET(context.Background(), poolWaitEndpoint, fake.url())
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 450*time.Millisecond,
		"wait+jitter must be clamped to the remaining wait budget, not stacked on top")
}

// ctx done => the ctx error, even when NextWithWait itself returns its own
// "all N items are unavailable" (it does not look at ctx before giving up).
//
// Mutation: in nextAccountWithWait restore `if err == nil || ctx.Err() != nil
// { return acc, err }` => RED.
func TestPoolWait_CancelledCtxReturnsCtxError(t *testing.T) {
	c, accs := newPoolWaitClient(t, 2)
	for _, a := range accs {
		a.MarkEndpointRateLimited(poolWaitEndpoint, time.Now().Add(10*time.Second))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.nextAccountWithWait(ctx, poolWaitEndpoint, func(a *Account) bool { return a.AllowRequest(poolWaitEndpoint) }, nil, false, time.Now().Add(time.Minute))

	assert.True(t, errors.Is(err, context.Canceled), "want context.Canceled, got %v", err)
}

// A reset already in the past would mark nothing; it is floored at now+1s.
//
// Mutation: in errors.go parseRateLimitReset delete the floor branch => RED.
func TestParseRateLimitReset_PastIsFloored(t *testing.T) {
	got := parseRateLimitReset(strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10))
	assert.WithinDuration(t, time.Now().Add(minRateLimitBlock), got, 200*time.Millisecond)

	future := time.Now().Add(time.Hour).Truncate(time.Second)
	assert.True(t, parseRateLimitReset(strconv.FormatInt(future.Unix(), 10)).Equal(future), "future reset must be kept as is")
}
