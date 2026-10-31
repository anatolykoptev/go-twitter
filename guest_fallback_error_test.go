package twitter

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An authenticated request rejected with 404 that then fails the same way over
// the guest fallback must surface the authenticated failure first. Reporting
// only "(guest) HTTP 404" hid that the logged-in request had been rejected.
//
// Mutation: in request.go doPoolRequest's guest `status != 200` arm, set
// apiErr.Message to the bare guest message (drop withAuthFailure) => RED.
func TestGuestFallbackFailure_NamesAuthenticatedRejectionFirst(t *testing.T) {
	const endpoint = "UserByRestId" // guest-eligible (not in requiresAuth): falls back after the pool fails
	fake := newPoolWaitFake(t)
	fake.status.Store(http.StatusNotFound)
	fake.body = `{"errors":[{"message":"Not Found"}]}`

	c, _ := newPoolWaitClient(t, 1)
	c.cfg.DisableGuestFallback = false
	c.setGuestToken("test-guest-token") // skip real guest-token acquisition

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, err := c.doGET(ctx, endpoint, fake.srv.URL+"/i/api/graphql/x/"+endpoint)
	require.Error(t, err)

	msg := err.Error()
	authIdx := strings.Index(msg, endpoint+" HTTP 404")
	guestIdx := strings.Index(msg, "(guest) HTTP 404")
	require.GreaterOrEqual(t, authIdx, 0, "authenticated status+endpoint missing: %q", msg)
	require.GreaterOrEqual(t, guestIdx, 0, "guest failure missing: %q", msg)
	assert.Less(t, authIdx, guestIdx, "authenticated failure must come first: %q", msg)
}

// UserTweetsAndReplies needs a logged-in account: when the pool fails there is
// no guest request at all and the error is the authenticated one.
//
// Mutation: remove "UserTweetsAndReplies" from requiresAuth in request.go => RED
// (the call falls back to the guest token and the error says "(guest)").
func TestUserTweetsAndReplies_RequiresAuth_NoGuestFallback(t *testing.T) {
	const endpoint = "UserTweetsAndReplies"
	require.True(t, requiresAuth(endpoint))

	fake := newPoolWaitFake(t)
	fake.status.Store(http.StatusNotFound)
	fake.body = `{"errors":[{"message":"Not Found"}]}`

	c, accs := newPoolWaitClient(t, 1)
	c.cfg.DisableGuestFallback = false
	c.setGuestToken("test-guest-token")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, err := c.doGET(ctx, endpoint, fake.srv.URL+"/i/api/graphql/x/"+endpoint)
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "requires auth", msg)
	assert.Contains(t, msg, endpoint+" HTTP 404", msg)
	assert.NotContains(t, msg, "(guest)", msg)
	assert.EqualValues(t, len(accs), fake.hits.Load(), "only the authenticated attempt may reach the server")
}
