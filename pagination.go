package twitter

import (
	"context"
	"log/slog"
	"strings"

	"github.com/anatolykoptev/go-twitter/internal/telemetry"
)

// terminalCursorPrefix marks the Bottom cursor X sends when a timeline has
// nothing further: "0|<suffix>", or a bare "0". Following it returns an empty
// page with another "0|" cursor whose suffix has changed, so the repeated-cursor
// guard alone never fires on it.
const terminalCursorPrefix = "0|"

// isTerminalCursor reports whether c is X's end-of-timeline cursor.
func isTerminalCursor(c string) bool {
	return c == "0" || strings.HasPrefix(c, terminalCursorPrefix)
}

// noProgressLimit is how many consecutive pages may add no new item (every item
// already seen, or every user unavailable) before the call gives up.
const noProgressLimit = 5

// pageLoop is the stop policy shared by the cursor-paginated readers. After
// each page the loop stops on the first of:
//
//   - maxCount items collected;
//   - no cursor (the natural end);
//   - an empty page, whatever cursor came with it;
//   - X's terminal cursor ("0|...", or "0"): the page's own items are kept;
//   - a cursor already followed in this call;
//   - noProgressLimit consecutive pages that added no new item (items are
//     deduplicated by id within the call, so a server repeating items under fresh
//     cursors is caught).
//
// A short page is not a stop reason: a list is followed while pages keep adding
// new items, however small they get. Every page either adds an item (at most
// maxCount such pages) or extends the run of stalled ones, so the walk is bounded. A natural end is not an error: the caller
// gets what was collected.
type pageLoop struct {
	maxCount    int
	pages       int
	noProgress  int // consecutive pages that added nothing new
	seenCursors map[string]struct{}
	seenIDs     map[string]struct{}
}

func newPageLoop(maxCount int) *pageLoop {
	return &pageLoop{
		maxCount:    maxCount,
		seenCursors: map[string]struct{}{},
		seenIDs:     map[string]struct{}{},
	}
}

// fresh reports whether id has not been collected earlier in this call, and
// remembers it.
func (l *pageLoop) fresh(id string) bool {
	if _, dup := l.seenIDs[id]; dup {
		return false
	}
	l.seenIDs[id] = struct{}{}
	return true
}

// after is called once per fetched page with the page's entry count, the number
// of new items it added, the total collected so far and the page's Bottom
// cursor. It returns the stop reason, or "" to continue with that cursor.
func (l *pageLoop) after(entries, added, total int, next string) string {
	l.pages++
	if added == 0 {
		l.noProgress++
	} else {
		l.noProgress = 0
	}
	switch {
	case total >= l.maxCount:
		return telemetry.StopMaxCount
	case next == "":
		return telemetry.StopNoCursor
	case entries == 0:
		return telemetry.StopEmptyPage
	case isTerminalCursor(next):
		return telemetry.StopTerminalCursor
	}
	if _, dup := l.seenCursors[next]; dup {
		return telemetry.StopRepeatedCursor
	}
	l.seenCursors[next] = struct{}{}
	if l.noProgress >= noProgressLimit {
		return telemetry.StopPageCap
	}
	return ""
}

// finish records the call's page count and stop reason, and logs the two
// reasons that end a call short of maxCount without a natural end.
func (l *pageLoop) finish(c *Client, op, reason string, collected int) {
	c.metrics.recordPagination(op, l.pages, reason)
	if (reason == telemetry.StopPageCap || reason == telemetry.StopTerminalCursor) && collected < l.maxCount {
		slog.Warn("twitter: paginated read stopped before maxCount",
			"operation", op, "reason", reason, "pages", l.pages,
			"collected", collected, "max_count", l.maxCount)
	}
}

// ctxDone records a call that ended because its context did, before another
// request was made.
func (l *pageLoop) ctxDone(c *Client, op string, collected int) {
	l.finish(c, op, telemetry.StopCtx, collected)
}

// failed records a call whose latest request or parse failed; that request
// counts as a page. A context that ended is reported as ctx, anything else as
// error.
func (l *pageLoop) failed(ctx context.Context, c *Client, op string, collected int) {
	l.pages++
	reason := telemetry.StopError
	if ctx.Err() != nil {
		reason = telemetry.StopCtx
	}
	l.finish(c, op, reason, collected)
}
