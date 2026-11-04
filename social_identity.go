package twitter

import (
	"errors"
	"fmt"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/social"
)

// ErrLeaseIdentity marks a lease whose stated browser identity cannot be
// presented as stated: it is incomplete, or names a TLS profile this build does
// not know. The lease is skipped rather than served under a different browser.
var ErrLeaseIdentity = errors.New("twitter: leased account's browser identity is unusable")

// applyLeaseIdentity sets the identity an account service stated for a leased
// session (profile and User-Agent together), so the session is presented by the
// browser the service uses and ensureIdentity derives nothing. A lease without
// an identity (nil or empty) is left untouched and keeps the derived identity.
//
// The identity is validated here, at lease time, by the same construction the
// per-account client is built with: a TLS profile go-stealth does not map fails
// with stealth.ErrUnknownTLSProfile. A stated identity must name both a TLS
// profile and a User-Agent; one half alone would be completed from this build's
// own tables, which can differ from the service's.
func applyLeaseIdentity(acc *Account, id *social.Identity) error {
	if id.IsZero() {
		return nil
	}
	if id.TLSProfile == "" || id.UserAgent == "" {
		return fmt.Errorf("%w: tls_profile and user_agent must both be stated", ErrLeaseIdentity)
	}
	profile := stealth.TLSProfile(id.TLSProfile)
	probe, err := stealth.NewClient(stealth.WithProfile(profile))
	if err != nil {
		return fmt.Errorf("%w: tls_profile %q: %w", ErrLeaseIdentity, id.TLSProfile, err)
	}
	probe.CloseIdleConnections()

	acc.mu.Lock()
	defer acc.mu.Unlock()
	acc.Profile = stealth.BrowserProfile{
		TLSProfile: profile,
		UserAgent:  id.UserAgent,
		Browser:    id.Browser,
		OS:         id.OS,
	}
	acc.UserAgent = id.UserAgent
	return nil
}
