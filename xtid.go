package twitter

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"sync"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-twitter/xtid"
)

// xtidFailureValue is the x-client-transaction-id the web client sends when
// its generator throws: its request filter wraps id generation in try/catch and
// answers btoa(`e:${error}`), so with the feature switch on a GraphQL XHR
// carries the header even then. The text after "e:" is the thrown error's string
// form. The one used here is derived statically, not observed: the generator
// null-guards its meta read, and its first unguarded dereference is
// `p(qsa(sel))[t[5]%4].childNodes[0].childNodes[1]`, which V8 reports as below
// when the meta or the SVG nodes are missing.
var xtidFailureValue = base64.StdEncoding.EncodeToString(
	[]byte("e:TypeError: Cannot read properties of undefined (reading 'childNodes')"))

// xtidFailureLogInterval rate-limits the warning while a failure streak lasts.
const xtidFailureLogInterval = 5 * time.Minute

// xtidFailureStreak decides when a missing x-client-transaction-id is worth a
// log line: the first failure after a success, then at most one per interval
// while the streak continues. The zero value is ready to use.
type xtidFailureStreak struct {
	mu         sync.Mutex
	failing    bool
	lastLog    time.Time
	suppressed int
}

// onFailure records one failure and reports whether to log it now, with the
// number of failures suppressed since the previous log line of this streak.
func (s *xtidFailureStreak) onFailure(now time.Time) (logNow bool, suppressed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing && now.Sub(s.lastLog) < xtidFailureLogInterval {
		s.suppressed++
		return false, 0
	}
	suppressed = s.suppressed
	s.failing, s.lastLog, s.suppressed = true, now, 0
	return true, suppressed
}

// onSuccess ends the current streak.
func (s *xtidFailureStreak) onSuccess() {
	s.mu.Lock()
	s.failing, s.suppressed = false, 0
	s.mu.Unlock()
}

// noteXtidFailure counts a failed id generation and logs it once per streak
// (see xtidFailureLogInterval).
func (c *Client) noteXtidFailure(err error) {
	c.metrics.recordXtidFailure()
	if logNow, suppressed := c.xtidFail.onFailure(time.Now()); logNow {
		slog.Warn("twitter: x-client-transaction-id unavailable, sending the web client's failure value instead",
			slog.Any("error", err), slog.Int("suppressed_since_last_log", suppressed))
	}
}

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
