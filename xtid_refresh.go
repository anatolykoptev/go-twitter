package twitter

import (
	"context"
	"log/slog"
	"net/http"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/xtid"
)

// Result labels of twitter.client.xtid.retries.
const (
	xtidRetryRescued  = "rescued"
	xtidRetryStill404 = "still_404"
)

// xtidRetry is the per-call state of the transaction-id refresh: one refresh
// attempt and at most one re-send per logical request.
type xtidRetry struct {
	attempted bool // the refresh was tried (whatever its result)
	resent    bool // a request was re-sent after a successful refresh and its outcome is not recorded yet
}

// xtidRetryFinished records that the call succeeded after a refresh-triggered
// re-send. Call it from a defer with the call's final error.
func (c *Client) xtidRetryFinished(st *xtidRetry, err error) {
	if st.resent && err == nil {
		st.resent = false
		c.metrics.recordXtidRetry(xtidRetryRescued)
	}
}

// retryAfterXtidRefresh handles a response that a stale transaction id can
// cause: a 404 on a route that exists. Like a browser that reloads its page
// when requests stop working, it rebuilds the id generator from freshly fetched
// pages and reports true when the same request is worth sending once more.
//
// Current-wire accounts only: the legacy wire is frozen in behaviour as well as
// bytes. It runs at most once per logical request (st), covers only the 404
// class, and only rebuilds keys old enough to be stale (see
// xtid.Manager.Refresh — concurrent faults share one rebuild, and a dead
// endpoint cannot turn every call into a page fetch). The endpoint-fault
// bookkeeping of the response is the caller's, unchanged: this adds a refresh
// before it, never instead of it.
func (c *Client) retryAfterXtidRefresh(ctx context.Context, st *xtidRetry, apiErr *APIError, status int,
	acc *Account, bc *stealth.BrowserClient, authTok, ct0, ua string) bool {
	if c.wireFor(acc, bc) != wireChrome || status != http.StatusNotFound || apiErr.Class != errEndpoint {
		return false
	}
	if st.resent { // the re-send after a refresh was answered 404 again
		st.resent = false
		c.metrics.recordXtidRetry(xtidRetryStill404)
	}
	if st.attempted {
		return false
	}
	st.attempted = true
	r := c.qidRefresh()
	res := c.xtidMgr.Refresh(ctx, r.Shared(c.warmFetcherFor(acc, bc, twitterHeaders(authTok, ct0, ua))))
	c.metrics.recordXtidRefresh(string(res))
	if res != xtid.RefreshRefreshed {
		return false
	}
	user := ""
	if acc != nil {
		user = acc.Username
	}
	slog.Info("twitter: 404 on a known route, transaction-id keys refreshed, retrying once",
		slog.String("user", user))
	st.resent = true
	return true
}
