package parse

import (
	"strconv"
	"time"
)

// TwitterEpochMs is the Twitter snowflake epoch (2010-11-04T01:42:54.657Z).
const TwitterEpochMs = 1288834974657

// SnowflakeTime returns the creation time encoded in a Twitter/X snowflake ID
// (a tweet ID): (id >> 22) + 1288834974657 ms. The root package exposes it as
// twitter.SnowflakeTime.
func SnowflakeTime(id string) (time.Time, error) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return time.Time{}, &snowflakeError{id: id, err: err}
	}
	return time.UnixMilli(int64(n>>22) + TwitterEpochMs).UTC(), nil
}

type snowflakeError struct {
	id  string
	err error
}

func (e *snowflakeError) Error() string {
	return "twitter: invalid snowflake id " + strconv.Quote(e.id) + ": " + e.err.Error()
}

func (e *snowflakeError) Unwrap() error { return e.err }
