package twitter

import (
	"testing"
	"time"

	"github.com/anatolykoptev/go-stealth/pool"
)

// newSelfHealTestClient builds a minimal Client wired exactly like NewClient for
// the self-heal path: real pool, real Accounts with embedded HealthTracker, and
// the real nonResponsiveBackoff derived from NonResponsiveCooldown. It does NOT
// stand up the stealth HTTP stack — the self-heal policy under test lives in the
// pool + HealthTracker, which this exercises directly via the same calls
// request.go makes.
func newSelfHealTestClient(t *testing.T, n int, cooldown time.Duration) (*Client, []*Account) {
	t.Helper()
	accs := make([]*Account, n)
	for i := range accs {
		a := &Account{Username: string(rune('a' + i))}
		a.active = true
		a.HealthTracker = pool.DefaultHealthTracker() // maxConsecFailures=5
		accs[i] = a
	}
	nrb := pool.BackoffConfig{
		InitialWait: cooldown,
		MaxWait:     30 * time.Minute,
		Multiplier:  2.0,
		JitterPct:   0.3,
	}
	c := &Client{
		pool:                 pool.New(accs, pool.DefaultConfig()),
		nonResponsiveBackoff: nrb,
	}
	return c, accs
}

// tripOnce drives RecordFailure to the consecutive-failure trip threshold and
// applies the exact transient-failure action request.go takes.
func tripOnce(c *Client, acc *Account) {
	for {
		if acc.RecordFailure() {
			c.pool.SoftDeactivateBackoff(acc, c.nonResponsiveBackoff, acc.TripCount())
			return
		}
	}
}

// TestNonResponsive_SoftDeactivatesThenRecovers proves a transiently-broken
// endpoint no longer permanently latches an account: after the trip the account
// is soft-deactivated with a NON-zero (recoverable) ReactivateAt, and once the
// cooldown elapses the pool serves it again — no restart, no reconstruction.
func TestNonResponsive_SoftDeactivatesThenRecovers(t *testing.T) {
	c, accs := newSelfHealTestClient(t, 1, 5*time.Minute)
	acc := accs[0]

	tripOnce(c, acc)

	if acc.IsActive() {
		t.Fatal("account should be deactivated after tripping")
	}
	if acc.ReactivateAt().IsZero() {
		t.Fatal("transient failure must leave a NON-zero ReactivateAt (recoverable), not a permanent latch")
	}
	// Pool exhausted while in cooldown.
	if _, err := c.pool.Next(nil); err == nil {
		t.Fatal("expected pool exhausted during cooldown")
	}
	// Advance the fake clock past the cooldown.
	acc.SetReactivateAt(time.Now().Add(-time.Second))
	got, err := c.pool.Next(nil)
	if err != nil {
		t.Fatalf("account failed to auto-recover after cooldown: %v", err)
	}
	if got.Username != acc.Username || !got.IsActive() {
		t.Fatal("recovered account should be served and active")
	}
}

// TestNonResponsive_AllAccountsBroken_PoolSelfHeals reproduces the live failure:
// every account hits the same transiently-broken endpoint and trips, so the pool
// reports "all N unavailable"; after the cooldown the pool serves again WITHOUT a
// process restart.
func TestNonResponsive_AllAccountsBroken_PoolSelfHeals(t *testing.T) {
	const n = 4
	c, accs := newSelfHealTestClient(t, n, 5*time.Minute)

	for _, a := range accs {
		tripOnce(c, a)
	}
	if _, err := c.pool.Next(nil); err == nil {
		t.Fatal("expected all N unavailable while every account is in cooldown")
	}

	for _, a := range accs {
		a.SetReactivateAt(time.Now().Add(-time.Second))
	}
	// Drive Next n times: each round-robin hit auto-reactivates one account, so
	// after n calls the WHOLE fleet must be back -- proving full self-heal, not
	// just one survivor. (Healthy() does not auto-reactivate, only Next does.)
	recovered := map[string]bool{}
	for i := 0; i < n; i++ {
		got, err := c.pool.Next(nil)
		if err != nil {
			t.Fatalf("pool failed to self-heal after cooldown (call %d): %v", i, err)
		}
		recovered[got.Username] = true
	}
	if len(recovered) != n {
		t.Fatalf("only %d/%d accounts self-healed: %v", len(recovered), n, recovered)
	}
	if c.pool.Healthy(nil) != n {
		t.Fatalf("expected all %d accounts healthy after self-heal, got %d", n, c.pool.Healthy(nil))
	}
}

// TestSuspended_StaysPermanent guards against over-correction: the errSuspended
// path (DeactivateItem) must remain permanent — zero ReactivateAt the
// auto-reactivate guard never clears.
func TestSuspended_StaysPermanent(t *testing.T) {
	c, accs := newSelfHealTestClient(t, 1, 5*time.Minute)
	acc := accs[0]

	// This is exactly what request.go does on errSuspended.
	c.pool.DeactivateItem(acc)

	if !acc.ReactivateAt().IsZero() {
		t.Fatal("suspended account must keep a zero ReactivateAt (permanent)")
	}
	if _, err := c.pool.Next(nil); err == nil {
		t.Fatal("suspended account must never be auto-reactivated")
	}
	acc.SetReactivateAt(acc.ReactivateAt()) // no-op; still zero
	if _, err := c.pool.Next(nil); err == nil {
		t.Fatal("suspended account stayed permanently out, as required")
	}
}

// TestNonResponsiveCooldown_DefaultsTo5m proves the config default is wired.
func TestNonResponsiveCooldown_DefaultsTo5m(t *testing.T) {
	cfg := ClientConfig{}
	cfg.defaults()
	if cfg.NonResponsiveCooldown != 5*time.Minute {
		t.Fatalf("NonResponsiveCooldown default = %v, want 5m", cfg.NonResponsiveCooldown)
	}
}
