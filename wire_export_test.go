package twitter

import (
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
)

// The exported rule agrees with the internal decision for every built-in
// profile and chrome_131, and the account form follows the stated profile.
//
// Mutation: wire.go WireFor — return WireLegacy unconditionally => RED on the
// chrome profiles; or return WireChrome for a profile outside wireChromeProfiles
// => RED on the legacy ones.
func TestWireFor_AgreesWithTheInternalDecision(t *testing.T) {
	profiles := map[stealth.TLSProfile]bool{stealth.ProfileChrome131: true}
	for _, p := range stealth.BuiltinProfiles {
		profiles[p.TLSProfile] = true
	}
	sawChrome, sawLegacy := false, false
	for p := range profiles {
		want := WireLegacy
		if wireGenFor(p) == wireChrome {
			want = WireChrome
			sawChrome = true
		} else {
			sawLegacy = true
		}
		assert.Equal(t, want, WireFor(p), "%s", p)
		assert.Equal(t, want, (&Account{Profile: stealth.BrowserProfile{TLSProfile: p}}).Wire(), "%s", p)
	}
	assert.True(t, sawChrome && sawLegacy, "the list covers both generations")
	assert.Equal(t, WireLegacy, WireFor(""), "no stated profile is legacy")
}
