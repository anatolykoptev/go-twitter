package twitter

import (
	"context"
	"sync"
	"testing"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
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

// A client built with WithoutRequestJitter performs zero jitter sleep on a GET
// and does not spend the default 0.5-2.5s before sending.
//
// Mutation: request.go doPoolRequest, replace c.preRequestJitter(ctx) with
// stealth.DefaultJitter.Sleep(ctx) => this test goes RED (elapsed assertion).
func TestWithoutRequestJitter_NoSleepOnGET(t *testing.T) {
	f := newRLFakeX(t, okWindow)
	c, _ := newRLClient(t, 1, 50, nil)
	// Sentinel far above the ~0.5s baseline of a GET here: a client that falls
	// back to the process default sleeps 3-4s and trips the elapsed bound.
	stealth.DefaultJitter = stealth.Jitter{Min: 3 * time.Second, Max: 4 * time.Second}

	var cfg ClientConfig
	WithoutRequestJitter()(&cfg)
	c.requestJitter = cfg.RequestJitter
	rec := &jitterRecorder{}
	c.jitterSleep = rec.sleep

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := c.doPoolRequest(ctx, "GET", rlEndpoint, f.url(), nil)
	elapsed := time.Since(start)
	require.NoError(t, err)

	assert.Empty(t, rec.got(), "no-jitter client must not sleep before the request")
	assert.Less(t, elapsed, 2*time.Second, "GET paid the process-default jitter instead of the configured none")
	assert.Equal(t, 1, f.hitsFor("tokA"))
}

// A default client (no option) still draws from the 500ms-2.5s range.
//
// Mutation: same line as above => the recorder sees no call => RED.
func TestDefaultClient_UsesDefaultJitterRange(t *testing.T) {
	f := newRLFakeX(t, okWindow)
	c, _ := newRLClient(t, 1, 50, nil)
	stealth.DefaultJitter = stealth.Jitter{Min: 500 * time.Millisecond, Max: 2500 * time.Millisecond}
	rec := &jitterRecorder{}
	c.jitterSleep = rec.sleep

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		_, _, err := c.doPoolRequest(ctx, "GET", rlEndpoint, f.url(), nil)
		require.NoError(t, err)
	}
	ds := rec.got()
	require.Len(t, ds, 5)
	for _, d := range ds {
		assert.GreaterOrEqual(t, d, 500*time.Millisecond)
		assert.Less(t, d, 2500*time.Millisecond)
	}
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
	assert.Nil(t, d.requestJitter, "unset => stealth.DefaultJitter")
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
