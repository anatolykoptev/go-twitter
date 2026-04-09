// Package slogsample provides sampling utilities for slog handlers.
package slogsample

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// SamplingHandler wraps a slog.Handler and suppresses repeated messages with the
// same sample_key, passing at most one log record per interval per key.
// Suppressed counts are emitted as a "suppressed_count" attribute on the next
// allowed record.
type SamplingHandler struct {
	inner    slog.Handler
	interval time.Duration
	mu       sync.Mutex
	limiters map[string]*samplerEntry
}

type samplerEntry struct {
	nextAllowed time.Time
	suppressed  int64
}

// NewSamplingHandler creates a SamplingHandler that wraps inner and allows at most
// one log record per interval per sample_key attribute value.
func NewSamplingHandler(inner slog.Handler, interval time.Duration) *SamplingHandler {
	return &SamplingHandler{
		inner:    inner,
		interval: interval,
		limiters: make(map[string]*samplerEntry),
	}
}

// Enabled reports whether the inner handler is enabled for the given level.
func (h *SamplingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// WithAttrs returns a new handler that includes the given attributes.
func (h *SamplingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return NewSamplingHandler(h.inner.WithAttrs(attrs), h.interval)
}

// WithGroup returns a new handler that groups subsequent attributes under name.
func (h *SamplingHandler) WithGroup(name string) slog.Handler {
	return NewSamplingHandler(h.inner.WithGroup(name), h.interval)
}

// Handle processes a log record. If the record contains a "sample_key" attribute,
// it is subject to rate-limiting: at most one record per interval per key.
// Suppressed records increment a counter that is emitted on the next allowed record.
func (h *SamplingHandler) Handle(ctx context.Context, r slog.Record) error {
	sampleKey := extractSampleKey(r)
	if sampleKey == "" {
		return h.inner.Handle(ctx, r)
	}

	key := r.Message + "|" + sampleKey

	h.mu.Lock()
	entry, ok := h.limiters[key]
	if !ok {
		entry = &samplerEntry{}
		h.limiters[key] = entry
	}

	now := time.Now()
	if now.Before(entry.nextAllowed) {
		entry.suppressed++
		h.mu.Unlock()
		return nil
	}

	suppressed := entry.suppressed
	entry.suppressed = 0
	entry.nextAllowed = now.Add(h.interval)
	h.mu.Unlock()

	if suppressed > 0 {
		r2 := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
		r.Attrs(func(a slog.Attr) bool {
			r2.AddAttrs(a)
			return true
		})
		r2.AddAttrs(slog.Int64("suppressed_count", suppressed))
		return h.inner.Handle(ctx, r2)
	}

	return h.inner.Handle(ctx, r)
}

// extractSampleKey finds the "sample_key" string attribute in a record.
// Returns "" if not found.
func extractSampleKey(r slog.Record) string {
	var key string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "sample_key" {
			key = a.Value.String()
			return false
		}
		return true
	})
	return key
}
