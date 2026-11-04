package twitter

import (
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"math"
	"strings"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/internal/bundle"
)

// An account presents one browser identity for its whole life: the TLS
// fingerprint of its client, the User-Agent, the Client Hints derived from that
// User-Agent, and the x-xp-forwarded-for value that embeds it. The identity is
// a function of the account key (its username), so it survives restarts and
// does not move when the pool is reordered or resized. A profile or User-Agent
// the caller set on the Account is authoritative and never replaced.

// accountKey is the stable key an account's identity derives from. Usernames
// are matched case-insensitively across the package (AccountByUsername).
func accountKey(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// identityCandidates are the profiles an account without one may be given:
// desktop Chromium. twitterHeaders and twitterHeaderOrder model that family's
// request shape (Client Hints, fetch metadata); a Safari or Firefox TLS
// fingerprint behind those headers would be an inconsistency of its own.
func identityCandidates() []stealth.BrowserProfile {
	var out []stealth.BrowserProfile
	for _, p := range stealth.BuiltinProfiles {
		if !p.Mobile && (p.Browser == "chrome" || p.Browser == "edge") {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return stealth.BuiltinProfiles
	}
	return out
}

// An account without an identity draws each candidate profile with weight
// identityShareWeights[browser/OS] * identityVersionWeights[browser/TLS profile].
//
// identityShareWeights is the desktop share, in percent, per browser/OS: Windows
// the majority, macOS second, Linux small, Edge about a tenth of Windows. The
// numbers are rounded estimates of public desktop-browser and desktop-OS usage
// statistics (StatCounter-style shares), not a measurement of x.com's audience.
var identityShareWeights = map[string]float64{
	"chrome/windows": 63,
	"edge/windows":   10,
	"chrome/macos":   21,
	"edge/macos":     1,
	"chrome/linux":   5,
}

// identityVersionWeights splits a browser's share across its TLS profiles by
// recency, because real users sit overwhelmingly on the newest major or two:
// newest dominant, the previous one small, older near zero. The factors of one
// browser add up to 1, so the OS shares above stay the browser/OS mix. When a
// newer profile is added, give it the dominant factor and shrink the others
// here, in this table; a weight does not depend on the rest of the candidate
// list, which is what lets a removed candidate move only its own accounts.
var identityVersionWeights = map[string]float64{
	"chrome/chrome_156": 0.75,
	"chrome/chrome_152": 0.20,
	"chrome/chrome_150": 0.04,
	"chrome/chrome_146": 0.01,
	"edge/chrome_152":   1,
}

// candidateWeight is the draw weight of one candidate; ok is false when the
// share or the version table does not name it.
func candidateWeight(p stealth.BrowserProfile) (w float64, ok bool) {
	s, ok1 := identityShareWeights[p.Browser+"/"+p.OS]
	v, ok2 := identityVersionWeights[p.Browser+"/"+string(p.TLSProfile)]
	if !ok1 || !ok2 {
		return unlistedShareWeight, false
	}
	return s * v, true
}

// unlistedShareWeight is the weight of a candidate the tables do not name (a
// profile go-stealth added): small, so it is rare until someone sizes it.
// TestIdentityShareWeights_CoverEveryCandidate fails first.
const unlistedShareWeight = 0.1

// profileForAccountKey picks the account's profile by weighted rendezvous
// hashing: every candidate is scored against the key and the highest score
// wins. Unlike an index into a list, the answer for one key changes only when
// its own winner is removed or a newcomer outscores it, so growing go-stealth's
// profile list does not reshuffle every account.
func profileForAccountKey(key string) stealth.BrowserProfile {
	return pickProfile(key, identityCandidates())
}

// unitInterval maps a hash to u strictly inside (0,1): 52 bits, and x+0.5 is
// exact in float64 below 2^52, so even the extreme hashes give 0 < u < 1, ln(u)
// finite and negative, and the score finite.
func unitInterval(h uint64) float64 {
	return (float64(h>>12) + 0.5) / (1 << 52)
}

// pickProfile is profileForAccountKey over an explicit candidate list. The score
// of a candidate is -w/ln(u), with u in (0,1) hashed from the key and the
// candidate, and w its share weight: a candidate wins a fraction w/sum(w) of
// keys, and removing one moves only the keys that had picked it.
func pickProfile(key string, cands []stealth.BrowserProfile) stealth.BrowserProfile {
	var (
		best      stealth.BrowserProfile
		bestScore float64
		found     bool
	)
	for _, p := range cands {
		w, _ := candidateWeight(p)
		// The hash keys on what makes a profile itself (browser, OS, TLS
		// fingerprint), never on its UA text: go-stealth editing a UA string
		// must not move accounts.
		sum := sha256.Sum256([]byte(key + "\x00" + p.Browser + "\x00" + p.OS + "\x00" + string(p.TLSProfile)))
		u := unitInterval(binary.BigEndian.Uint64(sum[:8]))
		if score := -w / math.Log(u); !found || score > bestScore {
			best, bestScore, found = p, score, true
		}
	}
	return best
}

// ensureIdentity completes an account's identity once, before its client is
// built or any request is signed:
//   - no profile and no UA: derive the profile from the account key;
//   - a UA only (caller-set): keep it and pair it with the built-in profile
//     that presents exactly that UA, when there is one;
//   - a profile only: take the UA from the profile.
//
// It never overwrites what the caller set, and is idempotent.
func (a *Account) ensureIdentity() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ensureIdentityLocked()
}

// ensureIdentityLocked is ensureIdentity for a caller already holding a.mu
// (clientForAccount builds the client under it).
func (a *Account) ensureIdentityLocked() {
	switch {
	case a.Profile.TLSProfile == "" && a.UserAgent == "":
		a.Profile = profileForAccountKey(accountKey(a.Username))
		a.UserAgent = a.Profile.UserAgent
	case a.Profile.TLSProfile == "":
		for _, p := range stealth.BuiltinProfiles {
			if p.UserAgent == a.UserAgent {
				a.Profile = p
				return
			}
		}
		if !a.identityWarned {
			a.identityWarned = true
			slog.Warn("twitter: account user agent matches no built-in browser profile; its TLS fingerprint will not match it",
				slog.String("user", a.Username))
		}
	case a.UserAgent == "":
		a.UserAgent = a.Profile.UserAgent
		if a.UserAgent == "" {
			a.UserAgent = stealth.UserAgentForProfile(a.Profile.TLSProfile)
		}
	}
}

// clientUA is the User-Agent a client presents, falling back to the package
// default for a nil client or one that reports none.
func clientUA(bc *stealth.BrowserClient) string {
	if bc != nil {
		if ua := bc.Identity().UserAgent; ua != "" {
			return ua
		}
	}
	return defaultUserAgent
}

// defaultUserAgent and defaultTLSProfile are the identity go-stealth gives a
// client built with no options. They are the fallback where no client exists to
// ask, and the TLS half for a caller-set UA that no built-in profile presents.
var defaultUserAgent, defaultTLSProfile = stealthDefaultIdentity()

// stealthDefaultIdentity reads the identity of a default go-stealth client,
// falling back to the bundled UA if such a client cannot be built.
func stealthDefaultIdentity() (string, stealth.TLSProfile) {
	bc, err := stealth.NewClient()
	if err != nil {
		return bundle.DefaultUserAgent, ""
	}
	defer bc.CloseIdleConnections()
	id := bc.Identity()
	if id.UserAgent == "" {
		return bundle.DefaultUserAgent, id.TLSProfile
	}
	return id.UserAgent, id.TLSProfile
}
