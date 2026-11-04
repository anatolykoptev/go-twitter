package twitter

import (
	"context"
	"log/slog"
	"net/http"

	stealth "github.com/anatolykoptev/go-stealth"
)

// retryAfterXtidRefresh handles a response that a stale transaction id can
// cause: a 404 on a route that exists. Like a browser that reloads its page
// when requests stop working, it rebuilds the id generator from freshly fetched
// pages and reports true when the same request is worth sending once more.
//
// It runs at most once per logical request (*done), covers only the 404 class,
// and only rebuilds keys old enough to be stale (see xtid.Manager.Refresh —
// concurrent faults share one rebuild, and a dead endpoint cannot turn every
// call into a page fetch). The endpoint-fault bookkeeping of the response is
// the caller's, unchanged: this adds a refresh before it, never instead of it.
func (c *Client) retryAfterXtidRefresh(ctx context.Context, done *bool, apiErr *APIError, status int,
	acc *Account, bc *stealth.BrowserClient, authTok, ct0, ua string) bool {
	if *done || status != http.StatusNotFound || apiErr.Class != errEndpoint {
		return false
	}
	*done = true
	r := c.qidRefresh()
	if !c.xtidMgr.Refresh(ctx, r.Shared(c.warmFetcherFor(acc, bc, twitterHeaders(authTok, ct0, ua)))) {
		return false
	}
	slog.Info("twitter: 404 on a known route, transaction-id keys refreshed, retrying once",
		slog.String("user", acc.Username))
	return true
}
