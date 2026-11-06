package twitter

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anatolykoptev/go-twitter/internal/gqlextract"
	"github.com/anatolykoptev/go-twitter/internal/xwebnative"
)

// Contract between the real x-web Relay artifact and the request builder of
// every operation in xwebnative.Ops(). The artifact is minimised from a public
// chunk saved at crawl time (internal/gqlextract/testdata/xweb). X answers a
// missing provided variable with HTTP 422, and a rotated build may add one; a
// guard written as a hand list of names cannot see that, the artifact can.
//
// The registry is xwebnative.Ops(): an op added there without a fixture and a
// builder entry below fails this test, and a fixture whose op is not in the
// registry fails it too.

// xwebContracts maps a registry op to the real artifact and to the variables its
// request builder actually sends (the builder's own output, not a copy of it).
var xwebContracts = map[string]struct {
	fixture string
	sent    func() map[string]any
}{
	tweetNotificationsOp: {
		fixture: "tweet-notifications-2026-11-04.js",
		sent:    func() map[string]any { return tweetNotificationsVariables(20) },
	},
}

const relayProviderPrefix = "__relay_internal__pv__"

func TestXWebContract_BuilderMatchesRealArtifact(t *testing.T) {
	registry := xwebnative.Ops()
	require.NotEmpty(t, registry)

	var names []string
	for op := range registry {
		names = append(names, op)
	}
	sort.Strings(names)
	for _, op := range names {
		kind := registry[op]
		c, ok := xwebContracts[op]
		require.True(t, ok, "%s is in xwebnative.Ops() but has no contract fixture/builder here", op)

		body, err := os.ReadFile(filepath.Join("internal", "gqlextract", "testdata", "xweb", c.fixture))
		require.NoError(t, err)
		ops := gqlextract.ParseRelayArtifacts("https://abs.twimg.com/x-web/x-web/assets/"+c.fixture, body)
		require.Len(t, ops, 1, "%s: fixture must hold exactly the one artifact", op)
		art := ops[0]
		require.Equal(t, op, art.Name)
		assert.Equal(t, kind, art.Kind, "%s: operation kind", op)
		assert.Equal(t, gqlextract.StackXWeb, art.Stack)
		assert.NotEmpty(t, art.ID)

		var sentProviders, sentVars []string
		for k := range c.sent() {
			if strings.HasPrefix(k, relayProviderPrefix) {
				sentProviders = append(sentProviders, k)
			} else {
				sentVars = append(sentVars, k)
			}
		}
		slices.Sort(sentProviders)
		wantProviders := slices.Sorted(slices.Values(art.Providers))
		assert.Equal(t, wantProviders, sentProviders,
			"%s: the builder must send exactly the provided variables the artifact declares", op)
		for _, v := range sentVars {
			assert.Contains(t, art.Variables, v,
				"%s: the builder sends variable %q that the artifact does not declare", op, v)
		}
	}

	// No stale fixture: every contract entry names a registry op.
	for op := range xwebContracts {
		_, ok := registry[op]
		assert.True(t, ok, "%s has a contract fixture but is not in xwebnative.Ops()", op)
	}
}
