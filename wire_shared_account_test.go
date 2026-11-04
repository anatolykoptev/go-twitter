package twitter

import (
	"context"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An Account handed to two Clients carries the transport the second one built.
// The first Client never registered that transport, so a registry lookup would
// call it legacy and send the legacy header shape over a fetch-mode transport.
// With an account on the request the generation comes from the account's stated
// profile, the same input the transport was built from.
//
// Mutation: wire.go wireFor — return c.owned.wireOf(bc) even when acc != nil
// => RED on the order and sec-fetch-mode assertions.
func TestSharedAccount_WireFollowsTheAccountNotTheRegistry(t *testing.T) {
	rec := &wireRecorder{}
	acc := &Account{Username: "shared", AuthToken: "t", CT0: "c"} // identity derived: current
	cfg := ClientConfig{Accounts: []*Account{acc}, DefaultProxy: deadProxy, stealthOpts: []stealth.ClientOption{stealth.WithBackend(rec.factory)}}
	c1, err := NewClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c1.Close() })
	c2, err := NewClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c2.Close() })
	c1.warmDoerOverride = &fakeWarmBackend{}

	bc := acc.client // built by c2
	require.Equal(t, wireLegacy, c1.owned.wireOf(bc), "premise: c1 never registered the transport c2 built")
	require.Equal(t, wireChrome, accountWire(acc))

	_, _, _, _ = c1.doRequestWithBody(context.Background(), acc, bc, "GET", "https://x.com/i/api/graphql/w/UserByScreenName",
		twitterHeaders("t", "c", acc.UserAgent), nil)
	got := rec.last()
	assert.Equal(t, "cors", got.Headers["sec-fetch-mode"])
	assert.Equal(t, chromeHeaderOrder, got.Order)
	assert.NotContains(t, got.Headers, "x-xp-forwarded-for")
}

// The login flows hold only the transport, so a transport another Client built
// for the account is registered when this Client hands it out.
//
// Mutation: client.go clientForAccount — drop the setWire in the acc.client !=
// nil branch => RED.
func TestSharedAccount_ClientForAccountRegistersForeignTransport(t *testing.T) {
	acc := &Account{Username: "shared2", AuthToken: "t", CT0: "c"}
	cfg := ClientConfig{Accounts: []*Account{acc}, DefaultProxy: deadProxy}
	c1, err := NewClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c1.Close() })
	c2, err := NewClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c2.Close() })

	require.Equal(t, wireLegacy, c1.owned.wireOf(acc.client), "premise")
	bc, err := c1.clientForAccount(acc)
	require.NoError(t, err)
	assert.Equal(t, wireChrome, c1.owned.wireOf(bc))
}
