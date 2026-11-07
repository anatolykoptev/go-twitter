package twitter

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anatolykoptev/go-twitter/internal/queryid"
)

const captureURL = "https://x.com/i/api/graphql/q/UserByScreenName"

// A current identity's capture carries its own User-Agent and Client Hints and
// the current header order; the frozen legacy identity keeps the legacy order and
// UA. The expected values are the account's stated identity and the two order
// tables, not anything CaptureRequest computes.
//
// Mutation: capture.go CaptureRequest — build probe without Profile/UserAgent
// (`Profile: acc.Profile, UserAgent: acc.UserAgent` removed) => the chrome row's
// UA/order assertions go RED (an unset identity is derived from the username).
func TestCaptureRequest_ReportsTheStatedIdentity(t *testing.T) {
	cur := chromeProfile(t, stealth.ProfileChrome156, "chrome", "macos")
	for _, tc := range []struct {
		name  string
		acc   *Account
		order []string
		ua    string
	}{
		{"current", &Account{Username: "cap-current", Profile: cur, UserAgent: cur.UserAgent}, chromeHeaderOrder, cur.UserAgent},
		{"legacy", &Account{Username: "cap-legacy", Profile: legacyProfile, UserAgent: legacyProfile.UserAgent}, twitterHeaderOrder, legacyProfile.UserAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CaptureRequest(tc.acc, "GET", captureURL)
			require.NoError(t, err)
			assert.Equal(t, tc.ua, got.Headers["user-agent"])
			assert.Equal(t, tc.order, got.Order)
			assert.Equal(t, "captured-ct0", got.Headers["x-csrf-token"], "placeholder credentials, never the caller's")
			assert.Empty(t, tc.acc.AuthToken, "the caller's account is not modified")
			names := got.Names()
			assert.Contains(t, names, "sec-ch-ua")
			// Names lists order-table members in table order.
			last := -1
			for _, n := range names {
				for i, o := range tc.order {
					if o == n {
						assert.Greater(t, i, last, n)
						last = i
					}
				}
			}
		})
	}
}

// An identity-less account is derived from its username, like NewClient does, so
// two captures for one username agree.
func TestCaptureRequest_DerivesAnUnstatedIdentityStably(t *testing.T) {
	a, err := CaptureRequest(&Account{Username: "cap-derived"}, "GET", captureURL)
	require.NoError(t, err)
	b, err := CaptureRequest(&Account{Username: "cap-derived"}, "GET", captureURL)
	require.NoError(t, err)
	assert.NotEmpty(t, a.Headers["user-agent"])
	assert.Equal(t, a.Headers["user-agent"], b.Headers["user-agent"])
}

func TestCaptureRequest_Errors(t *testing.T) {
	_, err := CaptureRequest(nil, "GET", captureURL)
	assert.Error(t, err)
	_, err = CaptureRequest(&Account{Username: "x"}, "GET", "://bad")
	assert.Error(t, err)
}

func TestCapturedRequest_NamesOrderThenSorted(t *testing.T) {
	r := CapturedRequest{
		Headers: map[string]string{"b": "1", "z": "1", "a": "1", "c": "1"},
		Order:   []string{"c", "missing", "b", "c"},
	}
	assert.Equal(t, []string{"c", "b", "a", "z"}, r.Names())
}

// The transaction id is present on both wires, as it is online: the legacy shape
// used to lose it offline because key generation had nothing to fetch.
//
// Mutation: capture.go CaptureRequest — delete `c.warmDoerOverride = captureWarm{}`
// -> key generation fails offline and the legacy row loses the header (RED).
func TestCaptureRequest_BothWiresCarryATransactionID(t *testing.T) {
	cur := chromeProfile(t, stealth.ProfileChrome156, "chrome", "macos")
	for name, acc := range map[string]*Account{
		"current": {Username: "cap-txn-cur", Profile: cur, UserAgent: cur.UserAgent},
		"legacy":  {Username: "cap-txn-old", Profile: legacyProfile, UserAgent: legacyProfile.UserAgent},
	} {
		got, err := CaptureRequest(acc, "GET", captureURL)
		require.NoError(t, err, name)
		assert.Contains(t, got.Names(), "x-client-transaction-id", name)
		assert.NotEqual(t, xtidFailureValue, got.Headers["x-client-transaction-id"], name+": a real id, not the failure value")
	}
}

// A capture touches no network: not even the name lookup the default URL guard
// does before the transport sees a request. The resolver is replaced by one that
// fails the test on any use.
//
// Mutation: capture.go CaptureRequest — remove `stealth.WithoutSSRFGuard()` from
// stealthOpts -> the guard resolves the host through the replaced resolver (RED).
func TestCaptureRequest_NeverTouchesTheNetwork(t *testing.T) {
	var lookups atomic.Int32
	orig := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		lookups.Add(1)
		return nil, errors.New("no network in a capture")
	}}
	t.Cleanup(func() { net.DefaultResolver = orig })

	cur := chromeProfile(t, stealth.ProfileChrome156, "chrome", "macos")
	for _, acc := range []*Account{
		{Username: "cap-net-cur", Profile: cur, UserAgent: cur.UserAgent},
		{Username: "cap-net-old", Profile: legacyProfile, UserAgent: legacyProfile.UserAgent},
	} {
		_, err := CaptureRequest(acc, "GET", captureURL)
		require.NoError(t, err)
	}
	assert.Zero(t, lookups.Load(), "name lookups during a capture")
}

// A capture leaves the process-wide queryId refresher alone: with an enabled one
// installed, no refresh runs, no snapshot is installed and no alert fires.
//
// Mutations: capture.go — delete `c.qid = newQIDRefresher(...)` -> the process
// refresher is driven by the capture (RED); delete the `SetWarmPageHook` no-op ->
// the warm page reaches the process refresher (RED).
func TestCaptureRequest_LeavesTheProcessRefresherAlone(t *testing.T) {
	r := newQIDRefresher(queryid.Config{
		TTL: queryid.RefreshTTL, FaultWindow: queryid.FaultWindow, MinInterval: queryid.MinInterval,
		Timeout: 5 * time.Second, FaultThreshold: queryid.FaultThreshold,
	})
	var alerts atomic.Int32
	r.SetAlertHook(func(string, any) { alerts.Add(1) })
	// The process refresher has seen the old page layout: a page of the new layout
	// reaching it would raise queryid.layout_changed, which makes a leak visible.
	r.ObservePage([]byte(`<html><script src="https://abs.twimg.com/responsive-web/client-web/main.0123456789abcdefa.js"></script></html>`))
	prev, prevSnap := processQIDPtr.Load(), queryid.Active.Load()
	processQIDPtr.Store(r)
	queryid.Active.Store(nil)
	t.Cleanup(func() {
		r.WG.Wait()
		processQIDPtr.Store(prev)
		queryid.Active.Store(prevSnap)
	})

	cur := chromeProfile(t, stealth.ProfileChrome156, "chrome", "macos")
	for _, acc := range []*Account{
		{Username: "cap-qid-cur", Profile: cur, UserAgent: cur.UserAgent},
		{Username: "cap-qid-old", Profile: legacyProfile, UserAgent: legacyProfile.UserAgent},
	} {
		_, err := CaptureRequest(acc, "GET", captureURL)
		require.NoError(t, err)
	}
	r.WG.Wait()
	assert.Zero(t, alerts.Load(), "no alert reached the process refresher's hook")
	assert.Nil(t, queryid.Active.Load(), "no snapshot was installed")
}
