package twitter

import "context"

// AutoReloginGate lets an external controller veto automatic re-login flows.
// A nil gate is treated as "always allow" — preserves backward compatibility
// for consumers that don't use go-social.
type AutoReloginGate interface {
	Allowed(ctx context.Context, username string) (allowed bool, reason string)
}

// SetAutoReloginGate registers a gate checked before relogin attempts.
func (c *Client) SetAutoReloginGate(g AutoReloginGate) {
	c.reloginGate = g
}
