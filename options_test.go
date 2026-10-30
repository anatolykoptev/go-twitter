package twitter

import (
	"context"
	"sync"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
	"github.com/anatolykoptev/go-stealth/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jitterRecorder replaces the real sleep so a test observes the requested
// delay without wall-clock flakiness.
type jitterRecorder struct {
	mu sync.Mutex
	ds []time.Duration
}

func (r *jitterRecorder) sleep(_ context.Context, d time.Duration) error {
	r.mu.Lock()
	r.ds = append(r.ds, d)
	r.mu.Unlock()
	return nil
}

func (r *jitterRecorder) got() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.ds...)
}

func okWindow(string, int) rlWindow {
	return rlWindow{limit: 50, remaining: 40, reset: time.Now().Add(time.Minute)}
}

// A default client (no option) performs zero pre-request jitter on a GET, even
// when the process-wide stealth.DefaultJitter holds a long sentinel range.
//
// Mutation: options.go preRequestJitter, replace the nil-config early return
// with `j := stealth.DefaultJitter` (default resolves to stealth.DefaultJitter
// again) => TestDefaultClient_NoPreRequestJitter goes RED:
// "default client must not sleep before the request".
func TestDefaultClient_NoPreRequestJitter(t *testing.T) {
	f := newRLFakeX(t, okWindow)
	c, _ := newRLClient(t, 1, 50, nil)
	prev := stealth.DefaultJitter
	t.Cleanup(func() { stealth.DefaultJitter = prev })
	stealth.DefaultJitter = stealth.Jitter{Min: 3 * time.Second, Max: 4 * time.Second}
	rec := &jitterRecorder{}
	c.jitterSleep = rec.sleep
	require.Nil(t, c.requestJitter)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := c.doPoolRequest(ctx, "GET", rlEndpoint, f.url(), nil)
	elapsed := time.Since(start)
	require.NoError(t, err)

	assert.Empty(t, rec.got(), "default client must not sleep before the request")
	assert.Less(t, elapsed, 2*time.Second, "GET paid the process-default jitter")
	assert.Equal(t, 1, f.hitsFor("tokA"))
}

// WithoutRequestJitter after WithRequestJitter turns the delay off again.
func TestWithoutRequestJitter_OverridesOptIn(t *testing.T) {
	var cfg ClientConfig
	WithRequestJitter(time.Second, 2*time.Second)(&cfg)
	WithoutRequestJitter()(&cfg)
	c := &Client{requestJitter: cfg.RequestJitter}
	rec := &jitterRecorder{}
	c.jitterSleep = rec.sleep
	require.NoError(t, c.preRequestJitter(context.Background()))
	assert.Empty(t, rec.got())
}

// The per-account pacer still spaces two back-to-back requests on one account
// when pre-request jitter is off.
//
// Mutation: request.go doPoolRequest, delete the `c.accountPacer.Wait(ctx,
// acc.ID())` call => the elapsed assertion goes RED.
func TestDefaultClient_AccountPacerStillSpacesRequests(t *testing.T) {
	const pace = time.Second
	f := newRLFakeX(t, okWindow)
	c, _ := newRLClient(t, 1, 50, nil)
	rec := &jitterRecorder{}
	c.jitterSleep = rec.sleep

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Warm-up without a pacer so one-time setup cost is not part of the span.
	_, _, err := c.doPoolRequest(ctx, "GET", rlEndpoint, f.url(), nil)
	require.NoError(t, err)

	c.accountPacer = ratelimit.NewKeyedPacer(pace, 0)
	start := time.Now()
	for i := 0; i < 2; i++ {
		_, _, err = c.doPoolRequest(ctx, "GET", rlEndpoint, f.url(), nil)
		require.NoError(t, err)
	}

	// The first paced request fires at once; the second waits out the pace.
	assert.GreaterOrEqual(t, time.Since(start), pace-50*time.Millisecond, "second request on the same account was not paced")
	assert.Empty(t, rec.got(), "pacing must not be implemented via the jitter hook")
}

func TestWithRequestJitter_CustomRange(t *testing.T) {
	f := newRLFakeX(t, okWindow)
	c, _ := newRLClient(t, 1, 50, nil)
	var cfg ClientConfig
	WithRequestJitter(10*time.Millisecond, 20*time.Millisecond)(&cfg)
	c.requestJitter = cfg.RequestJitter
	rec := &jitterRecorder{}
	c.jitterSleep = rec.sleep

	_, _, err := c.doPoolRequest(context.Background(), "GET", rlEndpoint, f.url(), nil)
	require.NoError(t, err)
	ds := rec.got()
	require.Len(t, ds, 1)
	assert.GreaterOrEqual(t, ds[0], 10*time.Millisecond)
	assert.Less(t, ds[0], 20*time.Millisecond)
}

// NewClient must carry the option into the Client (a config field nobody reads
// would silently leave the +1.5s in place).
func TestNewClient_WiresRequestJitter(t *testing.T) {
	c, err := NewClient(ClientConfig{DisableGuestFallback: true, SessionDir: t.TempDir()}, WithoutRequestJitter(), WithAccountPace(0, 0))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NotNil(t, c.requestJitter)
	assert.Equal(t, stealth.Jitter{}, *c.requestJitter)
	assert.Nil(t, c.accountPacer, "WithAccountPace(0,0) disables the pacer")

	d, err := NewClient(ClientConfig{DisableGuestFallback: true, SessionDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	assert.Nil(t, d.requestJitter, "unset => no pre-request jitter")
	assert.NotNil(t, d.accountPacer)
}

func TestWithAccountPace(t *testing.T) {
	var cfg ClientConfig
	WithAccountPace(100*time.Millisecond, 50*time.Millisecond)(&cfg)
	assert.False(t, cfg.AccountPaceDisabled)
	assert.Equal(t, 100*time.Millisecond, cfg.AccountPaceMin)
	assert.Equal(t, 50*time.Millisecond, cfg.AccountPaceRandom)

	var z ClientConfig
	WithAccountPace(0, 0)(&z)
	assert.True(t, z.AccountPaceDisabled)

	var m ClientConfig
	WithAccountPace(0, time.Second)(&m)
	m.defaults()
	assert.Equal(t, time.Nanosecond, m.AccountPaceMin, "zero min must not fall back to the 800ms default")
}

// WithAccountPace(min, 0) must space by ~min, not min plus the 1.2s default
// random component (defaults() replaces a zero AccountPaceRandom).
//
// Mutation: options.go WithAccountPace, drop the `if random <= 0 { random =
// time.Nanosecond }` floor => TestWithAccountPace_ZeroRandomMeansNoRandom goes
// RED (`AccountPaceRandom` becomes 1.2s).
func TestWithAccountPace_ZeroRandomMeansNoRandom(t *testing.T) {
	cfg := ClientConfig{}
	WithAccountPace(100*time.Millisecond, 0)(&cfg)
	cfg.defaults()
	assert.Equal(t, 100*time.Millisecond, cfg.AccountPaceMin)
	assert.Less(t, cfg.AccountPaceRandom, time.Millisecond, "zero random must not be replaced by the 1.2s default")

	p := buildAccountPacer(cfg)
	ctx := context.Background()
	require.NoError(t, p.Wait(ctx, "acct"))
	start := time.Now()
	require.NoError(t, p.Wait(ctx, "acct"))
	gap := time.Since(start)
	assert.GreaterOrEqual(t, gap, 90*time.Millisecond)
	assert.Less(t, gap, 300*time.Millisecond, "spacing must be ~100ms, not 100ms + up to 1.2s")
}
