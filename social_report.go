package twitter

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// socialReportMaxInflight caps the usage reports that may be in flight at once.
// A slow credential service must not make report goroutines pile up without
// bound; when the cap is reached a new report is dropped (and counted) rather
// than blocking the caller.
const socialReportMaxInflight = 16

// reportSink runs usage reports in the background under a fixed concurrency
// cap. In-flight accounting is a mutex-guarded counter, not a sync.WaitGroup:
// a WaitGroup forbids Add from a zero counter while a Wait is running, which is
// exactly what a flush at shutdown does against submitters that are still live.
type reportSink struct {
	sem     chan struct{}
	dropped atomic.Int64

	mu      sync.Mutex
	n       int             // reports submitted and not yet finished
	waiters []chan struct{} // closed when n reaches zero
}

func newReportSink(limit int) *reportSink {
	return &reportSink{sem: make(chan struct{}, limit)}
}

// submit runs fn on its own goroutine unless the cap is reached, in which case
// it returns false and fn is not run.
func (s *reportSink) submit(fn func()) bool {
	select {
	case s.sem <- struct{}{}:
	default:
		if n := s.dropped.Add(1); n == 1 || n%100 == 0 {
			slog.Warn("twitter: usage report dropped, too many in flight",
				slog.Int("max_inflight", cap(s.sem)), slog.Int64("dropped_total", n))
		}
		return false
	}
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	go func() {
		defer s.done()
		fn()
	}()
	return true
}

func (s *reportSink) done() {
	<-s.sem
	s.mu.Lock()
	s.n--
	if s.n == 0 {
		for _, w := range s.waiters {
			close(w)
		}
		s.waiters = nil
	}
	s.mu.Unlock()
}

// wait blocks until the in-flight count reaches zero or ctx is done. Reports
// submitted while it waits may or may not be waited for: it returns at the first
// instant nothing is in flight, so under a continuous stream of submitters it
// can run until ctx expires.
func (s *reportSink) wait(ctx context.Context) error {
	s.mu.Lock()
	if s.n == 0 {
		s.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	s.waiters = append(s.waiters, ch)
	s.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// socialReports carries the usage reports of the *WithSocial helpers.
var socialReports = newReportSink(socialReportMaxInflight)

// FlushSocialReports waits until the usage reports that *WithSocial calls have
// queued in the background are delivered, or ctx is done. The helpers return as
// soon as their result is ready and send the report afterwards; call this at
// shutdown (with a short deadline) so a final report is not lost with the
// process. Reports submitted while it waits may or may not be waited for; it
// returns once nothing is in flight. It returns ctx.Err() when reports are still in flight at the deadline.
func FlushSocialReports(ctx context.Context) error {
	return socialReports.wait(ctx)
}
