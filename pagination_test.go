package twitter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/anatolykoptev/go-twitter/internal/telemetry"
)

// Pagination guards for the cursor-paged readers. X can end a list with a
// Bottom cursor that is still non-empty, so a loop that stops only on an empty
// cursor spends requests until the caller's deadline. These tests drive the
// real fetchers over a fake x.com, page by page.

// userPageEntry is one TimelineUser entry.
func userPageEntry(id string) string {
	return fmt.Sprintf(`{"entryId":"user-%[1]s","content":{"entryType":"TimelineTimelineItem","__typename":"TimelineTimelineItem","itemContent":{"__typename":"TimelineUser","user_results":{"result":{"__typename":"User","rest_id":%[1]q,"core":{"name":"n%[1]s","screen_name":"h%[1]s"}}}}}}`, id)
}

// cursorEntry is the Bottom cursor entry.
func cursorEntry(value string) string {
	return fmt.Sprintf(`{"entryId":"cursor-bottom-%s","content":{"entryType":"TimelineTimelineCursor","__typename":"TimelineTimelineCursor","value":%q,"cursorType":"Bottom"}}`, value, value)
}

// userPage builds a Followers/Following/BlueVerifiedFollowers response.
func userPage(cursor string, ids ...string) string {
	var es []string
	for _, id := range ids {
		es = append(es, userPageEntry(id))
	}
	if cursor != "" {
		es = append(es, cursorEntry(cursor))
	}
	return `{"data":{"user":{"result":{"timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[` + strings.Join(es, ",") + `]}]}}}}}}`
}

// retweeterPage builds a Retweeters response.
func retweeterPage(cursor string, ids ...string) string {
	var es []string
	for _, id := range ids {
		es = append(es, userPageEntry(id))
	}
	if cursor != "" {
		es = append(es, cursorEntry(cursor))
	}
	return `{"data":{"retweeters_timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[` + strings.Join(es, ",") + `]}]}}}}`
}

// forbiddenWarm answers every warm-page fetch with a permanent HTTP 403.
type forbiddenWarm struct{}

func (forbiddenWarm) DoCtx(context.Context, string, string, map[string]string, io.Reader) ([]byte, map[string]string, int, error) {
	return nil, nil, http.StatusForbidden, nil
}

// pagedFake serves page(n) for the n-th (0-based) request and counts requests.
type pagedFake struct {
	f    *metricsFixture
	hits atomic.Int32
}

func newPagedFake(t *testing.T, page func(n int, r *http.Request) string) *pagedFake {
	t.Helper()
	p := &pagedFake{}
	p.f = newMetricsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := int(p.hits.Add(1)) - 1
		if n > 200 { // runaway guard: a loop that never stops must fail fast, not hang
			http.Error(w, "runaway", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(page(n, r)))
	}, "pager")
	p.f.c.warmDoerOverride = forbiddenWarm{} // a permanent answer: no ~2.5 s of warm-fetch retries per test
	return p
}

func pagCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func ids(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprint(1850000000000000000+int64(i)))
	}
	return out
}

// timelinePage builds a list-timeline response (GetListTweets).
func timelinePage(cursor string, tweetIDs ...string) string {
	var es []string
	for _, id := range tweetIDs {
		es = append(es, tweetEntry(id))
	}
	if cursor != "" {
		es = append(es, cursorEntry(cursor))
	}
	return `{"data":{"list":{"tweets_timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[` + strings.Join(es, ",") + `]}]}}}}}`
}

// reader is one paginated fetcher under test: how to build its page and how to
// call it.
type reader struct {
	name  string
	op    string
	page  func(cursor string, ids ...string) string
	fetch func(ctx context.Context, c *Client, maxCount int) (int, error)
}

var readers = []reader{
	{"Following", "Following", userPage, func(ctx context.Context, c *Client, n int) (int, error) {
		u, err := c.GetFollowing(ctx, "42", n)
		return len(u), err
	}},
	{"Followers", "Followers", userPage, func(ctx context.Context, c *Client, n int) (int, error) {
		u, err := c.GetFollowers(ctx, "42", n)
		return len(u), err
	}},
	{"VerifiedFollowers", "BlueVerifiedFollowers", userPage, func(ctx context.Context, c *Client, n int) (int, error) {
		u, err := c.GetVerifiedFollowers(ctx, "42", n)
		return len(u), err
	}},
	{"Retweeters", "Retweeters", retweeterPage, func(ctx context.Context, c *Client, n int) (int, error) {
		u, err := c.GetRetweeters(ctx, "555", n)
		return len(u), err
	}},
	{"ListTweets", "ListLatestTweetsTimeline", timelinePage, func(ctx context.Context, c *Client, n int) (int, error) {
		u, err := c.GetListTweets(ctx, "L1", n)
		return len(u), err
	}},
}

// stopCount returns the stops counter total for (op, reason).
func stopCount(t *testing.T, f *metricsFixture, op, reason string) int64 {
	t.Helper()
	m, ok := f.collect(t)[telemetry.MetricPaginationStops]
	if !ok {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	var n int64
	for _, dp := range sum.DataPoints {
		a := attrMap(dp.Attributes)
		if a[telemetry.AttrOperation] == op && a[telemetry.AttrReason] == reason {
			n += dp.Value
		}
	}
	return n
}

// pagesRecorded returns the pages histogram (count, sum) for op.
func pagesRecorded(t *testing.T, f *metricsFixture, op string) (uint64, int64) {
	t.Helper()
	m, ok := f.collect(t)[telemetry.MetricPaginationPages]
	require.True(t, ok, "pagination.pages recorded nothing")
	h, ok := m.Data.(metricdata.Histogram[int64])
	require.True(t, ok)
	for _, dp := range h.DataPoints {
		if attrMap(dp.Attributes)[telemetry.AttrOperation] == op {
			return dp.Count, dp.Sum
		}
	}
	return 0, 0
}

// Every paginated reader shares one stop policy; each scenario runs against all
// of them.

// The production shape: the last page carries zero items and a non-empty Bottom
// cursor. The call ends after that page with what it collected.
func TestPagination_FinalEmptyPageWithCursor_Stops(t *testing.T) {
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			p := newPagedFake(t, func(n int, _ *http.Request) string {
				if n == 0 {
					return r.page("cur-1", ids(1, 3)...)
				}
				return r.page("cur-2") // zero items, cursor still non-empty
			})
			got, err := r.fetch(pagCtx(t), p.f.c, 500)
			require.NoError(t, err)
			assert.Equal(t, 3, got)
			assert.Equal(t, int32(2), p.hits.Load(), "must stop after the empty page")
			assert.Equal(t, int64(1), stopCount(t, p.f, r.op, telemetry.StopEmptyPage))
			pages, sum := pagesRecorded(t, p.f, r.op)
			assert.Equal(t, uint64(1), pages)
			assert.Equal(t, int64(2), sum, "pages per call")
		})
	}
}

// A cursor already followed in this call ends the read, even when the page
// carries items.
func TestPagination_RepeatedCursor_Stops(t *testing.T) {
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			p := newPagedFake(t, func(n int, _ *http.Request) string {
				return r.page("cur-A", ids(n*2+1, n*2+2)...) // same cursor every page
			})
			got, err := r.fetch(pagCtx(t), p.f.c, 500)
			require.NoError(t, err)
			assert.Equal(t, 4, got, "the repeated page's own items are kept")
			assert.Equal(t, int32(2), p.hits.Load())
			assert.Equal(t, int64(1), stopCount(t, p.f, r.op, telemetry.StopRepeatedCursor))
		})
	}
}

// A cursor seen earlier (not just the previous one) also ends the read.
func TestPagination_CursorSeenEarlier_Stops(t *testing.T) {
	cursors := []string{"cur-A", "cur-B", "cur-A"}
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			p := newPagedFake(t, func(n int, _ *http.Request) string {
				return r.page(cursors[n%len(cursors)], ids(n*2+1, n*2+2)...)
			})
			got, err := r.fetch(pagCtx(t), p.f.c, 500)
			require.NoError(t, err)
			assert.Equal(t, 6, got)
			assert.Equal(t, int32(3), p.hits.Load())
			assert.Equal(t, int64(1), stopCount(t, p.f, r.op, telemetry.StopRepeatedCursor))
		})
	}
}

// X's end-of-timeline cursor ("0|...") ends the read after the page that
// carries it; that page's items are kept.
func TestPagination_TerminalCursor_Stops(t *testing.T) {
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			p := newPagedFake(t, func(n int, _ *http.Request) string {
				if n == 0 {
					return r.page("cur-1", ids(1, 2)...)
				}
				return r.page("0|1754000000", ids(3, 4)...)
			})
			got, err := r.fetch(pagCtx(t), p.f.c, 500)
			require.NoError(t, err)
			assert.Equal(t, 4, got)
			assert.Equal(t, int32(2), p.hits.Load())
			assert.Equal(t, int64(1), stopCount(t, p.f, r.op, telemetry.StopTerminalCursor))
		})
	}
}

// Progress, not page size, bounds the walk: pages that shrink after a large
// first page still reach maxCount.
func TestPagination_ShrinkingPages_ReachMaxCount(t *testing.T) {
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			p := newPagedFake(t, func(n int, _ *http.Request) string {
				if n == 0 {
					return r.page("cur-0", ids(1, 6)...)
				}
				return r.page(fmt.Sprintf("cur-%d", n), ids(100+n, 100+n)...) // then 1 new item per page
			})
			got, err := r.fetch(pagCtx(t), p.f.c, 40)
			require.NoError(t, err)
			assert.Equal(t, 40, got)
			assert.Equal(t, int32(35), p.hits.Load())
			assert.Equal(t, int64(1), stopCount(t, p.f, r.op, telemetry.StopMaxCount))
		})
	}
}

// A server repeating the same items under fresh cursors adds nothing new: the
// items are deduplicated and the call gives up after noProgressLimit such pages.
func TestPagination_RepeatedItemsFreshCursors_StopsOnNoProgress(t *testing.T) {
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			p := newPagedFake(t, func(n int, _ *http.Request) string {
				return r.page(fmt.Sprintf("cur-%d", n), ids(1, 2)...)
			})
			got, err := r.fetch(pagCtx(t), p.f.c, 500)
			require.NoError(t, err)
			assert.Equal(t, 2, got, "repeats are collected once")
			assert.Equal(t, int32(1+noProgressLimit), p.hits.Load())
			assert.Equal(t, int64(1), stopCount(t, p.f, r.op, telemetry.StopPageCap))
		})
	}
}

// A bare "0" cursor is terminal too.
func TestPagination_BareZeroCursor_Terminal(t *testing.T) {
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			p := newPagedFake(t, func(n int, _ *http.Request) string { return r.page("0", ids(1, 2)...) })
			got, err := r.fetch(pagCtx(t), p.f.c, 500)
			require.NoError(t, err)
			assert.Equal(t, 2, got)
			assert.Equal(t, int32(1), p.hits.Load())
			assert.Equal(t, int64(1), stopCount(t, p.f, r.op, telemetry.StopTerminalCursor))
		})
	}
}

// A normal list: every page is followed until maxCount is reached, and no page
// is requested after that.
func TestPagination_NormalList_ReturnsAllUpToMaxCount(t *testing.T) {
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			p := newPagedFake(t, func(n int, _ *http.Request) string {
				return r.page(fmt.Sprintf("cur-%d", n), ids(n*2+1, n*2+2)...)
			})
			got, err := r.fetch(pagCtx(t), p.f.c, 6)
			require.NoError(t, err)
			assert.Equal(t, 6, got)
			assert.Equal(t, int32(3), p.hits.Load(), "no request after maxCount is reached")
			assert.Equal(t, int64(1), stopCount(t, p.f, r.op, telemetry.StopMaxCount))
		})
	}
}

// A short list that ends with no cursor returns what it has: a natural end.
func TestPagination_NoCursor_NaturalEnd(t *testing.T) {
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			p := newPagedFake(t, func(n int, _ *http.Request) string {
				if n == 0 {
					return r.page("cur-1", ids(1, 2)...)
				}
				return r.page("", ids(3, 3)...)
			})
			got, err := r.fetch(pagCtx(t), p.f.c, 500)
			require.NoError(t, err)
			assert.Equal(t, 3, got)
			assert.Equal(t, int32(2), p.hits.Load())
			assert.Equal(t, int64(1), stopCount(t, p.f, r.op, telemetry.StopNoCursor))
		})
	}
}

// A server that answers fewer items per page than requested must not be cut
// short by the page budget: the budget follows the largest page actually seen.
func TestPagination_SmallServerPages_NotTruncated(t *testing.T) {
	for _, r := range readers {
		t.Run(r.name, func(t *testing.T) {
			p := newPagedFake(t, func(n int, _ *http.Request) string {
				if n == 9 {
					return r.page("", ids(n+1, n+1)...)
				}
				return r.page(fmt.Sprintf("cur-%d", n), ids(n+1, n+1)...) // 1 item per page
			})
			got, err := r.fetch(pagCtx(t), p.f.c, 10)
			require.NoError(t, err)
			assert.Equal(t, 10, got)
		})
	}
}

// A page whose users are all unavailable (suspended) still carries user
// entries: it is not the end of the list.
func TestPagination_AllUnavailablePage_Continues(t *testing.T) {
	unavailable := `{"entryId":"user-x","content":{"entryType":"TimelineTimelineItem","__typename":"TimelineTimelineItem","itemContent":{"__typename":"TimelineUser","user_results":{"result":{"__typename":"UserUnavailable"}}}}}`
	page1 := `{"data":{"user":{"result":{"timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[` +
		unavailable + `,` + cursorEntry("cur-1") + `]}]}}}}}}`
	p := newPagedFake(t, func(n int, _ *http.Request) string {
		if n == 0 {
			return page1
		}
		return userPage("", ids(1, 2)...)
	})
	users, err := p.f.c.GetFollowing(pagCtx(t), "42", 500)
	require.NoError(t, err)
	assert.Len(t, users, 2)
	assert.Equal(t, int32(2), p.hits.Load())
}

// A request failure ends the call with the error and records the reason.
func TestPagination_RequestError_RecordsError(t *testing.T) {
	p := newPagedFake(t, func(n int, _ *http.Request) string { return `not json` })
	_, err := p.f.c.GetRetweeters(pagCtx(t), "555", 10)
	require.Error(t, err)
	assert.Equal(t, int64(1), stopCount(t, p.f, "Retweeters", telemetry.StopError))
}

// A context that ends mid-call returns the items so far with the context error.
func TestPagination_CtxCancelled_RecordsCtx(t *testing.T) {
	p := newPagedFake(t, func(n int, _ *http.Request) string {
		return userPage(fmt.Sprintf("cur-%d", n), ids(n*2+1, n*2+2)...)
	})
	ctx, cancel := context.WithCancel(pagCtx(t))
	cancel()
	users, err := p.f.c.GetFollowing(ctx, "42", 500)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, users)
	assert.Equal(t, int32(0), p.hits.Load())
	assert.Equal(t, int64(1), stopCount(t, p.f, "Following", telemetry.StopCtx))
}

const unavailableUserEntry = `{"entryId":"user-x","content":{"entryType":"TimelineTimelineItem","__typename":"TimelineTimelineItem","itemContent":{"__typename":"TimelineUser","user_results":{"result":{"__typename":"UserUnavailable"}}}}}`

// Retweeters: a page of only unavailable accounts is not the end of the list.
func TestPagination_Retweeters_AllUnavailablePage_Continues(t *testing.T) {
	page1 := `{"data":{"retweeters_timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[` +
		unavailableUserEntry + `,` + cursorEntry("cur-1") + `]}]}}}}`
	p := newPagedFake(t, func(n int, _ *http.Request) string {
		if n == 0 {
			return page1
		}
		return retweeterPage("", ids(1, 2)...)
	})
	users, err := p.f.c.GetRetweeters(pagCtx(t), "555", 500)
	require.NoError(t, err)
	assert.Len(t, users, 2)
	assert.Equal(t, int32(2), p.hits.Load())
}

// moduleUserPage wraps the users in a TimelineTimelineModule, as X does for
// grouped user lists.
func moduleUserPage(cursor string, ids ...string) string {
	var items []string
	for _, id := range ids {
		items = append(items, fmt.Sprintf(`{"entryId":"user-%[1]s-item","item":{"itemContent":{"__typename":"TimelineUser","user_results":{"result":{"__typename":"User","rest_id":%[1]q,"core":{"name":"n","screen_name":"h%[1]s"}}}}}}`, id))
	}
	mod := `{"entryId":"module-1","content":{"entryType":"TimelineTimelineModule","__typename":"TimelineTimelineModule","items":[` + strings.Join(items, ",") + `]}}`
	return `{"data":{"user":{"result":{"timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[` + mod + `,` + cursorEntry(cursor) + `]}]}}}}}}`
}

// Module-wrapped users are neither dropped nor read as an empty page.
func TestPagination_ModuleWrappedUsers_Collected(t *testing.T) {
	p := newPagedFake(t, func(n int, _ *http.Request) string {
		if n == 0 {
			return moduleUserPage("cur-1", ids(1, 3)...)
		}
		return moduleUserPage("", ids(4, 5)...)
	})
	users, err := p.f.c.GetFollowing(pagCtx(t), "42", 500)
	require.NoError(t, err)
	assert.Len(t, users, 5)
	assert.Equal(t, int32(2), p.hits.Load())
}
