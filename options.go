package twitter

import (
	"context"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
)

// ClientOption adjusts a ClientConfig before NewClient builds the client.
type ClientOption func(*ClientConfig)

// WithRequestJitter opts back in to a random delay before every pool-rotated
// GET/POST, drawn uniformly from [min, max). By default a client sends
// immediately: account rotation and quotas belong to the service that leases
// accounts, and the old 500ms-2.5s sleep dominated read latency (median ~1.5s
// of a ~2s call). The per-account pacer (WithAccountPace) is unaffected and
// still spaces consecutive requests of one account. The pinned write path
// (doPOST) and media upload keep their own human pacing regardless.
//
// stealth.DefaultJitter reproduces the previous default:
//
//	twitter.WithRequestJitter(stealth.DefaultJitter.Min, stealth.DefaultJitter.Max)
func WithRequestJitter(min, max time.Duration) ClientOption {
	return func(cfg *ClientConfig) {
		cfg.RequestJitter = &stealth.Jitter{Min: min, Max: max}
	}
}

// WithoutRequestJitter removes the pre-request delay; equivalent to
// WithRequestJitter(0, 0). This is already the default and exists to state the
// intent explicitly (and to override an earlier WithRequestJitter).
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

// preRequestJitter sleeps the pre-request jitter opted into with
// WithRequestJitter. Unset (the default) or a non-positive draw does not sleep.
func (c *Client) preRequestJitter(ctx context.Context) error {
	if c.requestJitter == nil {
		return nil
	}
	d := c.requestJitter.Duration()
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
