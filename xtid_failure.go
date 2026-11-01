package twitter

import (
	"log/slog"
	"sync"
	"time"
)

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
		slog.Warn("twitter: x-client-transaction-id unavailable, sending requests without it",
			slog.Any("error", err), slog.Int("suppressed_since_last_log", suppressed))
	}
}
