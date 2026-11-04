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
// cap.
type reportSink struct {
	sem     chan struct{}
	wg      sync.WaitGroup
	dropped atomic.Int64
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
	s.wg.Add(1)
	go func() {
		defer func() {
			<-s.sem
			s.wg.Done()
		}()
		fn()
	}()
	return true
}

// wait blocks until every submitted report has finished or ctx is done.
func (s *reportSink) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
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
// process. It returns ctx.Err() when reports are still in flight at the deadline.
func FlushSocialReports(ctx context.Context) error {
	return socialReports.wait(ctx)
}
