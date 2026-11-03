package twitter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNotFoundBodiesSurfaceAsErrNotFound drives the real read paths over a fake
// transport. A suspended handle and a deleted tweet must come back as
// ErrNotFound: the pool and go-social treat that as a healthy-account answer
// (no retry, no account fault), so a sentinel that stops matching would make
// both retryable and charge them to the account.
//
// Mutation: in errors.go replace `var ErrNotFound = model.ErrNotFound` with
// `var ErrNotFound = errors.New("twitter: resource not found")` => RED (the
// parsers wrap model.ErrNotFound, which the fresh value no longer matches).
func TestNotFoundBodiesSurfaceAsErrNotFound(t *testing.T) {
	useQIDRefresher(t)
	c, _ := newQIDClient(t, newQIDWorld(t), 1)

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/UserByScreenName") {
			_, _ = rw.Write([]byte(`{"data":{"user":{"result":{"__typename":"UserUnavailable"}}}}`))
			return
		}
		_, _ = rw.Write([]byte(`{"data":{"threaded_conversation_with_injections_v2":{"instructions":[{"type":"TimelineAddEntries","entries":[]}]}}}`))
	}))
	t.Cleanup(srv.Close)
	orig := twitterBase
	twitterBase = srv.URL + "/i/api/graphql"
	t.Cleanup(func() { twitterBase = orig })

	_, err := c.GetUserByScreenName(ctxTimeout(t), "gone")
	require.ErrorIs(t, err, ErrNotFound, "suspended handle")

	_, err = c.GetTweetByID(ctxTimeout(t), "1985000000000000001")
	require.ErrorIs(t, err, ErrNotFound, "deleted tweet")
}
