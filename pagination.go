package twitter

import (
	"context"
	"strings"

	"github.com/anatolykoptev/go-twitter/internal/telemetry"
)

// terminalCursorPrefix marks the Bottom cursor X sends when a timeline has
// nothing further: its value is "0|<id>". Following it returns an empty page
// (or the same cursor again), so the fetchers treat it as the end.
const terminalCursorPrefix = "0|"

// pageLoop is the stop policy shared by the cursor-paginated readers. X ends
// lists with a Bottom cursor that is still non-empty, so "stop on an empty
// cursor" alone pages until the caller's deadline. After each page the loop
// stops on the first of:
//
//   - maxCount items collected;
//   - no cursor (the natural end);
//   - an empty page, whatever cursor came with it;
//   - a cursor already followed in this call;
//   - X's terminal cursor ("0|..."): the page's own items are kept, then it ends;
//   - the page budget, ceil(maxCount/pageSize)+2, where pageSize is the largest
//     page seen (never more than requested), so a server that returns fewer
//     items than asked for is not cut short.
//
// A natural end is not an error: the caller gets what was collected.
type pageLoop struct {
	maxCount    int
	reqSize     int // items requested per page
	pages       int
	largestPage int
	seen        map[string]struct{}
}

func newPageLoop(maxCount, reqSize int) *pageLoop {
	return &pageLoop{maxCount: maxCount, reqSize: max(reqSize, 1), seen: map[string]struct{}{}}
}

// pageCap is the page budget for the count still wanted.
func (l *pageLoop) pageCap() int {
	size := min(l.reqSize, max(l.largestPage, 1))
	return (max(l.maxCount, 0)+size-1)/size + 2
}

// after is called once per fetched page with the page's item count, the total
// collected so far and the page's Bottom cursor. It returns the stop reason, or
// "" to continue with that cursor.
func (l *pageLoop) after(pageItems, total int, next string) string {
	l.pages++
	l.largestPage = max(l.largestPage, pageItems)
	switch {
	case total >= l.maxCount:
		return telemetry.StopMaxCount
	case next == "":
		return telemetry.StopNoCursor
	case pageItems == 0:
		return telemetry.StopEmptyPage
	case strings.HasPrefix(next, terminalCursorPrefix):
		return telemetry.StopTerminalCursor
	}
	if _, dup := l.seen[next]; dup {
		return telemetry.StopRepeatedCursor
	}
	l.seen[next] = struct{}{}
	if l.pages >= l.pageCap() {
		return telemetry.StopPageCap
	}
	return ""
}

// finish records the call's page count and stop reason.
func (l *pageLoop) finish(c *Client, op, reason string) {
	c.metrics.recordPagination(op, l.pages, reason)
}

// ctxDone records a call that ended because its context did, before another
// request was made.
func (l *pageLoop) ctxDone(c *Client, op string) { l.finish(c, op, telemetry.StopCtx) }

// failed records a call whose latest request or parse failed; that request
// counts as a page. A context that ended is reported as ctx, anything else as
// error.
func (l *pageLoop) failed(ctx context.Context, c *Client, op string) {
	l.pages++
	reason := telemetry.StopError
	if ctx.Err() != nil {
		reason = telemetry.StopCtx
	}
	l.finish(c, op, reason)
}
