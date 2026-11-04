package parse

import "errors"

// Reasons a timeline entry is dropped instead of becoming a tweet. The set is
// small and fixed: it is the label of a metric, so it must stay low-cardinality
// (docs/metrics.md lists them).
const (
	// DropNoID: a tweet result without a rest_id (after unwrapping wrappers).
	DropNoID = "no_id"
	// DropNoCreatedAt: a tweet result with an id but no creation time from any
	// source (legacy.created_at, details.created_at_ms, the snowflake id).
	DropNoCreatedAt = "no_created_at"
	// DropUnhandledType: a timeline item whose __typename the parser does not
	// know (a new X entry kind).
	DropUnhandledType = "unhandled_type"
	// DropTombstone: a deleted or unavailable tweet (TweetTombstone and the like).
	DropTombstone = "tombstone"
	// DropBlankText is NOT a drop: the tweet is still returned. It marks a
	// tweet whose text is empty in every location and that carries no media,
	// card, quote or retweet to explain it, i.e. a likely blanked parse.
	DropBlankText = "blank_text"
)

// Option tunes a parse call.
type Option func(*options)

type options struct {
	obs Observer
}

// Observer receives the parser's quality signals. Both callbacks are optional,
// run on the parsing goroutine and must not block.
type Observer struct {
	// Drop is called once per timeline entry dropped instead of returned, with
	// one of the Drop* reasons (DropBlankText is reported although the tweet is
	// kept).
	Drop func(reason string)
	// CreatedAtFallback is called once per tweet dated from its snowflake id
	// because no created_at field carried a time. The tweet is kept.
	CreatedAtFallback func()
}

func newOptions(opts []Option) options {
	var o options
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	return o
}

// drop reports one dropped entry to the observer, if any.
func (o options) drop(reason string) {
	if o.obs.Drop != nil {
		o.obs.Drop(reason)
	}
}

// createdAtFallback reports one snowflake-dated tweet to the observer, if any.
func (o options) createdAtFallback() {
	if o.obs.CreatedAtFallback != nil {
		o.obs.CreatedAtFallback()
	}
}

// WithObserver registers obs for the parse call.
func WithObserver(obs Observer) Option {
	return func(o *options) { o.obs = obs }
}

// dropError is the error parseTweetResult returns for a result it cannot turn
// into a tweet; it carries the reason the drop is counted under.
type dropError struct {
	reason string
	err    error
}

func (e *dropError) Error() string { return e.err.Error() }
func (e *dropError) Unwrap() error { return e.err }

// dropReason extracts the Drop* reason of err, "" when err is not a dropError.
func dropReason(err error) string {
	var de *dropError
	if errors.As(err, &de) {
		return de.reason
	}
	return ""
}
