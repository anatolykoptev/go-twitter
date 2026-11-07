package twitter

import (
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
