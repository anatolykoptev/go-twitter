package twitter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Follow-ups to the relogin work (#156) and the errors-only classification
// (#158). Every test names the production line whose mutation turns it RED.

const (
	code32Body  = `{"errors":[{"code":32,"message":"Could not authenticate you"}]}`
	code353Body = `{"errors":[{"code":353,"message":"csrf"}]}`
)

// --- #156.1: a failed relogin benches the account on every write/media path ---

// An account without a password cannot relogin, so every code-32 / code-353
// answer ends in a failed relogin. The account must then leave rotation.
func TestFailedRelogin_BenchesAccount(t *testing.T) {
	type run func(t *testing.T, c *Client, acc *Account, base string) error
	cases := map[string]struct {
		status int
		body   string
		run    run
	}{
		// Mutation: request.go doPOST errAuthExpired arm — delete the
		// c.pool.SoftDeactivate line after "relogin failed" => RED.
		"doPOST_code32": {401, code32Body, func(t *testing.T, c *Client, acc *Account, base string) error {
			_, err := c.doPOST(context.Background(), acc, "CreateTweet", base+"/i/api/graphql/x/CreateTweet", []byte(`{}`))
			return err
		}},
		// Mutation: request.go recoverCSRFPost — delete the SoftDeactivate
		// line after "relogin after CSRF failed" => RED.
		"doPOST_csrf": {403, code353Body, func(t *testing.T, c *Client, acc *Account, base string) error {
			_, err := c.doPOST(context.Background(), acc, "CreateTweet", base+"/i/api/graphql/x/CreateTweet", []byte(`{}`))
			return err
		}},
		// Mutation: media.go doMediaRequest errAuthExpired arm — delete the
		// SoftDeactivate line after the failed relogin => RED.
		"media_code32": {401, code32Body, func(t *testing.T, c *Client, acc *Account, base string) error {
			_, err := c.doMediaRequest(context.Background(), acc, "POST", base+"/1.1/media/upload.json?command=INIT", nil, "application/x-www-form-urlencoded")
			return err
		}},
		// Mutation: media.go recoverCSRFMedia — delete the SoftDeactivate
		// line after "relogin after CSRF failed" => RED.
		"media_csrf": {403, code353Body, func(t *testing.T, c *Client, acc *Account, base string) error {
			_, err := c.doMediaRequest(context.Background(), acc, "POST", base+"/1.1/media/upload.json?command=INIT", nil, "application/x-www-form-urlencoded")
			return err
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newPoolWaitFake(t)
			fake.status.Store(int32(tc.status))
			fake.body = tc.body
			c, accs := newPoolWaitClient(t, 1)

			err := tc.run(t, c, accs[0], fake.srv.URL)
			require.Error(t, err)
			assert.False(t, accs[0].IsActive(), "a failed relogin must take the account out of rotation, as the pool paths do")
		})
	}
}

// --- #156.2: relogin is single-flight per account ---

// blockingGate is an AutoReloginGate that parks the leader inside Allowed until
// released and counts what the relogin reported.
type blockingGate struct {
	release   chan struct{}
	allowed   atomic.Int32
	successes atomic.Int32
	failures  atomic.Int32
}

func newBlockingGate() *blockingGate { return &blockingGate{release: make(chan struct{})} }

func (g *blockingGate) Allowed(context.Context, string) (bool, string) {
	g.allowed.Add(1)
	<-g.release
	return true, ""
}
func (g *blockingGate) RecordFailure(string) { g.failures.Add(1) }
func (g *blockingGate) RecordSuccess(string) { g.successes.Add(1) }

// reloginWaiters reports how many callers are parked on acc's in-flight relogin.
func reloginWaiters(c *Client, acc *Account) int {
	c.reloginMu.Lock()
	defer c.reloginMu.Unlock()
	if f := c.reloginFlights[acc]; f != nil {
		return int(f.waiters.Load())
	}
	return 0
}

// waitFor polls cond for up to d.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// N concurrent requests hit code 32 on the SAME account: exactly one login
// reaches the login server, the gate sees one outcome, and every request is
// served with the new token.
//
// Mutation: auth.go relogin — make the in-flight lookup never match (`if f, ok
// := c.reloginFlights[acc]; ok && false`) => RED: logins == N, gate saw N.
func TestRelogin_ConcurrentCode32_SingleLogin(t *testing.T) {
	const n = 6
	c, acc, j := newReloginJourneyClient(t, seededStore(), "pw")
	c.cfg.MaxInFlightPerAccount = -1
	gate := newBlockingGate()
	c.SetAutoReloginGate(gate)

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, errs[i] = c.GetUserTweets(ctx, "100"+string(rune('0'+i)), 20)
		}()
	}
	// Hold the login until every other request is parked on it. A mutant that
	// does not coalesce never parks anyone: the wait times out and the counts
	// below fail.
	waitFor(3*time.Second, func() bool { return reloginWaiters(c, acc) == n-1 })
	close(gate.release)
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "request %d must be served after the shared relogin", i)
	}
	assert.EqualValues(t, 1, j.logins.Load(), "one login request must reach the login server for %d concurrent code-32 callers", n)
	assert.EqualValues(t, 1, gate.allowed.Load(), "the gate is consulted once per flight")
	assert.EqualValues(t, 1, gate.successes.Load(), "the breaker must see ONE outcome, not one per caller")
	assert.Zero(t, gate.failures.Load())
}

// A failed shared relogin is also ONE failure for the breaker, and every
// caller gets the failure.
//
// Mutation: auth.go relogin — as above => RED: failures == N.
func TestRelogin_ConcurrentFailure_OneBreakerFailure(t *testing.T) {
	const n = 5
	c, acc, _ := newReloginJourneyClient(t, seededStore(), "") // token-only: the relogin cannot succeed
	gate := newBlockingGate()
	c.SetAutoReloginGate(gate)

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.relogin(context.Background(), acc)
		}()
	}
	waitFor(3*time.Second, func() bool { return reloginWaiters(c, acc) == n-1 })
	close(gate.release)
	wg.Wait()

	for i, err := range errs {
		assert.Error(t, err, "caller %d must see the shared failure", i)
	}
	assert.EqualValues(t, 1, gate.failures.Load(), "one flight, one recorded failure")
}

// A waiter whose ctx ends stops waiting at once; the flight and its leader are
// unaffected.
//
// Mutation: auth.go relogin — remove the `case <-ctx.Done():` arm of the
// waiter's select => RED: the cancelled waiter does not return.
func TestRelogin_WaiterHonoursContext(t *testing.T) {
	c, acc, _ := newReloginJourneyClient(t, seededStore(), "pw")
	gate := newBlockingGate()
	c.SetAutoReloginGate(gate)

	leader := make(chan error, 1)
	go func() { leader <- c.relogin(context.Background(), acc) }()
	require.True(t, waitFor(3*time.Second, func() bool { return gate.allowed.Load() == 1 }), "leader never started")

	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { waiter <- c.relogin(ctx, acc) }()
	require.True(t, waitFor(3*time.Second, func() bool { return reloginWaiters(c, acc) == 1 }), "waiter never parked")
	cancel()
	select {
	case err := <-waiter:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter with a cancelled ctx must return without waiting for the flight")
	}

	close(gate.release)
	require.NoError(t, <-leader, "the leader's relogin is unaffected by a waiter's cancel")
	assert.EqualValues(t, 1, gate.allowed.Load())
}

// --- #156.3: session persistence after a relogin ---

// landedThenTimeoutStore performs the Save and then reports a deadline error,
// like a store whose write committed just as the client's 5s bound expired.
type landedThenTimeoutStore struct{ *recordingStore }

func (s landedThenTimeoutStore) Save(ctx context.Context, u string, sess Session) error {
	_ = s.recordingStore.Save(ctx, u, sess)
	return context.DeadlineExceeded
}

// A save that timed out AFTER the write landed must not delete the good session.
//
// Mutation: auth.go establishCredentials — in the save-failure handler call
// c.deleteSession unconditionally (drop the read-back check) => RED: the stored
// session is gone.
func TestRelogin_SaveTimeoutAfterLandedWrite_KeepsSession(t *testing.T) {
	rec := seededStore()
	c, _, _ := newReloginJourneyClient(t, landedThenTimeoutStore{rec}, "pw")

	require.NoError(t, getTweets(t, c))

	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, reloginNewTok, rec.data["u1"].AuthToken, "the landed write holds the live session; it must survive")
	assert.Empty(t, rec.deletes, "no delete when the new session is verifiably stored")
}

// A timed-out save that did NOT land leaves the rejected token stored: that one
// is dropped.
//
// Mutation: auth.go establishCredentials — treat a timeout as "landed" (return
// without the read-back comparison) => RED: the stale entry stays.
func TestRelogin_SaveTimeoutWithoutLandedWrite_DropsStale(t *testing.T) {
	rec := seededStore()
	c, _, _ := newReloginJourneyClient(t, timeoutNoWriteStore{rec}, "pw")

	require.NoError(t, getTweets(t, c))

	rec.mu.Lock()
	defer rec.mu.Unlock()
	_, stillThere := rec.data["u1"]
	assert.False(t, stillThere, "the write never landed, so the rejected session must be dropped")
}

type timeoutNoWriteStore struct{ *recordingStore }

func (s timeoutNoWriteStore) Save(context.Context, string, Session) error {
	return context.DeadlineExceeded
}

// capture collects slog records at or above Error.
type capture struct {
	mu   sync.Mutex
	msgs []string
}

func (h *capture) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelError }
func (h *capture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Message)
	h.mu.Unlock()
	return nil
}
func (h *capture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capture) WithGroup(string) slog.Handler      { return h }

// Save AND Delete fail: nothing can be written to the store, the relogin still
// succeeded in memory — but the rejected session left in the store must not be
// silent.
//
// Mutation: auth.go establishCredentials — drop the slog.Error emitted when the
// stale entry cannot be removed => RED: no error record.
func TestRelogin_SaveAndDeleteFail_StaleEntryIsReported(t *testing.T) {
	rec := seededStore()
	rec.saveErr = errors.New("store down")
	rec.delErr = errors.New("store down")
	c, acc, _ := newReloginJourneyClient(t, rec, "pw")

	h := &capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	require.NoError(t, getTweets(t, c))

	tok, _, _ := acc.Credentials()
	assert.Equal(t, reloginNewTok, tok, "the live credentials are the new ones")
	h.mu.Lock()
	defer h.mu.Unlock()
	found := false
	for _, m := range h.msgs {
		if strings.Contains(m, "rejected session") {
			found = true
		}
	}
	assert.True(t, found, "an unremovable rejected session must be reported at error level; got %v", h.msgs)
}

// --- #158.1: postReloginFailure honours the retry's HTTP status ---

// Mutation: request.go postReloginFailure — drop the `status` guard so
// errorsOnlyClass applies to any status (`case errorsOnly(body):`) => RED
// (ErrUnexpectedResponse for the 5xx/404 rows).
func TestPostReloginFailure_NonSuccessStatusKeepsStatusClass(t *testing.T) {
	body := []byte(`{"errors":[{"code":130,"message":"Over capacity"}]}`) // unlisted code: errors-only
	cases := []struct {
		name   string
		status int
		want   errorClass
		active bool
	}{
		{"503_overloaded", 503, errOverloaded, true},
		{"404_endpoint", 404, errEndpoint, true},
		{"410_endpoint", 410, errEndpoint, true},
		{"200_unexpected", 200, errUnexpected, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, accs := newPoolWaitClient(t, 1)
			ae := c.postReloginFailure(accs[0], "UserTweets", tc.status, body, nil, nil, errAuthExpired, time.Hour, "post-relogin request failed")
			assert.Equal(t, tc.want, ae.Class)
			assert.Equal(t, tc.active, accs[0].IsActive(), "an X-side or terminal verdict must not bench the account just logged in")
		})
	}
}

// Journey: 401 code 32, relogin, the retry meets a 503 errors-only body. The
// 5xx stays retryable: more than the initial + post-relogin request reach X,
// and the verdict is overload, not a terminal unexpected response.
//
// Mutation: as above => RED (ErrUnexpectedResponse, 2 requests).
func TestPostRelogin_5xxErrorsOnlyRetry_StaysRetryable(t *testing.T) {
	c, acc, j := newReloginJourneyClient(t, seededStore(), "pw")
	var hits atomic.Int32
	j.gqlHits = &hits
	j.answerAuthed = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"errors":[{"code":130,"message":"Over capacity"}]}`))
	}

	err := getTweets(t, c)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOverloaded)
	assert.NotErrorIs(t, err, ErrUnexpectedResponse)
	assert.Greater(t, hits.Load(), int32(2), "a 5xx after relogin is retried, not turned into a terminal answer")
	assert.True(t, acc.IsActive())
}

// --- #158.2: account-verdict codes ---

// Mutation: errors.go classifyError — delete `185, 226` from the errBlocked
// case => RED for those rows.
func TestClassifyError_AccountVerdictCodes(t *testing.T) {
	mk := func(code int) []byte {
		return []byte(`{"errors":[{"code":` + strconv.Itoa(code) + `,"message":"m"}]}`)
	}
	for _, code := range []int{161, 185, 226} {
		assert.Equal(t, errBlocked, classifyError(mk(code), nil), "code %d", code)
	}
	// Left unmapped on purpose (see docs/errors.md): their scope is not
	// established, so they must keep today's errors-only handling.
	for _, code := range []int{37, 220, 344} {
		assert.Equal(t, errNone, classifyError(mk(code), nil), "code %d", code)
	}
}

// Journey: the first account answers an errors-only 200 with code 226 (the
// request looked automated for THAT account); the pool benches it and the next
// account serves. Before the code was classified the answer was a terminal
// ErrUnexpectedResponse with no bench and no rotation.
//
// Mutation: errors.go classifyError — delete `226` from the errBlocked case =>
// RED (ErrUnexpectedResponse, account 0 still active).
func TestAccountVerdictCode_BenchesAndRotates(t *testing.T) {
	var hits atomic.Int32
	c, accs := newPoolWaitClient(t, 2)
	srv := startFakeX(t, func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"errors":[{"code":226,"message":"This request looks like it might be automated."}]}`))
			return
		}
		_, _ = w.Write([]byte(userTweetsBody(snowflakeAt(time.Now()))))
	})
	_ = srv
	c.client = srv
	for _, a := range accs {
		a.client = srv
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.GetUserTweets(ctx, "999", 20)
	require.NoError(t, err, "the second account must serve the call")
	assert.EqualValues(t, 2, hits.Load())
	active := 0
	for _, a := range accs {
		if a.IsActive() {
			active++
		}
	}
	assert.Equal(t, 1, active, "the account that answered 226 must be benched")
}

// --- #158.5: untested guards ---

// doPOST loop-top terminalVerdict: a post-relogin errors-only answer is
// terminal, so the loop must not send another request.
//
// Mutation: request.go doPOST — delete the `if ae := terminalVerdict(lastErr);
// ae != nil { return nil, ae }` block at the top of the attempt loop => RED:
// 3 requests instead of 2.
func TestDoPOST_PostReloginTerminal_NoFurtherAttempt(t *testing.T) {
	c, acc, j := newReloginJourneyClient(t, seededStore(), "pw")
	var hits atomic.Int32
	j.gqlHits = &hits
	j.answerAuthed = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(unrecognisedErrorsOnly))
	}
	url := twitterBase + "/x/CreateTweet"

	_, err := c.doPOST(context.Background(), acc, "CreateTweet", url, []byte(`{}`))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnexpectedResponse)
	assert.EqualValues(t, 2, hits.Load(), "initial request + post-relogin retry; the terminal answer stops the loop")
}

// doPoolRequest's terminalVerdict check before the guest fallback: the terminal
// answer arrives on the LAST attempt, so the loop-top check never sees it. The
// call must return it, not fall through to a guest request.
//
// Mutation: request.go doPoolRequest — delete the `if ae := terminalVerdict(
// lastErr); ae != nil { return nil, nil, ae }` block that follows release()
// before "--- Guest token fallback ---" => RED: a guest request is made.
func TestGuestFallback_NotReachedAfterLastAttemptTerminal(t *testing.T) {
	c, _, j := newReloginJourneyClient(t, seededStore(), "pw")
	c.cfg.DisableGuestFallback = false
	c.setGuestToken("cached-guest")
	var guestHits, authedHits atomic.Int32
	j.answerAuthed = func(w http.ResponseWriter, _ *http.Request) {
		authedHits.Add(1)
		_, _ = w.Write([]byte(unrecognisedErrorsOnly))
	}
	var preHits atomic.Int32
	j.pre = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("x-guest-token") != "" {
			guestHits.Add(1)
			_, _ = w.Write([]byte(`{"data":{"user":{}}}`))
			return true
		}
		// The first two attempts end non-terminally (131, no data) so the
		// relogin lands on the third and last attempt.
		if !strings.Contains(r.Header.Get("Cookie"), "auth_token="+reloginNewTok) && preHits.Add(1) <= 2 {
			_, _ = w.Write([]byte(`{"errors":[{"code":131,"message":"internal"}]}`))
			return true
		}
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, err := c.doGET(ctx, "UserByRestId", twitterBase+"/x/UserByRestId")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnexpectedResponse)
	assert.EqualValues(t, 1, authedHits.Load(), "the relogin landed on the last attempt")
	assert.Zero(t, guestHits.Load(), "a terminal answer must not be retried as a guest request")
}
