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

	"github.com/anatolykoptev/go-stealth/pool"
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

// callRelogin starts n goroutines that each relogin acc after a request failed
// with reloginOldTok, and returns their errors once all have finished. The
// gate holds the leader until n-1 callers are parked on its flight, so the
// outcome does not depend on scheduling.
func callRelogin(t *testing.T, c *Client, acc *Account, gate *blockingGate, n int) []error {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.relogin(context.Background(), acc, reloginOldTok)
		}()
	}
	parked := waitFor(5*time.Second, func() bool { return reloginWaiters(c, acc) == n-1 })
	close(gate.release)
	wg.Wait()
	require.True(t, parked, "the other %d callers never parked on the leader's flight", n-1)
	return errs
}

// N callers whose requests failed with code 32 on the SAME account: exactly one
// login reaches the login server, the gate sees one outcome, and every caller
// ends with the new credentials.
//
// Mutation: auth.go relogin — make the in-flight lookup never match (`if f, ok
// := c.reloginFlights[acc]; ok && false`) => RED: the barrier cannot be met
// (nobody parks) and logins != 1.
func TestRelogin_ConcurrentCode32_SingleLogin(t *testing.T) {
	const n = 6
	c, acc, j := newReloginJourneyClient(t, seededStore(), "pw")
	gate := newBlockingGate()
	c.SetAutoReloginGate(gate)

	for i, err := range callRelogin(t, c, acc, gate, n) {
		assert.NoError(t, err, "caller %d", i)
	}
	tok, _, _ := acc.Credentials()
	assert.Equal(t, reloginNewTok, tok)
	assert.EqualValues(t, 1, j.logins.Load(), "one login request must reach the login server for %d concurrent code-32 callers", n)
	assert.EqualValues(t, 1, gate.allowed.Load(), "the gate is consulted once per flight")
	assert.EqualValues(t, 1, gate.successes.Load(), "the breaker must see ONE outcome, not one per caller")
	assert.Zero(t, gate.failures.Load())
}

// A failed shared relogin is also ONE failure for the breaker, and every
// caller gets the failure.
//
// Mutation: auth.go relogin — as above => RED: the barrier fails, failures != 1.
func TestRelogin_ConcurrentFailure_OneBreakerFailure(t *testing.T) {
	const n = 5
	c, acc, _ := newReloginJourneyClient(t, seededStore(), "") // token-only: the relogin cannot succeed
	gate := newBlockingGate()
	c.SetAutoReloginGate(gate)

	for i, err := range callRelogin(t, c, acc, gate, n) {
		assert.Error(t, err, "caller %d must see the shared failure", i)
	}
	assert.EqualValues(t, 1, gate.failures.Load(), "one flight, one recorded failure")
}

// Waiters of a failed flight must not bench the account again: N callers of
// one failed relogin are one deactivation (one item_deactivated alert). The
// callers run through doPOST, whose retries open further flights, so the
// invariant is one bench per FLIGHT (gate.allowed), never one per caller.
//
// Mutation: auth.go benchFailedRelogin — delete the errReloginShared early
// return => RED: alerts > flights.
func TestRelogin_FailedFlight_WaitersDoNotBench(t *testing.T) {
	const n = 4
	fake := newPoolWaitFake(t)
	fake.status.Store(401)
	fake.body = code32Body
	c, accs := newPoolWaitClient(t, 1)
	var alerts atomic.Int32
	c.pool = pool.New(accs, pool.Config{AlertHook: func(topic string, _ any) {
		if topic == "alerts.item_deactivated" {
			alerts.Add(1)
		}
	}})
	gate := newBlockingGate()
	c.SetAutoReloginGate(gate)

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.doPOST(context.Background(), accs[0], "CreateTweet", fake.srv.URL+"/i/api/graphql/x/CreateTweet", []byte(`{}`))
		}()
	}
	parked := waitFor(5*time.Second, func() bool { return reloginWaiters(c, accs[0]) == n-1 })
	close(gate.release)
	wg.Wait()
	require.True(t, parked, "callers never parked on the first flight")

	flights := gate.allowed.Load()
	require.GreaterOrEqual(t, flights, int32(1))
	assert.EqualValues(t, flights, alerts.Load(), "one deactivation per failed flight, not per caller")
}

// A panic inside the login must not read as success to the waiters.
//
// Mutation: auth.go relogin — create the flight with `err: nil` (drop
// errReloginAborted) => RED: the waiter gets nil.
func TestRelogin_PanickingLeader_WaiterSeesFailure(t *testing.T) {
	c, acc, _ := newReloginJourneyClient(t, seededStore(), "pw")
	gate := &panicGate{blockingGate: newBlockingGate()}
	c.SetAutoReloginGate(gate)

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		defer func() { _ = recover() }()
		_ = c.relogin(context.Background(), acc, reloginOldTok)
	}()
	require.True(t, waitFor(3*time.Second, func() bool { return gate.allowed.Load() == 1 }))
	waiter := make(chan error, 1)
	go func() { waiter <- c.relogin(context.Background(), acc, reloginOldTok) }()
	require.True(t, waitFor(3*time.Second, func() bool { return reloginWaiters(c, acc) == 1 }))
	close(gate.release)
	<-leaderDone
	select {
	case err := <-waiter:
		assert.Error(t, err, "a flight that panicked is not a success")
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never released")
	}
}

type panicGate struct{ *blockingGate }

func (g *panicGate) Allowed(ctx context.Context, u string) (bool, string) {
	g.blockingGate.Allowed(ctx, u)
	panic("login blew up")
}

// A caller whose request failed with a token an earlier relogin already
// replaced must not log in again.
//
// Mutation: auth.go relogin — delete the `cur != failedTok` early return =>
// RED: a second login reaches the server.
func TestRelogin_LateArrivalWithReplacedToken_NoSecondLogin(t *testing.T) {
	c, acc, j := newReloginJourneyClient(t, seededStore(), "pw")

	require.NoError(t, c.relogin(context.Background(), acc, reloginOldTok))
	require.EqualValues(t, 1, j.logins.Load())

	// Request B failed with the OLD token before A's relogin finished.
	require.NoError(t, c.relogin(context.Background(), acc, reloginOldTok))
	assert.EqualValues(t, 1, j.logins.Load(), "the token B failed with is already replaced: no second login")
	tok, _, _ := acc.Credentials()
	assert.Equal(t, reloginNewTok, tok)
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
	go func() { leader <- c.relogin(context.Background(), acc, reloginOldTok) }()
	require.True(t, waitFor(3*time.Second, func() bool { return gate.allowed.Load() == 1 }), "leader never started")

	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { waiter <- c.relogin(ctx, acc, reloginOldTok) }()
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

// --- #158.2: account-specific codes stay unclassified ---

// 185, 220 and 344 are not verdicts we can attribute to the serving account
// (see docs/errors.md), so classifyError leaves them alone. 37 and 226 were
// promoted to account verdicts (errNotAuthorized / errAutomated): see
// automated_test.go.
//
// Mutation: errors.go classifyError — `case 161, 185:` => RED for code 185.
func TestClassifyError_UnattributableCodesStayUnclassified(t *testing.T) {
	for _, code := range []int{185, 220, 344} {
		body := []byte(`{"errors":[{"code":` + strconv.Itoa(code) + `,"message":"m"}]}`)
		assert.Equal(t, errNone, classifyError(body, nil), "code %d", code)
	}
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
