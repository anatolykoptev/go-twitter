package twitter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"context"
	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryPaceRig is a two-account pool over a loopback server that records, per
// account token, the arrival time of every request and answers through reply.
type retryPaceRig struct {
	c    *Client
	accs []*Account

	mu    sync.Mutex
	times map[string][]time.Time
}

func (r *retryPaceRig) arrivals(tok string) []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.times[tok]...)
}

// newRetryPaceRig builds n accounts (tokens tok0..), a retry backoff of
// `backoff` (fixed, no jitter) and — when gap > 0 — a real per-account pacer
// with that fixed gap. reply decides the response for a given account token.
func newRetryPaceRig(t *testing.T, n int, backoff, gap time.Duration, reply func(w http.ResponseWriter, tok string)) *retryPaceRig {
	t.Helper()
	r := &retryPaceRig{times: map[string][]time.Time{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tok := ""
		for i := 0; i < n; i++ {
			if strings.Contains(req.Header.Get("Cookie"), "auth_token=tok"+string(rune('0'+i))) {
				tok = "tok" + string(rune('0'+i))
			}
		}
		r.mu.Lock()
		r.times[tok] = append(r.times[tok], time.Now())
		r.mu.Unlock()
		reply(w, tok)
	}))
	t.Cleanup(srv.Close)

	r.c, r.accs = newPoolWaitClient(t, n)
	for i, a := range r.accs {
		a.AuthToken = "tok" + string(rune('0'+i))
	}
	// newPoolWaitClient shrinks the backoff to 1ms; restore a visible one
	// (its cleanup already puts the original back).
	stealth.DefaultBackoff = stealth.BackoffConfig{InitialWait: backoff, MaxWait: backoff, Multiplier: 1}
	if gap > 0 {
		r.c.cfg.AccountPaceMin, r.c.cfg.AccountPaceRandom = gap, time.Nanosecond
		r.c.accountPacer = ratelimit.NewKeyedPacer(gap, time.Nanosecond)
	}
	origBase := twitterBase
	twitterBase = srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = origBase })
	return r
}

func okTweets(w http.ResponseWriter) {
	_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
}

// P1 — journey: account 0 answers 429, account 1 is idle. The retry goes
// straight to account 1: a 429 is a verdict about account 0 only.
//
// Mutation: request.go doPoolRequest, the `skipBackoff` gate before the retry
// sleep -> force it false (restore the unconditional sleep) => RED.
func TestPoolRetry_AccountScopedFailureSkipsBackoffWhenOtherAccountIdle(t *testing.T) {
	r := newRetryPaceRig(t, 2, time.Second, 0, func(w http.ResponseWriter, tok string) {
		if tok == "tok0" {
			w.Header().Set("x-rate-limit-reset", "9999999999")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		okTweets(w)
	})
	start := time.Now()
	tweets, err := r.c.GetUserTweets(context.Background(), "999", 20)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Len(t, tweets, 1)
	assert.Len(t, r.arrivals("tok1"), 1, "the idle account serves the retry")
	assert.Less(t, elapsed, 300*time.Millisecond, "a 429 on one account must not delay the retry on another (backoff is 1s)")
}

// P2 — an X-side failure (503) is not about the serving account: pacing the
// next attempt still protects everyone, so the backoff stays.
//
// Mutation: request.go doPoolRequest, `skipBackoff` -> true for every class
// (drop the account-scoped test) => RED.
func TestPoolRetry_XSideFailureKeepsBackoff(t *testing.T) {
	const backoff = 600 * time.Millisecond
	for _, tc := range []struct {
		name string
		fail func(w http.ResponseWriter)
	}{
		{"503", func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }},
		{"LoadShed on 200", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"errors":[{"code":-1,"message":"LoadShed"}]}`))
		}},
		{"131 internal", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"errors":[{"code":131,"message":"Internal error"}]}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRetryPaceRig(t, 2, backoff, 0, func(w http.ResponseWriter, tok string) {
				if tok == "tok0" {
					tc.fail(w)
					return
				}
				okTweets(w)
			})
			start := time.Now()
			_, err := r.c.GetUserTweets(context.Background(), "999", 20)
			elapsed := time.Since(start)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, elapsed, backoff*9/10, "the retry after an X-side failure must still be paced")
		})
	}
}

// P3 — the next request goes to the account whose pace gap has elapsed, with
// no pacer sleep, even though round-robin points at the just-used one.
//
// Mutation: request.go doPoolRequest, `preferReady` -> `return nil` (pick is
// pace-unaware again) => RED.
func TestPoolPace_PrefersAccountPastItsGap(t *testing.T) {
	const gap = 500 * time.Millisecond
	r := newRetryPaceRig(t, 2, time.Millisecond, gap, func(w http.ResponseWriter, _ string) { okTweets(w) })
	// acc0 just served a request: the pacer holds it and the pick knows.
	require.True(t, r.c.accountPacer.Allow(r.accs[0].ID()))
	r.accs[0].reservePace(gap)

	start := time.Now()
	_, err := r.c.GetUserTweets(context.Background(), "999", 20)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Len(t, r.arrivals("tok1"), 1, "the ready account must serve")
	assert.Empty(t, r.arrivals("tok0"), "the just-used account must be left alone")
	assert.Less(t, elapsed, gap/2, "no pace wait while another account is ready")
}

// P4 — spacing invariant under load: no account ever sends two requests
// closer than its gap, however many callers race for the pool.
//
// Mutation: request.go doPoolRequest, `if c.accountPacer != nil {` around the
// post-pick Wait -> `if false {` (drop the guarantee) => RED.
func TestPoolPace_NeverSpacesAccountCloserThanGap(t *testing.T) {
	const gap = 150 * time.Millisecond
	r := newRetryPaceRig(t, 2, time.Millisecond, gap, func(w http.ResponseWriter, _ string) { okTweets(w) })
	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.c.GetUserTweets(context.Background(), "999", 20)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	total := 0
	for _, tok := range []string{"tok0", "tok1"} {
		ts := r.arrivals(tok)
		total += len(ts)
		for i := 1; i < len(ts); i++ {
			// Arrival order can differ from grant order by scheduling noise only.
			assert.GreaterOrEqual(t, ts[i].Sub(ts[i-1]), gap-30*time.Millisecond, "account %s: requests %d and %d are closer than the gap", tok, i-1, i)
		}
	}
	assert.Equal(t, callers, total)
}

// P5 — every account is inside its gap: the request waits the gap out and
// succeeds; it does not fail with "pool exhausted".
//
// Mutation: request.go doPoolRequest, drop the fallback to the regular pick
// (make the pace refusal final: `acc, accErr = nil, errPaceBusy` instead of
// falling through to nextAccountWithWait/pool.Next) => RED.
func TestPoolPace_AllAccountsInsideGapWaitsAndSucceeds(t *testing.T) {
	const gap = 400 * time.Millisecond
	r := newRetryPaceRig(t, 2, time.Millisecond, gap, func(w http.ResponseWriter, _ string) { okTweets(w) })
	for _, a := range r.accs {
		require.True(t, r.c.accountPacer.Allow(a.ID()))
		a.reservePace(gap)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.c.GetUserTweets(ctx, "999", 20)
	elapsed := time.Since(start)
	require.NoError(t, err, "a pool inside its gaps must wait, not fail")
	assert.GreaterOrEqual(t, elapsed, gap/2, "the pacer still enforces the gap")
	assert.Less(t, elapsed, 2*time.Second, "the wait is bounded by the gap")
}
