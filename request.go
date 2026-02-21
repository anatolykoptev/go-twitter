package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	stealth "github.com/anatolykoptev/go-stealth"
)

const maxRetries = 3

// doGET executes a GET request with multi-account retry, ct0 rotation, relogin,
// and guest-token fallback.
func (c *Client) doGET(ctx context.Context, endpoint, url string) ([]byte, map[string]string, error) {
	// Anti-fingerprint jitter
	if err := stealth.DefaultJitter.Sleep(ctx); err != nil {
		return nil, nil, err
	}

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			delay := stealth.DefaultBackoff.Duration(attempt)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}

		var acc *Account
		var accErr error

		filter := func(a *Account) bool {
			return a.AllowRequest(endpoint) && time.Now().After(a.proxyBackoff)
		}

		if requiresAuth(endpoint) {
			acc, accErr = c.pool.NextWithWait(ctx, filter, 5*time.Minute)
		} else {
			acc, accErr = c.pool.Next(filter)
		}
		if accErr != nil {
			lastErr = accErr
			break
		}

		// Proactive ct0 rotation
		if acc.CT0Age() > ct0MaxAge {
			_, oldCT0, _ := acc.Credentials()
			acc.RotateCT0()
			slog.Info("ct0 rotated (proactive)", slog.String("user", acc.Username), slog.String("old_prefix", oldCT0[:min(8, len(oldCT0))]))
			authTok2, ct02, _ := acc.Credentials()
			_ = saveSession(c.cfg.SessionDir, acc.Username, authTok2, ct02)
		}

		bc := c.clientForAccount(acc)

		authTok, ct0, ua := acc.Credentials()
		body, respHdrs, status, err := c.doRequest(bc, "GET", url, twitterHeaders(authTok, ct0, ua))
		if err != nil {
			if acc.Proxy != "" && isProxyError(err) {
				c.markProxyDown(acc)
			} else {
				acc.RecordFailure()
			}
			lastErr = err
			continue
		}

		// Reset proxy consecutive failures on any HTTP response
		acc.mu.Lock()
		acc.proxyConsecFails = 0
		acc.mu.Unlock()

		// Handle HTTP status
		switch {
		case status == 429:
			c.recordAPICall(endpoint, false, true)
			acc.MarkEndpointRateLimited(endpoint, parseRateLimitReset(respHdrs["x-rate-limit-reset"]))
			lastErr = fmt.Errorf("429 rate limited")
			continue

		case status == 401 || status == 403:
			c.recordAPICall(endpoint, false, false)
			errClass := classifyError(body, respHdrs)
			switch errClass {
			case errCSRF:
				slog.Warn("CSRF error 353, rotating ct0", slog.String("user", acc.Username))
				acc.RotateCT0()
				authTok2, ct02, ua2 := acc.Credentials()
				_ = saveSession(c.cfg.SessionDir, acc.Username, authTok2, ct02)
				body2, respHdrs2, status2, err2 := c.doRequest(bc, "GET", url, twitterHeaders(authTok2, ct02, ua2))
				if err2 == nil && status2 == 200 {
					if newCT0 := extractCT0FromHeaders(respHdrs2); newCT0 != "" {
						acc.SetCT0(newCT0)
						authTok3, ct03, _ := acc.Credentials()
						_ = saveSession(c.cfg.SessionDir, acc.Username, authTok3, ct03)
					}
					c.recordAPICall(endpoint, true, false)
					acc.RecordSuccess()
					return body2, respHdrs2, nil
				}
				acc.RecordFailure()
				lastErr = fmt.Errorf("CSRF retry failed")
				continue
			case errAuthExpired:
				slog.Warn("auth expired (code 32), attempting relogin", slog.String("user", acc.Username))
				if reErr := c.relogin(acc); reErr != nil {
					slog.Warn("relogin failed", slog.String("user", acc.Username), slog.Any("error", reErr))
					c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
					lastErr = reErr
					continue
				}
				authTok2, ct02, ua2 := acc.Credentials()
				body2, respHdrs2, status2, err2 := c.doRequest(bc, "GET", url, twitterHeaders(authTok2, ct02, ua2))
				if err2 == nil && status2 == 200 {
					c.recordAPICall(endpoint, true, false)
					acc.RecordSuccess()
					return body2, respHdrs2, nil
				}
				c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
				lastErr = fmt.Errorf("post-relogin request failed")
				continue
			default:
				acc.RecordFailure()
				lastErr = fmt.Errorf("%s HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
				continue
			}

		case status != 200:
			c.recordAPICall(endpoint, false, false)
			slog.Warn("doGET non-200", slog.String("endpoint", endpoint), slog.Int("status", status), slog.String("body", truncateBytes(body, 500)))
			if shouldDeactivate := acc.RecordFailure(); shouldDeactivate {
				total, failed, consec := acc.Stats()
				slog.Warn("account unhealthy, deactivating",
					slog.String("user", acc.Username),
					slog.Int("total", total),
					slog.Int("failed", failed),
					slog.Int("consec", consec))
				c.pool.DeactivateItem(acc)
			}
			return nil, nil, fmt.Errorf("%s HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
		}

		// HTTP 200 — check for error codes in response body
		errClass := classifyError(body, respHdrs)
		switch errClass {
		case errNone:
			if newCT0 := extractCT0FromHeaders(respHdrs); newCT0 != "" && newCT0 != ct0 {
				acc.SetCT0(newCT0)
				authTok2, ct02, _ := acc.Credentials()
				_ = saveSession(c.cfg.SessionDir, acc.Username, authTok2, ct02)
			}
			c.recordAPICall(endpoint, true, false)
			acc.RecordSuccess()
			return body, respHdrs, nil

		case errCSRF:
			slog.Warn("CSRF error 353, rotating ct0", slog.String("user", acc.Username))
			acc.RotateCT0()
			authTok2, ct02, ua2 := acc.Credentials()
			_ = saveSession(c.cfg.SessionDir, acc.Username, authTok2, ct02)
			body2, respHdrs2, status2, err2 := c.doRequest(bc, "GET", url, twitterHeaders(authTok2, ct02, ua2))
			if err2 == nil && status2 == 200 && classifyError(body2, respHdrs2) == errNone {
				if newCT0 := extractCT0FromHeaders(respHdrs2); newCT0 != "" {
					acc.SetCT0(newCT0)
					authTok3, ct03, _ := acc.Credentials()
					_ = saveSession(c.cfg.SessionDir, acc.Username, authTok3, ct03)
				}
				c.recordAPICall(endpoint, true, false)
				acc.RecordSuccess()
				return body2, respHdrs2, nil
			}
			lastErr = fmt.Errorf("CSRF retry failed")
			continue

		case errAuthExpired:
			slog.Warn("auth expired (code 32), attempting relogin", slog.String("user", acc.Username))
			if reErr := c.relogin(acc); reErr != nil {
				slog.Warn("relogin failed, soft-deactivating", slog.String("user", acc.Username), slog.Any("error", reErr))
				c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
				lastErr = reErr
				continue
			}
			authTok2, ct02, ua2 := acc.Credentials()
			body2, respHdrs2, status2, err2 := c.doRequest(bc, "GET", url, twitterHeaders(authTok2, ct02, ua2))
			if err2 == nil && status2 == 200 {
				c.recordAPICall(endpoint, true, false)
				acc.RecordSuccess()
				return body2, respHdrs2, nil
			}
			c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
			lastErr = fmt.Errorf("post-relogin request failed")
			continue

		case errInternal:
			if hasResponseData(body) {
				if newCT0 := extractCT0FromHeaders(respHdrs); newCT0 != "" && newCT0 != ct0 {
					acc.SetCT0(newCT0)
					authTok2, ct02, _ := acc.Credentials()
					_ = saveSession(c.cfg.SessionDir, acc.Username, authTok2, ct02)
				}
				c.recordAPICall(endpoint, true, false)
				acc.RecordSuccess()
				slog.Debug("error 131 with usable data, treating as success", slog.String("endpoint", endpoint))
				return body, respHdrs, nil
			}
			slog.Warn("error 131 without data, retrying", slog.String("user", acc.Username), slog.String("endpoint", endpoint))
			lastErr = fmt.Errorf("Twitter internal error (131)")
			continue

		case errBanned:
			c.recordAPICall(endpoint, false, false)
			slog.Warn("account banned (code 88)", slog.String("user", acc.Username))
			c.pool.SoftDeactivate(acc, c.cfg.BanCooldown)
			lastErr = fmt.Errorf("account banned")
			continue

		case errSuspended:
			c.recordAPICall(endpoint, false, false)
			slog.Warn("account suspended (code 64), permanently deactivating", slog.String("user", acc.Username))
			c.pool.DeactivateItem(acc)
			lastErr = fmt.Errorf("account suspended")
			continue

		case errLocked:
			c.recordAPICall(endpoint, false, false)
			slog.Warn("account locked (code 326, captcha needed)", slog.String("user", acc.Username))
			if c.cfg.CaptchaSolver != nil {
				slog.Info("attempting CAPTCHA unlock via relogin", slog.String("user", acc.Username))
				if reErr := c.relogin(acc); reErr == nil {
					authTok2, ct02, ua2 := acc.Credentials()
					body2, respHdrs2, status2, err2 := c.doRequest(bc, "GET", url, twitterHeaders(authTok2, ct02, ua2))
					if err2 == nil && status2 == 200 {
						c.recordAPICall(endpoint, true, false)
						acc.RecordSuccess()
						slog.Info("CAPTCHA unlock succeeded", slog.String("user", acc.Username))
						return body2, respHdrs2, nil
					}
					slog.Warn("post-CAPTCHA request failed", slog.String("user", acc.Username))
				} else {
					slog.Warn("CAPTCHA unlock failed", slog.String("user", acc.Username), slog.Any("error", reErr))
				}
			}
			c.pool.SoftDeactivate(acc, c.cfg.BanCooldown)
			lastErr = fmt.Errorf("account locked")
			continue

		default: // errBlocked, errNotAuthorized
			c.recordAPICall(endpoint, false, false)
			slog.Warn("account error", slog.String("user", acc.Username), slog.Int("class", int(errClass)))
			c.pool.SoftDeactivate(acc, c.cfg.AuthCooldown)
			lastErr = fmt.Errorf("account error class %d", errClass)
			continue
		}
	}

	// --- Guest token fallback ---
	if requiresAuth(endpoint) {
		if lastErr != nil {
			return nil, nil, fmt.Errorf("pool exhausted for %s (requires auth): %w", endpoint, lastErr)
		}
		return nil, nil, fmt.Errorf("%s requires authenticated account", endpoint)
	}

	gt, ok := c.getGuestTokenCached()
	if !ok {
		token, err := c.acquireGuestToken(ctx, c.client)
		if err != nil {
			if lastErr != nil {
				return nil, nil, fmt.Errorf("pool exhausted for %s: %w", endpoint, lastErr)
			}
			return nil, nil, fmt.Errorf("guest token unavailable for %s: %w", endpoint, err)
		}
		c.setGuestToken(token)
		gt = token
		slog.Info("guest token acquired as fallback", slog.String("endpoint", endpoint))
	}

	body, respHdrs, status, err := c.doRequest(c.client, "GET", url, guestHeaders(gt))
	if err != nil {
		return nil, nil, err
	}
	if status == 429 {
		c.recordAPICall(endpoint, false, true)
		c.markGuestTokenRateLimited(parseRateLimitReset(respHdrs["x-rate-limit-reset"]))
		return nil, nil, fmt.Errorf("guest token rate-limited for %s", endpoint)
	}
	if status == 401 || status == 403 {
		slog.Warn("guest token expired, reacquiring", slog.String("endpoint", endpoint), slog.Int("status", status))
		c.setGuestToken("")
		newGT, gtErr := c.acquireGuestToken(ctx, c.client)
		if gtErr != nil {
			c.recordAPICall(endpoint, false, false)
			return nil, nil, fmt.Errorf("guest token reacquisition failed for %s: %w", endpoint, gtErr)
		}
		c.setGuestToken(newGT)
		body, respHdrs, status, err = c.doRequest(c.client, "GET", url, guestHeaders(newGT))
		if err != nil {
			return nil, nil, err
		}
		if status != 200 {
			c.recordAPICall(endpoint, false, false)
			return nil, nil, fmt.Errorf("%s (guest retry) HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
		}
		c.recordAPICall(endpoint, true, false)
		return body, respHdrs, nil
	}
	if status != 200 {
		c.recordAPICall(endpoint, false, false)
		return nil, nil, fmt.Errorf("%s (guest) HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
	}
	c.recordAPICall(endpoint, true, false)
	return body, respHdrs, nil
}

// doPOST executes a POST mutation with a specific account.
// Unlike doGET, it does not rotate accounts from the pool — the caller provides the account.
// Handles CSRF rotation, auth expiry, and retries on transient errors.
func (c *Client) doPOST(ctx context.Context, acc *Account, endpoint, url string, payload []byte) ([]byte, error) {
	if err := stealth.DefaultJitter.Sleep(ctx); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			delay := stealth.DefaultBackoff.Duration(attempt)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// Proactive ct0 rotation
		if acc.CT0Age() > ct0MaxAge {
			acc.RotateCT0()
			authTok, ct0, _ := acc.Credentials()
			_ = saveSession(c.cfg.SessionDir, acc.Username, authTok, ct0)
		}

		bc := c.clientForAccount(acc)
		authTok, ct0, ua := acc.Credentials()
		body, respHdrs, status, err := c.doRequestWithBody(bc, "POST", url, twitterHeaders(authTok, ct0, ua), bytes.NewReader(payload))
		if err != nil {
			if acc.Proxy != "" && isProxyError(err) {
				c.markProxyDown(acc)
			} else {
				acc.RecordFailure()
			}
			lastErr = err
			continue
		}

		// Reset proxy consecutive failures on any HTTP response
		acc.mu.Lock()
		acc.proxyConsecFails = 0
		acc.mu.Unlock()

		switch {
		case status == 429:
			c.recordAPICall(endpoint, false, true)
			acc.MarkEndpointRateLimited(endpoint, parseRateLimitReset(respHdrs["x-rate-limit-reset"]))
			lastErr = fmt.Errorf("429 rate limited")
			continue

		case status == 401 || status == 403:
			c.recordAPICall(endpoint, false, false)
			errClass := classifyError(body, respHdrs)
			switch errClass {
			case errCSRF:
				slog.Warn("doPOST: CSRF error 353, rotating ct0", slog.String("user", acc.Username))
				acc.RotateCT0()
				authTok2, ct02, ua2 := acc.Credentials()
				_ = saveSession(c.cfg.SessionDir, acc.Username, authTok2, ct02)
				body2, _, status2, err2 := c.doRequestWithBody(bc, "POST", url, twitterHeaders(authTok2, ct02, ua2), bytes.NewReader(payload))
				if err2 == nil && (status2 == 200 || status2 == 201) {
					c.recordAPICall(endpoint, true, false)
					acc.RecordSuccess()
					return body2, nil
				}
				acc.RecordFailure()
				lastErr = fmt.Errorf("CSRF retry failed")
				continue
			case errAuthExpired:
				slog.Warn("doPOST: auth expired, attempting relogin", slog.String("user", acc.Username))
				if reErr := c.relogin(acc); reErr != nil {
					lastErr = fmt.Errorf("relogin failed: %w", reErr)
					continue
				}
				authTok2, ct02, ua2 := acc.Credentials()
				body2, _, status2, err2 := c.doRequestWithBody(bc, "POST", url, twitterHeaders(authTok2, ct02, ua2), bytes.NewReader(payload))
				if err2 == nil && (status2 == 200 || status2 == 201) {
					c.recordAPICall(endpoint, true, false)
					acc.RecordSuccess()
					return body2, nil
				}
				lastErr = fmt.Errorf("post-relogin request failed")
				continue
			default:
				acc.RecordFailure()
				return nil, fmt.Errorf("%s HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
			}

		case status != 200:
			c.recordAPICall(endpoint, false, false)
			acc.RecordFailure()
			return nil, fmt.Errorf("%s HTTP %d: %s", endpoint, status, truncateBytes(body, 200))
		}

		// HTTP 200 — check for error codes in response body
		errClass := classifyError(body, respHdrs)
		switch errClass {
		case errNone:
			if newCT0 := extractCT0FromHeaders(respHdrs); newCT0 != "" && newCT0 != ct0 {
				acc.SetCT0(newCT0)
				authTok2, ct02, _ := acc.Credentials()
				_ = saveSession(c.cfg.SessionDir, acc.Username, authTok2, ct02)
			}
			c.recordAPICall(endpoint, true, false)
			acc.RecordSuccess()
			return body, nil
		case errCSRF:
			slog.Warn("doPOST: CSRF in 200, rotating ct0", slog.String("user", acc.Username))
			acc.RotateCT0()
			authTok2, ct02, ua2 := acc.Credentials()
			_ = saveSession(c.cfg.SessionDir, acc.Username, authTok2, ct02)
			body2, _, status2, err2 := c.doRequestWithBody(bc, "POST", url, twitterHeaders(authTok2, ct02, ua2), bytes.NewReader(payload))
			if err2 == nil && (status2 == 200 || status2 == 201) && classifyError(body2, nil) == errNone {
				c.recordAPICall(endpoint, true, false)
				acc.RecordSuccess()
				return body2, nil
			}
			lastErr = fmt.Errorf("CSRF retry failed")
			continue
		default:
			c.recordAPICall(endpoint, false, false)
			acc.RecordFailure()
			return nil, fmt.Errorf("%s error class %d: %s", endpoint, errClass, truncateBytes(body, 200))
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("%s failed after %d attempts: %w", endpoint, maxRetries, lastErr)
	}
	return nil, fmt.Errorf("%s failed after %d attempts", endpoint, maxRetries)
}

// requiresAuth returns true for endpoints that need a real authenticated account.
func requiresAuth(endpoint string) bool {
	switch endpoint {
	case "Following", "Followers", "Retweeters", "CreateTweet":
		return true
	}
	return false
}

// isProxyError returns true if the error looks like a proxy connectivity failure.
func isProxyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "proxy") ||
		strings.Contains(msg, "SOCKS") ||
		strings.Contains(msg, "tunnel") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host")
}

// markProxyDown applies exponential backoff for proxy failures.
func (c *Client) markProxyDown(acc *Account) {
	acc.mu.Lock()
	acc.proxyConsecFails++
	fails := acc.proxyConsecFails
	acc.mu.Unlock()

	duration := stealth.BackoffConfig{
		InitialWait: c.cfg.ProxyBackoffInitial,
		MaxWait:     c.cfg.ProxyBackoffMax,
		Multiplier:  2.0,
		JitterPct:   0.3,
	}.Duration(fails - 1)

	acc.mu.Lock()
	acc.proxyBackoff = time.Now().Add(duration)
	acc.mu.Unlock()

	slog.Warn("proxy down, backing off",
		slog.String("user", acc.Username),
		slog.String("proxy", stealth.MaskProxy(acc.Proxy)),
		slog.Int("consec_fails", fails),
		slog.Duration("backoff", duration))
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// hasResponseData returns true if the JSON body contains a non-null "data" field.
func hasResponseData(body []byte) bool {
	var probe struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	return len(probe.Data) > 0 && string(probe.Data) != "null"
}

// addGraphQLParams builds the full URL with variables, features, and optional fieldToggles.
func addGraphQLParams(url string, variables, features map[string]any, fieldToggles ...map[string]any) string {
	v, _ := json.Marshal(variables)
	f, _ := json.Marshal(features)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	result := url + sep + "variables=" + jsonEscape(v) + "&features=" + jsonEscape(f)
	if len(fieldToggles) > 0 && fieldToggles[0] != nil {
		ft, _ := json.Marshal(fieldToggles[0])
		result += "&fieldToggles=" + jsonEscape(ft)
	}
	return result
}

func jsonEscape(b []byte) string {
	s := string(b)
	var result strings.Builder
	for _, ch := range s {
		switch {
		case ch == ' ':
			result.WriteString("%20")
		case ch == '"':
			result.WriteString("%22")
		case ch == '{':
			result.WriteString("%7B")
		case ch == '}':
			result.WriteString("%7D")
		case ch == '[':
			result.WriteString("%5B")
		case ch == ']':
			result.WriteString("%5D")
		case ch == ':':
			result.WriteString("%3A")
		case ch == ',':
			result.WriteString("%2C")
		case ch == '\'':
			result.WriteString("%27")
		case ch == '|':
			result.WriteString("%7C")
		default:
			result.WriteRune(ch)
		}
	}
	return result.String()
}
