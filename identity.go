package twitter

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"

	stealth "github.com/anatolykoptev/go-stealth"
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

// profileForAccountKey picks the account's profile by rendezvous hashing: every
// candidate is scored against the key and the highest score wins. Unlike an
// index into a list, the answer for one key changes only when its own winner is
// removed or a newcomer outscores it, so growing go-stealth's profile list does
// not reshuffle every account.
func profileForAccountKey(key string) stealth.BrowserProfile {
	var (
		best      stealth.BrowserProfile
		bestScore uint64
		found     bool
	)
	for _, p := range identityCandidates() {
		sum := sha256.Sum256([]byte(key + "\x00" + string(p.TLSProfile) + "\x00" + p.UserAgent))
		if score := binary.BigEndian.Uint64(sum[:8]); !found || score > bestScore {
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
