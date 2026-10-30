package twitter

import (
	"context"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
)

// ClientOption adjusts a ClientConfig before NewClient builds the client.
type ClientOption func(*ClientConfig)

// WithRequestJitter replaces the random delay (default 500ms-2.5s) applied
// before every pool-rotated GET/POST with a uniform delay in [min, max).
// It does not affect the pinned write path (doPOST) or media upload, which keep
// their human pacing.
//
// Intended for latency-critical read polling: the default jitter dominates the
// wall time of a call (median ~1.5s of ~2s). Shrinking it makes request timing
// more regular and therefore easier to fingerprint; callers should randomize
// their own poll schedule instead.
func WithRequestJitter(min, max time.Duration) ClientOption {
	return func(cfg *ClientConfig) {
		cfg.RequestJitter = &stealth.Jitter{Min: min, Max: max}
	}
}

// WithoutRequestJitter removes the pre-request delay entirely; equivalent to
// WithRequestJitter(0, 0). See WithRequestJitter for when this is appropriate
// (latency-critical read polling) and what it costs (more regular request
// timing; randomize the poll schedule yourself).
func WithoutRequestJitter() ClientOption { return WithRequestJitter(0, 0) }

// WithAccountPace overrides the per-account human-pace spacing for this client
// only: consecutive requests of one account are spaced by [min, min+random).
// WithAccountPace(0, 0) disables the pacer. A zero min with a positive random is
// treated as a 1ns floor (a zero AccountPaceMin would otherwise be replaced by
// the 800ms default). Removing the pace makes one account's request rhythm
// more regular; the per-endpoint rate limiter still applies.
func WithAccountPace(min, random time.Duration) ClientOption {
	return func(cfg *ClientConfig) {
		if min <= 0 && random <= 0 {
			cfg.AccountPaceDisabled = true
			return
		}
		cfg.AccountPaceDisabled = false
		if min <= 0 {
			min = time.Nanosecond
		}
		if random < 0 {
			random = 0
		}
		cfg.AccountPaceMin, cfg.AccountPaceRandom = min, random
	}
}

// preRequestJitter sleeps the configured pre-request jitter (nil config =
// stealth.DefaultJitter, read at call time). A non-positive draw does not sleep.
func (c *Client) preRequestJitter(ctx context.Context) error {
	j := stealth.DefaultJitter
	if c.requestJitter != nil {
		j = *c.requestJitter
	}
	d := j.Duration()
	if d <= 0 {
		return nil
	}
	if c.jitterSleep != nil {
		return c.jitterSleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
