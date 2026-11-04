package twitter

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
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
		w := identityShareWeights[osBrowserKey(p)]
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

// Every candidate has a weight in the table: a profile go-stealth adds for an
// OS or browser the table does not name would otherwise get the fallback
// silently.
//
// Mutation: identity.go identityShareWeights — delete the "edge/macos" entry => RED.
func TestIdentityShareWeights_CoverEveryCandidate(t *testing.T) {
	for _, p := range identityCandidates() {
		_, ok := identityShareWeights[osBrowserKey(p)]
		assert.True(t, ok, "no share weight for %s (%s)", osBrowserKey(p), p.TLSProfile)
	}
	for key := range identityShareWeights {
		found := false
		for _, p := range identityCandidates() {
			found = found || osBrowserKey(p) == key
		}
		assert.True(t, found, "weight %q names no candidate", key)
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
	"alice":  "chrome/windows/chrome_146",
	"bob":    "edge/macos/chrome_152",
	"carol":  "chrome/macos/chrome_146",
	"dave":   "edge/windows/chrome_152",
	"erin":   "chrome/windows/chrome_146",
	"Frank ": "chrome/windows/chrome_150",
}
