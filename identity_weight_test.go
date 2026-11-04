package twitter

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func syntheticUsernames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("synthetic_user_%05d", i)
	}
	return out
}

func osBrowserKey(p stealth.BrowserProfile) string { return p.Browser + "/" + p.OS }

// Derived identities follow the desktop share table: over 10 000 usernames the
// share per browser/OS lands within 2 points of its weight, so Linux stays rare
// and Windows is the majority.
//
// Mutation: identity.go profileForAccountKey — score with weight 1 for every
// candidate (the previous equal-weight choice) => RED (Linux gets ~17%).
func TestProfileForAccountKey_FollowsDesktopShare(t *testing.T) {
	const n = 10000
	count := map[string]int{}
	for _, u := range syntheticUsernames(n) {
		count[osBrowserKey(profileForAccountKey(accountKey(u)))]++
	}
	// Expected share of a browser/OS: its candidates' weights over all weights.
	var total float64
	perKey := map[string]float64{}
	for _, p := range identityCandidates() {
		w, _ := candidateWeight(p)
		perKey[osBrowserKey(p)] += w
		total += w
	}
	for key, w := range perKey {
		want, got := 100*w/total, 100*float64(count[key])/n
		assert.InDelta(t, want, got, 2.0, "%s: want ~%.1f%%, got %.1f%%", key, want, got)
	}
	assert.Less(t, 100*float64(count["chrome/linux"])/n, 8.0, "Linux is rare on x.com")
	assert.Greater(t, 100*float64(count["chrome/windows"]+count["edge/windows"])/n, 60.0, "Windows is the majority")
}

// Every candidate has a weight in both tables: a profile go-stealth adds for an
// OS, browser or TLS version the tables do not name would otherwise get the
// fallback silently.
//
// Mutation: identity.go identityShareWeights — delete the "edge/macos" entry => RED.
func TestIdentityShareWeights_CoverEveryCandidate(t *testing.T) {
	for _, p := range identityCandidates() {
		_, ok := candidateWeight(p)
		assert.True(t, ok, "no weight for %s (%s)", osBrowserKey(p), p.TLSProfile)
	}
	for key := range identityShareWeights {
		found := false
		for _, p := range identityCandidates() {
			found = found || osBrowserKey(p) == key
		}
		assert.True(t, found, "share weight %q names no candidate", key)
	}
	// A browser's version factors add up to 1, so the OS shares stay the mix.
	sum := map[string]float64{}
	for key, v := range identityVersionWeights {
		b, _, _ := strings.Cut(key, "/")
		sum[b] += v
	}
	for b, s := range sum {
		assert.InDelta(t, 1.0, s, 1e-9, "%s version factors", b)
	}
}

// Versions are weighted by recency: among Chrome accounts, the newest profile
// dominates, the previous one is small and older ones are near zero.
//
// Mutation: identity.go identityVersionWeights — make the three Chrome factors
// equal (1/3 each) => RED.
func TestProfileForAccountKey_NewestVersionDominates(t *testing.T) {
	const n = 10000
	by := map[stealth.TLSProfile]int{}
	chrome := 0
	for _, u := range syntheticUsernames(n) {
		p := profileForAccountKey(accountKey(u))
		if p.Browser == "chrome" {
			by[p.TLSProfile]++
			chrome++
		}
	}
	share := func(p stealth.TLSProfile) float64 { return float64(by[p]) / float64(chrome) }
	assert.InDelta(t, 0.85, share(stealth.ProfileChrome152), 0.03)
	assert.InDelta(t, 0.12, share(stealth.ProfileChrome150), 0.03)
	assert.Less(t, share(stealth.ProfileChrome146), 0.06)
}

// The hash-to-unit-interval map never reaches 0 or 1, so a score is always
// finite, even for the extreme hashes.
//
// Mutation: identity.go unitInterval — use (x+0.5)/2^53 over 53 bits (the old
// form, which rounds to 1.0 for the largest hash) => RED.
func TestUnitInterval_StrictlyInsideZeroOne(t *testing.T) {
	for _, h := range []uint64{0, 1, 1 << 11, 1<<63 - 1, 1 << 63, ^uint64(0) - 1, ^uint64(0)} {
		u := unitInterval(h)
		assert.Greater(t, u, 0.0, "h=%x", h)
		assert.Less(t, u, 1.0, "h=%x", h)
		assert.False(t, math.IsInf(-1/math.Log(u), 0), "h=%x", h)
	}
}

// The identity of an account is a pure function of its key: the same names give
// the same profiles on every run. The sample is pinned, so a change to the
// scoring (which would move live accounts) fails here.
//
// Mutation: identity.go profileForAccountKey — hash the UA text instead of
// browser/OS/TLS profile, or change the hash input => RED (pinned picks move).
func TestProfileForAccountKey_Stable(t *testing.T) {
	want := map[string]string{
		"alice": "", "bob": "", "carol": "", "dave": "", "erin": "", "Frank ": "",
	}
	for u := range want {
		a := profileForAccountKey(accountKey(u))
		b := profileForAccountKey(accountKey(u))
		assert.Equal(t, a, b)
		want[u] = fmt.Sprintf("%s/%s/%s", a.Browser, a.OS, a.TLSProfile)
	}
	assert.Equal(t, pinnedPicks, want)
}

// Minimal movement (the rendezvous property): removing one candidate moves only
// the accounts that had picked it.
//
// Mutation: identity.go pickProfile — compute u from the candidate's index in
// the list instead of its identity => RED (removing the first candidate moves
// everyone).
func TestPickProfile_RemovingACandidateMovesOnlyItsAccounts(t *testing.T) {
	cands := identityCandidates()
	require.Greater(t, len(cands), 3)
	for _, drop := range []int{0, len(cands) / 2, len(cands) - 1} {
		rest := append(append([]stealth.BrowserProfile(nil), cands[:drop]...), cands[drop+1:]...)
		moved := 0
		for _, u := range syntheticUsernames(3000) {
			before := pickProfile(u, cands)
			after := pickProfile(u, rest)
			if before != cands[drop] {
				assert.Equal(t, before, after, "%s: an account whose pick stayed must not move", u)
			} else {
				moved++
			}
		}
		assert.Positive(t, moved, "dropped candidate %d was picked by nobody", drop)
	}
}

// previousProfileForAccountKey is v0.8.6's choice (every candidate equally
// likely), kept to measure how many derived identities the weighting moves.
func previousProfileForAccountKey(key string) stealth.BrowserProfile {
	var (
		best      stealth.BrowserProfile
		bestScore uint64
		found     bool
	)
	for _, p := range identityCandidates() {
		sum := sha256.Sum256([]byte(key + "\x00" + p.Browser + "\x00" + p.OS + "\x00" + string(p.TLSProfile)))
		if score := binary.BigEndian.Uint64(sum[:8]); !found || score > bestScore {
			best, bestScore, found = p, score, true
		}
	}
	return best
}

// Not an assertion on policy: it records how many derived identities the
// weighting changes, for the change description (go test -run Moved -v).
func TestProfileForAccountKey_MovedByWeighting(t *testing.T) {
	const n = 10000
	moved := 0
	for _, u := range syntheticUsernames(n) {
		k := accountKey(u)
		if previousProfileForAccountKey(k) != profileForAccountKey(k) {
			moved++
		}
	}
	t.Logf("derived identities changed by the weighting: %d of %d (%.1f%%)", moved, n, 100*float64(moved)/n)
	assert.False(t, math.IsNaN(float64(moved)))
}

// pinnedPicks are the identities picked for a fixed sample of names, recorded
// when the weighting was introduced.
var pinnedPicks = map[string]string{
	"alice":  "chrome/macos/chrome_152",
	"bob":    "chrome/windows/chrome_152",
	"carol":  "chrome/windows/chrome_152",
	"dave":   "edge/windows/chrome_152",
	"erin":   "chrome/windows/chrome_152",
	"Frank ": "chrome/windows/chrome_150",
}
