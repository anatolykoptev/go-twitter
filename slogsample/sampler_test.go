package slogsample_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	tlog "github.com/anatolykoptev/go-twitter/slogsample"
)

// captureHandler records all Handle calls.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler         { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler              { return h }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *captureHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

func (h *captureHandler) last() slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.records[len(h.records)-1]
}

// TestSamplingHandler_SuppressesRepeated verifies that 100 calls with the same
// sample_key produce exactly 1 output within one interval.
func TestSamplingHandler_SuppressesRepeated(t *testing.T) {
	inner := &captureHandler{}
	h := tlog.NewSamplingHandler(inner, time.Minute)
	ctx := context.Background()

	for range 100 {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "noisy message", 0)
		r.AddAttrs(slog.String("sample_key", "test_key"))
		if err := h.Handle(ctx, r); err != nil {
			t.Fatalf("Handle error: %v", err)
		}
	}

	if got := inner.count(); got != 1 {
		t.Fatalf("expected 1 output, got %d", got)
	}
}

// TestSamplingHandler_SecondPeriod verifies that after the interval elapses,
// a second record is emitted with suppressed_count=99.
func TestSamplingHandler_SecondPeriod(t *testing.T) {
	inner := &captureHandler{}
	// Use a very short interval so we can advance past it
	interval := 10 * time.Millisecond
	h := tlog.NewSamplingHandler(inner, interval)
	ctx := context.Background()

	// First period: 100 calls → 1 output (first call allowed, 99 suppressed)
	for range 100 {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "noisy message", 0)
		r.AddAttrs(slog.String("sample_key", "test_key"))
		_ = h.Handle(ctx, r)
	}

	if got := inner.count(); got != 1 {
		t.Fatalf("first period: expected 1 output, got %d", got)
	}

	// Wait for interval to elapse
	time.Sleep(interval + 5*time.Millisecond)

	// Second period: one call should emit with suppressed_count=99
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "noisy message", 0)
	r.AddAttrs(slog.String("sample_key", "test_key"))
	if err := h.Handle(ctx, r); err != nil {
		t.Fatalf("Handle error: %v", err)
	}

	if got := inner.count(); got != 2 {
		t.Fatalf("second period: expected 2 total outputs, got %d", got)
	}

	last := inner.last()
	var suppressed int64
	last.Attrs(func(a slog.Attr) bool {
		if a.Key == "suppressed_count" {
			suppressed = a.Value.Int64()
		}
		return true
	})

	if suppressed != 99 {
		t.Fatalf("expected suppressed_count=99, got %d", suppressed)
	}
}

// TestSamplingHandler_NoSampleKey verifies that records without sample_key pass through unfiltered.
func TestSamplingHandler_NoSampleKey(t *testing.T) {
	inner := &captureHandler{}
	h := tlog.NewSamplingHandler(inner, time.Minute)
	ctx := context.Background()

	for range 10 {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "plain message", 0)
		_ = h.Handle(ctx, r)
	}

	if got := inner.count(); got != 10 {
		t.Fatalf("expected 10 outputs for records without sample_key, got %d", got)
	}
}
