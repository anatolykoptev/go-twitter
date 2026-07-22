package twitter

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// errorClass categorizes Twitter API error responses for targeted handling.
type errorClass int

const (
	errNone          errorClass = iota
	errBanned                   // 88 — account banned / rate-limit abuse (durable)
	errRateLimited              // HTTP 429 — rate limit (transient)
	errSuspended                // 64 — account suspended
	errLocked                   // 326 — account locked (captcha needed)
	errCSRF                     // 353 — csrf token mismatch
	errAuthExpired              // 32 — could not authenticate
	errBlocked                  // 161 — blocked from performing action
	errNotAuthorized            // 179, 219 — not authorized
	errInternal                 // 131 — Twitter internal error
	errForbidden                // 403 — forbidden (no recognized error code)
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

// APIError represents a Twitter/X API error with classification.
type APIError struct {
	Status  int
	Codes   []int
	Class   errorClass
	Message string
	Err     error
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("twitter: HTTP %d: class %d", e.Status, e.Class)
}

// Unwrap returns the underlying cause, if any.
func (e *APIError) Unwrap() error { return e.Err }

func extractErrorCodes(body []byte) []int {
	var errResp struct {
		Errors []struct {
			Code int `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &errResp) != nil || len(errResp.Errors) == 0 {
		return nil
	}
	codes := make([]int, 0, len(errResp.Errors))
	for _, e := range errResp.Errors {
		codes = append(codes, e.Code)
	}
	return codes
}

// newAPIError builds an APIError from a response. It preserves the existing
// classifyError mapping for known JSON codes and folds HTTP 429 / 403 into the
// rate-limit / forbidden classes when no recognized code is present.
func newAPIError(status int, body []byte, hdrs map[string]string) *APIError {
	e := &APIError{
		Status: status,
		Codes:  extractErrorCodes(body),
		Class:  classifyError(body, hdrs),
	}
	switch {
	case e.Class == errNone && status == 429:
		e.Class = errRateLimited
	case e.Class == errNone && status == 403:
		e.Class = errForbidden
	}
	return e
}

// IsRateLimited reports a rate-limit condition (HTTP 429).
func IsRateLimited(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errRateLimited
	}
	return false
}

// IsBanned reports an account-banned condition (code 88).
func IsBanned(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errBanned
	}
	return false
}

// IsForbidden reports a 403 response without a recognized JSON code.
func IsForbidden(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errForbidden
	}
	return false
}

// IsSuspended reports an account-suspended condition (code 64).
func IsSuspended(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errSuspended
	}
	return false
}

// IsLocked reports an account-locked condition (code 326).
func IsLocked(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errLocked
	}
	return false
}

// IsCSRF reports a CSRF token mismatch (code 353).
func IsCSRF(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errCSRF
	}
	return false
}

// IsAuthExpired reports an expired-authentication condition (code 32).
func IsAuthExpired(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errAuthExpired
	}
	return false
}

// IsBlocked reports a blocked-from-action condition (code 161).
func IsBlocked(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errBlocked
	}
	return false
}

// IsNotAuthorized reports a not-authorized condition (codes 179 / 219).
func IsNotAuthorized(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errNotAuthorized
	}
	return false
}

// IsInternal reports a Twitter internal error (code 131).
func IsInternal(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errInternal
	}
	return false
}

// parseRateLimitReset parses the X-Rate-Limit-Reset unix timestamp header.
// Falls back to 15 minutes from now if missing or invalid.
func parseRateLimitReset(v string) time.Time {
	if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(ts, 0)
	}
	return time.Now().Add(15 * time.Minute)
}
