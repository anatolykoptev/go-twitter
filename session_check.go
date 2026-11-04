package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// epCheckSession labels CheckSession in rate windows, metrics and events.
const epCheckSession = "CheckSession"

// accountSettingsPath is the REST path the web app reads on load.
const accountSettingsPath = "/1.1/account/settings.json"

// SessionInfo is the identity a live session belongs to.
type SessionInfo struct {
	// ScreenName is the handle the session is signed in as, as X reports it.
	ScreenName string
}

// CheckSession verifies acc's own session the way the web app does on load:
// it asks X for the signed-in viewer's settings with the account's own
// identity, cookie jar and proxy. A fixed public profile lookup proves only
// that a token is accepted; this proves THIS account's session is alive, and it
// is the request a real browser makes on load.
//
// The result is classified by the same taxonomy as every other call: 32
// IsAuthExpired, 64 IsSuspended, 326 IsLocked, 226 IsAutomated, 353 IsCSRF, a
// 429 IsRateLimited. CheckSession reports the verdict and leaves the pool
// alone — it never relogins and never benches — so the caller decides.
//
// The call spends from the account's per-endpoint window like any other and
// is refused with IsRateLimited while that window is exhausted or held.
func (c *Client) CheckSession(ctx context.Context, acc *Account) (*SessionInfo, error) {
	if acc == nil {
		return nil, errors.New("twitter: CheckSession: nil account")
	}
	bc, err := c.clientForAccount(acc)
	if err != nil {
		return nil, fmt.Errorf("check session %s: %w", acc.Username, err)
	}
	if !acc.AllowRequest(epCheckSession) {
		return nil, &APIError{Class: errRateLimited, Message: epCheckSession + " rate limited for account"}
	}
	if err := c.acquireFlight(ctx, acc); err != nil {
		return nil, err
	}
	defer c.flightReleased(acc)

	authTok, ct0, ua := acc.Credentials()
	body, hdrs, status, err := c.observedRequest(ctx, acc, epCheckSession, bc, http.MethodGet, c.apiURL(bc, accountSettingsPath), twitterHeaders(authTok, ct0, ua))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		c.attributeTransportError(acc, err)
		return nil, fmt.Errorf("check session %s: %w", acc.Username, err)
	}
	c.observeRateLimit(acc, epCheckSession, hdrs, false)
	c.recordAPICall(epCheckSession, status == http.StatusOK, status == http.StatusTooManyRequests)

	if apiErr := sessionVerdict(status, body, hdrs); apiErr != nil {
		if apiErr.Class == errRateLimited {
			acc.MarkEndpointRateLimited(epCheckSession, parseRateLimitReset(hdrs[xRateLimitReset]))
		}
		apiErr.Message = fmt.Sprintf("check session %s: %s", acc.Username, apiErr.Message)
		return nil, apiErr
	}

	var settings struct {
		ScreenName string `json:"screen_name"`
	}
	if json.Unmarshal(body, &settings) != nil || settings.ScreenName == "" {
		return nil, &APIError{Status: status, Class: errUnexpected,
			Message: fmt.Sprintf("check session %s: settings carry no screen_name", acc.Username)}
	}
	if newCT0 := extractCT0FromHeaders(hdrs); newCT0 != "" && newCT0 != ct0 {
		acc.SetCT0(newCT0)
		authTok2, ct02, _ := acc.Credentials()
		c.saveSession(acc, authTok2, ct02)
	}
	return &SessionInfo{ScreenName: settings.ScreenName}, nil
}

// sessionVerdict turns one settings response into the error CheckSession
// returns, or nil when it is a live session. Body codes win through
// newAPIError (the classifier every path shares); a bare 401 is an expired
// session, an errors-only 2xx keeps its terminal class.
func sessionVerdict(status int, body []byte, hdrs map[string]string) *APIError {
	ae := newAPIError(status, body, hdrs)
	switch {
	case ae.Class != errNone:
	case status == http.StatusUnauthorized:
		ae.Class = errAuthExpired
	case status >= http.StatusOK && status < http.StatusMultipleChoices && errorsOnly(body):
		ae.Class = errorsOnlyClass(body)
	case status == http.StatusOK:
		return nil
	default:
		ae.Class = errUnexpected
	}
	ae.Message = fmt.Sprintf("HTTP %d: class %d", status, ae.Class)
	return ae
}
