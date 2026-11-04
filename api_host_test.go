package twitter

import (
	"context"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A current identity sends login, guest, settings and DM requests to api.x.com,
// as the web client does; the frozen legacy generation stays on api.twitter.com.
//
// Mutation: wire.go apiHostFor — return rawURL unchanged => RED on the chrome
// rows (and, for the login URLs, apiURL).
func TestAPIHost_CurrentIdentityUsesAPIXCom(t *testing.T) {
	cur := &Account{Username: "cur", AuthToken: "t", CT0: "c", Profile: chromeProfile(t, stealth.ProfileChrome156, "chrome", "macos")}
	old := &Account{Username: "old", AuthToken: "t", CT0: "c", Profile: legacyProfile, UserAgent: legacyProfile.UserAgent}
	for _, tc := range []struct {
		acc  *Account
		want string
	}{{cur, "https://api.x.com"}, {old, "https://api.twitter.com"}} {
		rec := &wireRecorder{}
		c, err := NewClient(ClientConfig{Accounts: []*Account{tc.acc}, DefaultProxy: deadProxy,
			stealthOpts: []stealth.ClientOption{stealth.WithBackend(rec.factory)}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		c.warmDoerOverride = &fakeWarmBackend{}

		require.NoError(t, c.ValidateAccount(context.Background(), tc.acc))
		assert.Equal(t, tc.want+"/1.1/account/settings.json", rec.last().URL, tc.acc.Username)

		_, _, _, _ = c.doRequestWithBody(context.Background(), tc.acc, tc.acc.client, "GET", dmInboxURL, twitterHeaders("t", "c", tc.acc.UserAgent), nil)
		assert.Equal(t, tc.want+"/1.1/dm/inbox_initial_state.json", rec.last().URL, tc.acc.Username)

		_, _, _, _ = c.doRequestWithBody(context.Background(), tc.acc, tc.acc.client, "POST", dmNewURL, twitterHeaders("t", "c", tc.acc.UserAgent), nil)
		assert.Equal(t, tc.want+"/1.1/dm/new2.json", rec.last().URL, tc.acc.Username)

		// The login paths hold only the transport (registered by clientForAccount).
		bc, err := c.clientForAccount(tc.acc)
		require.NoError(t, err)
		_, err = c.getGuestToken(bc)
		require.NoError(t, err)
		assert.Equal(t, tc.want+"/1.1/guest/activate.json", rec.last().URL, tc.acc.Username)
		_, err = c.initLoginFlowFull(bc, "gt")
		require.NoError(t, err)
		assert.Equal(t, tc.want+"/1.1/onboarding/task.json?flow_name=login", rec.last().URL, tc.acc.Username)
	}
}

// A test server standing in for the API root is not rewritten.
func TestAPIHost_OverriddenRootIsKept(t *testing.T) {
	assert.Equal(t, "http://127.0.0.1:9/x", apiHostFor(wireChrome, "http://127.0.0.1:9/x"))
	assert.Equal(t, "https://upload.twitter.com/1.1/media/upload.json", apiHostFor(wireChrome, mediaUploadURL), "upload has no x.com host in the oracle")
}

// The session cookies are found under the x.com origins too (a login through
// api.x.com leaves them there), and a legacy login still reads its own hosts.
//
// Mutation: auth.go sessionCookieURLs — delete the two x.com entries => RED.
func TestSessionCookie_ReadsBothDomains(t *testing.T) {
	rec := &wireRecorder{jar: map[string]string{
		"https://x.com auth_token":       "A",
		"https://api.twitter.com ct0":    "legacyct0",
		"https://twitter.com auth_token": "legacyauth",
		"https://api.x.com ct0":          "newct0",
	}}
	bc, err := stealth.NewClient(stealth.WithBackend(rec.factory))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close() })
	assert.Equal(t, "legacyauth", sessionCookie(bc, "auth_token"), "legacy hosts win when both hold it")
	assert.Equal(t, "legacyct0", sessionCookie(bc, "ct0"))
	delete(rec.jar, "https://twitter.com auth_token")
	delete(rec.jar, "https://api.twitter.com ct0")
	assert.Equal(t, "A", sessionCookie(bc, "auth_token"))
	assert.Equal(t, "newct0", sessionCookie(bc, "ct0"))
	assert.Empty(t, sessionCookie(bc, "missing"))
}
