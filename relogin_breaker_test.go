package twitter

import (
	"context"
	"testing"
	"time"
)

// --- Fix 3: relogin circuit-breaker preserves valid creds -------------------

// TestReloginBreaker_OpensAfterConsecutiveFailures proves the default relogin
// gate opens after N consecutive login failures and stays open for a cooldown.
// While open, relogin() is short-circuited BEFORE SetCredentials("",""), so a
// transient failure storm can neither destroy a still-valid session nor keep
// hammering the WAF-blocked guest-token/login endpoint (the self-worsening 399
// "try again later" throttle).
func TestReloginBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	g := newReloginBreaker(reloginBreakerThreshold, time.Minute)
	ctx := context.Background()
	const user = "u"

	// Fresh breaker allows the first attempt.
	if ok, _ := g.Allowed(ctx, user); !ok {
		t.Fatal("fresh breaker must allow the first relogin attempt")
	}

	// Record threshold consecutive failures.
	for i := 0; i < reloginBreakerThreshold; i++ {
		g.RecordFailure(user)
	}

	if ok, reason := g.Allowed(ctx, user); ok {
		t.Fatalf("breaker must be OPEN after %d consecutive failures, got allowed (reason=%q)", reloginBreakerThreshold, reason)
	}
}

// TestReloginBreaker_SuccessResets proves a successful login clears the
// consecutive-failure count so the account is not penalized for old failures.
func TestReloginBreaker_SuccessResets(t *testing.T) {
	g := newReloginBreaker(reloginBreakerThreshold, time.Minute)
	ctx := context.Background()
	const user = "u"

	for i := 0; i < reloginBreakerThreshold-1; i++ {
		g.RecordFailure(user)
	}
	g.RecordSuccess(user)
	// One more failure must not trip the breaker (count was reset).
	g.RecordFailure(user)
	if ok, _ := g.Allowed(ctx, user); !ok {
		t.Fatal("breaker must stay closed after a success reset the failure count")
	}
}

// TestReloginBreaker_PerUser proves the breaker is keyed per account: one
// account tripping its breaker must not block relogin for a different account.
func TestReloginBreaker_PerUser(t *testing.T) {
	g := newReloginBreaker(reloginBreakerThreshold, time.Minute)
	ctx := context.Background()

	for i := 0; i < reloginBreakerThreshold; i++ {
		g.RecordFailure("bad")
	}
	if ok, _ := g.Allowed(ctx, "bad"); ok {
		t.Fatal("tripped account must be blocked")
	}
	if ok, _ := g.Allowed(ctx, "good"); !ok {
		t.Fatal("a different account must NOT be blocked by another account's tripped breaker")
	}
}

// TestReloginBreaker_RecoversAfterCooldown proves the breaker re-allows attempts
// once the cooldown window elapses (it is not a permanent latch).
func TestReloginBreaker_RecoversAfterCooldown(t *testing.T) {
	g := newReloginBreaker(reloginBreakerThreshold, 10*time.Millisecond)
	ctx := context.Background()
	const user = "u"

	for i := 0; i < reloginBreakerThreshold; i++ {
		g.RecordFailure(user)
	}
	if ok, _ := g.Allowed(ctx, user); ok {
		t.Fatal("breaker must be open immediately after tripping")
	}
	time.Sleep(20 * time.Millisecond)
	if ok, _ := g.Allowed(ctx, user); !ok {
		t.Fatal("breaker must re-allow attempts after the cooldown window elapses")
	}
}

// TestNewClient_WiresDefaultReloginBreaker proves a client built without an
// external gate gets the internal default breaker (so the protection is on by
// default), while SetAutoReloginGate still overrides it (go-social path).
func TestNewClient_DefaultReloginBreakerPresent(t *testing.T) {
	c := &Client{}
	wireDefaultReloginGate(c)
	if c.reloginGate == nil {
		t.Fatal("default relogin breaker must be wired when no external gate is set")
	}

	// External gate overrides.
	ext := AutoReloginGateFunc(func(context.Context, string) (bool, string) { return true, "" })
	c.SetAutoReloginGate(ext)
	wireDefaultReloginGate(c) // must NOT clobber an explicitly-set gate
	if _, ok := c.reloginGate.(AutoReloginGateFunc); !ok {
		t.Fatal("wireDefaultReloginGate must not overwrite an externally-set gate")
	}
}
