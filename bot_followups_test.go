package twitter

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Follow-ups to the review findings left on #163 and #167. Every test names the
// production line whose mutation turns it RED.

// orderGate is an AutoReloginGate that records whether a login started before
// the previous flight had finished.
type orderGate struct {
	flightDone atomic.Bool  // set when the first flight is about to complete
	allowed    atomic.Int32 // logins started
	early      atomic.Int32 // logins started while the first flight was still open
}

func (g *orderGate) Allowed(context.Context, string) (bool, string) {
	if g.allowed.Add(1) >= 2 && !g.flightDone.Load() {
		g.early.Add(1)
	}
	return false, "test"
}
func (*orderGate) RecordFailure(string) {}
func (*orderGate) RecordSuccess(string) {}

// A caller that arrives while a flight is completing must not start a second
// login beside it: two flights could write the account's credentials and the
// stored session out of order. The flight leaves the map and signals done as
// one step under reloginMu, so the late caller either joins or finds it over.
//
// Mutation: auth.go relogin deferred cleanup — move c.reloginMu.Unlock() above
// the reloginWindowHook call (close(f.done) outside the lock) => RED: early == 1.
func TestRelogin_LateArrivalDuringCompletion_NoOverlappingLogin(t *testing.T) {
	c, acc, _ := newReloginJourneyClient(t, seededStore(), "") // token-only: the flight fails fast
	gate := &orderGate{}
	c.SetAutoReloginGate(gate)

	var late sync.WaitGroup
	var stalled atomic.Bool
	c.reloginWindowHook = func() {
		if !stalled.CompareAndSwap(false, true) {
			return // only the first flight's completion is stalled
		}
		late.Add(1)
		go func() {
			defer late.Done()
			_ = c.relogin(context.Background(), acc, "") // no stale-token skip: it must decide on the flight alone
		}()
		// Give the late caller time to reach the flight check.
		waitFor(200*time.Millisecond, func() bool { return gate.allowed.Load() >= 2 })
		gate.flightDone.Store(true)
	}

	require.Error(t, c.relogin(context.Background(), acc, ""))
	late.Wait()
	assert.Zero(t, gate.early.Load(), "a second login started before the first flight was done")
}

// --- #163: a stale CSRF token must not survive a failed save ---

// The relogin kept the auth token but refreshed ct0; the save then timed out
// without writing. The store still holds (token, old ct0): reading back only the
// token would keep that pair, and a restart would reload a rejected ct0.
//
// Mutation: session_store.go dropStaleSession — drop the `&& ct0 == acc.CT0`
// comparison (back to `tok == acc.AuthToken || tok == ""`) => RED: no delete.
func TestDropStaleSession_SameTokenOldCT0_IsDropped(t *testing.T) {
	rec := newRecordingStore()
	rec.data["u1"] = Session{AuthToken: "tokA", CT0: "ct0OLD", SavedAt: time.Now()}
	c := &Client{cfg: ClientConfig{SessionStore: rec}}
	acc := &Account{Username: "u1", AuthToken: "tokA", CT0: "ct0NEW"}

	c.dropStaleSession(acc, context.DeadlineExceeded)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	_, stillThere := rec.data["u1"]
	assert.False(t, stillThere, "the stored ct0 is not the live one; the entry must be dropped")
}

// The other side: a complete match is kept (the write landed).
func TestDropStaleSession_FullMatch_IsKept(t *testing.T) {
	rec := newRecordingStore()
	rec.data["u1"] = Session{AuthToken: "tokA", CT0: "ct0NEW", SavedAt: time.Now()}
	c := &Client{cfg: ClientConfig{SessionStore: rec}}
	acc := &Account{Username: "u1", AuthToken: "tokA", CT0: "ct0NEW"}

	c.dropStaleSession(acc, context.DeadlineExceeded)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Contains(t, rec.data, "u1")
	assert.Empty(t, rec.deletes)
}

// --- #163: a post-relogin 400/214 is terminal ---

// Mutation: request.go terminalVerdict — drop `|| ae.Class == errBadRequest`
// => RED: the pool attempts again (hits > 2).
func TestPostRelogin_BadRequest214_IsTerminal(t *testing.T) {
	c, _, j := newReloginJourneyClient(t, seededStore(), "pw")
	var hits atomic.Int32
	j.gqlHits = &hits
	j.answerAuthed = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"code":214,"message":"Bad Request: invalid cursor"}]}`))
	}

	err := getTweets(t, c)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBadRequest), "got %v", err)
	assert.EqualValues(t, 2, hits.Load(), "initial request + the post-relogin retry; the caller's bad input is not replayed")
}

// --- #167: an open account keeps the identity its session was minted with ---

// The welcome flow runs on a client with go-stealth's default identity; the
// account it returns must carry that identity, or its first pooled request
// derives another profile from the username and the session changes browser.
//
// Mutation: auth.go openAccountOn — delete the `Profile: id.BrowserProfile,
// UserAgent: id.UserAgent` fields from the returned Account => RED: the
// account derives the username's profile, which differs from the login client's.
func TestOpenAccount_CarriesLoginClientIdentity(t *testing.T) {
	// A guest token whose username derives a profile other than the default
	// client's, so inheriting nothing is observable.
	defaultTLS := defaultTLSProfile
	tok := ""
	for i := range 500 {
		cand := "t" + strconv.Itoa(1000000+i)
		if profileForAccountKey(accountKey("guest_"+cand[:8])).TLSProfile != defaultTLS {
			tok = cand
			break
		}
	}
	require.NotEmpty(t, tok, "no guest token derives a profile other than the default")

	j := &reloginJourney{guestTok: tok}
	bc := proxyHopFakeX(t, j.serve)
	c := &Client{}

	acc, err := c.openAccountOn(bc)
	require.NoError(t, err)
	acc.ensureIdentity() // what the first pooled request does

	id := bc.Identity()
	assert.Equal(t, id.TLSProfile, acc.Profile.TLSProfile, "profile must be the one the session was minted with")
	assert.Equal(t, id.UserAgent, acc.UserAgent, "User-Agent must be the one the session was minted with")
}
