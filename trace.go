package twitter

import (
	"context"
	"sync"
	"time"
)

// RequestTrace is an opt-in collector of per-phase timestamps for the
// requests issued under a context (see WithRequestTrace). It exists for
// latency diagnosis: nothing is recorded, and the request path pays only a
// nil check, unless a caller attaches one. Read the calls only after the
// traced client method has returned.
type RequestTrace struct {
	mu    sync.Mutex
	calls []*CallTrace
}

// CallTrace is the timeline of one logical request (one doGET/doPOST,
// including its internal retries). Phases are wall-clock instants; a zero
// value means the phase was not reached. Entry and JitterDone are first-write;
// the rest describe the LAST HTTP attempt, so a retried request still has a
// coherent final-attempt timeline (Attempts says how many there were).
type CallTrace struct {
	Endpoint string
	Entry    time.Time // doPoolRequest entered
	// JitterDone is after the opt-in pre-request jitter (WithRequestJitter);
	// it equals Entry's instant when no jitter is configured.
	JitterDone time.Time
	// Picked is after the pool handed out an account (includes any pool wait).
	Picked time.Time
	// Paced is after the per-account human-pace wait (accountPacer).
	Paced time.Time
	// HeadersStart/Ready bracket x-client-transaction-id + x-xp-forwarded-for
	// generation; Ready is the instant before the transport is called.
	HeadersStart time.Time
	Ready        time.Time
	// Done is when the transport returned the full response body.
	Done     time.Time
	Attempts int
	Status   int
	Bytes    int
	Proxied  bool // the serving account has its own proxy

	// rt is the owning RequestTrace: its mutex guards every field above, so a
	// concurrent Calls() reads a consistent snapshot.
	rt *RequestTrace
}

type requestTraceKey struct{}

// WithRequestTrace returns a context that makes client calls record their
// phase timeline into t.
func WithRequestTrace(ctx context.Context, t *RequestTrace) context.Context {
	return context.WithValue(ctx, requestTraceKey{}, t)
}

// Calls returns the traced requests in issue order.
func (t *RequestTrace) Calls() []CallTrace {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]CallTrace, len(t.calls))
	for i, c := range t.calls {
		out[i] = *c
	}
	return out
}

// beginCall opens a CallTrace for a new logical request; nil when ctx carries
// no RequestTrace. All CallTrace methods are nil-safe.
func beginCall(ctx context.Context, endpoint string) *CallTrace {
	t, _ := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if t == nil {
		return nil
	}
	c := &CallTrace{Endpoint: endpoint, Entry: time.Now(), rt: t}
	t.mu.Lock()
	t.calls = append(t.calls, c)
	t.mu.Unlock()
	return c
}

// currentCall is the most recently opened CallTrace of ctx's RequestTrace, nil
// when untraced. Requests under one trace are issued sequentially.
func currentCall(ctx context.Context) *CallTrace {
	t, _ := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.calls) == 0 {
		return nil
	}
	return t.calls[len(t.calls)-1]
}

// update runs fn on the CallTrace under the owning RequestTrace's mutex.
func (c *CallTrace) update(fn func()) {
	if c == nil || c.rt == nil {
		return
	}
	c.rt.mu.Lock()
	defer c.rt.mu.Unlock()
	fn()
}

func (c *CallTrace) jitterDone() {
	c.update(func() {
		if c.JitterDone.IsZero() {
			c.JitterDone = time.Now()
		}
	})
}

func (c *CallTrace) picked(proxied bool) {
	c.update(func() { c.Picked, c.Proxied = time.Now(), proxied })
}

func (c *CallTrace) paced() { c.update(func() { c.Paced = time.Now() }) }

func (c *CallTrace) headersStart() { c.update(func() { c.HeadersStart = time.Now() }) }

func (c *CallTrace) ready() { c.update(func() { c.Ready = time.Now() }) }

func (c *CallTrace) done(status, bytes int) {
	c.update(func() {
		c.Done, c.Status, c.Bytes = time.Now(), status, bytes
		c.Attempts++
	})
}
