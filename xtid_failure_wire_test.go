package twitter

import (
	"context"
	"encoding/base64"
	"io"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refuseWarmDoer answers every warm-page fetch with a permanent 403, so the
// transaction-id generator cannot be built.
type refuseWarmDoer struct{}

func (refuseWarmDoer) DoCtx(context.Context, string, string, map[string]string, io.Reader) ([]byte, map[string]string, int, error) {
	return nil, nil, 403, nil
}

// webClientFailureValue is what x.com's own request filter sends when its
// generator throws: btoa(`e:${error}`) (main bundle module 265390, function a).
// Spelled out here, not derived from the package under test.
const webClientFailureValue = "ZTpUeXBlRXJyb3I6IENhbm5vdCByZWFkIHByb3BlcnRpZXMgb2YgbnVsbCAocmVhZGluZyAnY29udGVudCcp"

// A current-wire request whose generator fails still carries the header, with
// the value the web client sends in that situation.
//
// Mutation: client.go doRequestWithBody, the xtid failure branch — delete the
// `if c.wireFor(acc, bc) == wireChrome { headers[...] = xtidFailureValue }`
// assignment => RED here (header absent).
func TestChromeWire_GeneratorFailureSendsWebClientValue(t *testing.T) {
	p := chromeProfile(t, stealth.ProfileChrome152, "chrome", "macos")
	c, err := NewClient(ClientConfig{DefaultProxy: deadProxy})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.warmDoerOverride = refuseWarmDoer{}
	bc, rec := recorderClient(t, p)
	c.owned.setWire(bc, wireGenFor(p.TLSProfile))

	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET", "https://x.com/i/api/graphql/w/Op", twitterHeaders("AUTH", "CT0", p.UserAgent), nil)

	got := rec.last()
	assert.Equal(t, webClientFailureValue, got.Headers["x-client-transaction-id"])
	raw, decErr := base64.StdEncoding.DecodeString(got.Headers["x-client-transaction-id"])
	require.NoError(t, decErr)
	assert.Equal(t, "e:", string(raw[:2]), "web client form: btoa(`e:${error}`)")
	assert.Contains(t, wireNames(got), "x-client-transaction-id", "the header is on the wire, in its browser position")
	assert.True(t, xtidFailing(c), "the failure is still recorded")
}

// The frozen legacy shape keeps omitting the header when the generator fails.
//
// Mutation: client.go doRequestWithBody, the same branch — drop the wireChrome
// condition so every wire gets the value => RED here (header present).
func TestLegacyFreeze_GeneratorFailureOmitsHeader(t *testing.T) {
	c, bc, rec := legacyClient(t)
	c.warmDoerOverride = refuseWarmDoer{}

	_, _, _, _ = c.doRequestWithBody(context.Background(), nil, bc, "GET", apiGraphQL, twitterHeaders("AUTH", "CT0", legacyProfile.UserAgent), nil)

	got := rec.last()
	_, present := got.Headers["x-client-transaction-id"]
	assert.False(t, present, "legacy wire sends no x-client-transaction-id when the generator fails")
	assert.True(t, xtidFailing(c), "the failure is still recorded")
}

func xtidFailing(c *Client) bool {
	c.xtidFail.mu.Lock()
	defer c.xtidFail.mu.Unlock()
	return c.xtidFail.failing
}
