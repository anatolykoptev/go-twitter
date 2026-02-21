package twitter

import (
	"encoding/json"
	"strconv"
	"time"
)

// errorClass categorizes Twitter API error responses for targeted handling.
type errorClass int

const (
	errNone          errorClass = iota
	errBanned                   // 88 — rate limit abuse
	errSuspended                // 64 — account suspended
	errLocked                   // 326 — account locked (captcha needed)
	errCSRF                     // 353 — csrf token mismatch
	errAuthExpired              // 32 — could not authenticate
	errBlocked                  // 161 — blocked from performing action
	errNotAuthorized            // 179, 219 — not authorized
	errInternal                 // 131 — Twitter internal error
)

// classifyError inspects a response body for known Twitter error codes.
func classifyError(body []byte, _ map[string]string) errorClass {
	var errResp struct {
		Errors []struct {
			Code int `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &errResp) != nil || len(errResp.Errors) == 0 {
		return errNone
	}

	for _, e := range errResp.Errors {
		switch e.Code {
		case 88:
			return errBanned
		case 64:
			return errSuspended
		case 326:
			return errLocked
		case 353:
			return errCSRF
		case 32:
			return errAuthExpired
		case 161:
			return errBlocked
		case 179, 219:
			return errNotAuthorized
		case 131:
			return errInternal
		}
	}
	return errNone
}

// parseRateLimitReset parses the X-Rate-Limit-Reset unix timestamp header.
// Falls back to 15 minutes from now if missing or invalid.
func parseRateLimitReset(v string) time.Time {
	if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(ts, 0)
	}
	return time.Now().Add(15 * time.Minute)
}
