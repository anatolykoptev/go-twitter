package twitter

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/social"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// identityProbe records what a *WithSocial read presented: the User-Agent of
// each request that reached the fake x.com, and the TLS profile / UA of the
// per-account client the library built (read before the test swaps in the
// loopback client).
type identityProbe struct {
	mu       sync.Mutex
	uas      []string
	built    []stealth.TLSProfile
	builtUAs []string
}

func (p *identityProbe) handler(inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.uas = append(p.uas, r.Header.Get("User-Agent"))
		p.mu.Unlock()
		inner(w, r)
	}
}

func (p *identityProbe) watchClients(prev func(*Client)) func(*Client) {
	return func(c *Client) {
		p.mu.Lock()
		for _, a := range c.cfg.Accounts {
			if a.client != nil {
				id := a.client.Identity()
				p.built = append(p.built, id.TLSProfile)
				p.builtUAs = append(p.builtUAs, id.UserAgent)
			}
		}
		p.mu.Unlock()
		prev(c)
	}
}

func (p *identityProbe) snapshot() (uas []string, built []stealth.TLSProfile, builtUAs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.uas...), append([]stealth.TLSProfile(nil), p.built...), append([]string(nil), p.builtUAs...)
}

func socialUserHandler(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(socialUserBody))
}

func newIdentityRig(t *testing.T, user string, id *social.Identity) (*socialRig, *identityProbe) {
	t.Helper()
	probe := &identityProbe{}
	r := newSocialRigX(t, probe.handler(socialUserHandler))
	r.leaseUser, r.leaseIdentity = user, id
	testHookSocialClient = probe.watchClients(testHookSocialClient)
	return r, probe
}

func getUser(t *testing.T, r *socialRig) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := GetUserWithSocial(ctx, r.sc, "tesla")
	return err
}

// L1 — journey: the account service states chrome_131; the request that reaches
// X carries that User-Agent and the per-account client was built with that TLS
// profile, not the identity this library derives from the username.
func TestWithSocial_LeaseIdentityPresentedOnTheWire(t *testing.T) {
	want := stealth.BrowserProfile{
		TLSProfile: stealth.ProfileChrome131, Browser: "chrome", OS: "windows",
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	}
	const user = "pinned_acc"
	require.NotEqual(t, want.TLSProfile, profileForAccountKey(user).TLSProfile,
		"fixture: the derived choice must differ, or the test cannot tell the two apart")

	r, probe := newIdentityRig(t, user, &social.Identity{
		Mode: "legacy", TLSProfile: string(want.TLSProfile), UserAgent: want.UserAgent, Browser: want.Browser, OS: want.OS,
	})
	require.NoError(t, getUser(t, r))

	uas, built, builtUAs := probe.snapshot()
	require.NotEmpty(t, uas, "fake x.com saw no request")
	for _, ua := range uas {
		assert.Equal(t, want.UserAgent, ua)
	}
	assert.Equal(t, []stealth.TLSProfile{want.TLSProfile}, built)
	assert.Equal(t, []string{want.UserAgent}, builtUAs)
}

// L2 — journey: values the service derived itself (chrome_150 and a UA this
// build has no profile for) are applied exactly; nothing is completed or
// replaced from this library's own derivation.
func TestWithSocial_LeaseIdentityDerivedValuesAppliedExactly(t *testing.T) {
	const user = "user_a"
	const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.7000.99 Safari/537.36"
	require.NotEqual(t, stealth.ProfileChrome150, profileForAccountKey(user).TLSProfile,
		"fixture: the derived choice must differ")

	r, probe := newIdentityRig(t, user, &social.Identity{
		Mode: "derived", TLSProfile: string(stealth.ProfileChrome150), UserAgent: ua, Browser: "chrome", OS: "windows",
	})
	require.NoError(t, getUser(t, r))

	uas, built, builtUAs := probe.snapshot()
	require.NotEmpty(t, uas)
	for _, got := range uas {
		assert.Equal(t, ua, got)
	}
	assert.Equal(t, []stealth.TLSProfile{stealth.ProfileChrome150}, built)
	assert.Equal(t, []string{ua}, builtUAs)
}

// L3 — a lease without an identity object keeps the derived identity.
func TestWithSocial_NoLeaseIdentityKeepsDerivedIdentity(t *testing.T) {
	const user = "pinned_acc"
	derived := profileForAccountKey(user)
	r, probe := newIdentityRig(t, user, nil)
	require.NoError(t, getUser(t, r))

	uas, built, _ := probe.snapshot()
	require.NotEmpty(t, uas)
	for _, ua := range uas {
		assert.Equal(t, derived.UserAgent, ua)
	}
	assert.Equal(t, []stealth.TLSProfile{derived.TLSProfile}, built)
}

// L4 — a stated TLS profile this build does not know: no request reaches X
// under any identity, the lease is not blamed on the credential (no usage
// report), and the caller sees ErrLeaseIdentity.
func TestWithSocial_UnknownLeaseProfileSendsNothingAndIsNotBlamed(t *testing.T) {
	cases := map[string]*social.Identity{
		"unknown profile": {TLSProfile: "chrome_999", UserAgent: "Mozilla/5.0 pinned"},
		"profile only":    {TLSProfile: string(stealth.ProfileChrome131)},
		"user agent only": {UserAgent: "Mozilla/5.0 pinned"},
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			r, probe := newIdentityRig(t, "pinned_acc", id)
			err := getUser(t, r)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrLeaseIdentity), "got %v", err)
			if id.TLSProfile == "chrome_999" {
				assert.ErrorIs(t, err, stealth.ErrUnknownTLSProfile)
			}

			uas, built, _ := probe.snapshot()
			assert.Empty(t, uas, "a session must not reach X under an identity the service did not state")
			assert.Empty(t, built, "no client may be built for it")
			flushReports(t)
			r.reportsMu.Lock()
			defer r.reportsMu.Unlock()
			assert.Empty(t, r.reports, "an unusable identity is not a verdict on the credential")
			assert.Equal(t, int32(maxSocialRetries), r.acquire.Load(), "every lease is tried, then the call fails")
		})
	}
}

// An empty identity object is the same as none.
func TestWithSocial_EmptyLeaseIdentityIsAbsent(t *testing.T) {
	derived := profileForAccountKey("pinned_acc")
	r, probe := newIdentityRig(t, "pinned_acc", &social.Identity{Mode: ""})
	require.NoError(t, getUser(t, r))
	_, built, _ := probe.snapshot()
	assert.Equal(t, []stealth.TLSProfile{derived.TLSProfile}, built)
}
