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
	status int
	hdrs   map[string]string
	body   string
}

func newPoolWaitFake(t *testing.T) *poolWaitFake {
	t.Helper()
	f := &poolWaitFake{status: http.StatusOK, body: `{"data":{"ok":true}}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		f.first.CompareAndSwap(0, time.Now().UnixNano())
		for k, v := range f.hdrs {
			w.Header().Set(k, v)
		}
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *poolWaitFake) url() string { return f.srv.URL + "/i/api/graphql/x/" + poolWaitEndpoint }

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
		xtidMgr: xtid.NewManager(&failingFetcher{}),
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
