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
