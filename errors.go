package twitter

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound marks deterministic "the resource does not exist" outcomes —
// deleted tweets, suspended/unavailable handles. Pool callers treat it as a
// healthy-account response (no retry, no auth_error report): the credential
// worked; the thing asked for simply isn't there.
var ErrNotFound = errors.New("twitter: resource not found")

// ErrEndpoint marks endpoint-wide faults (HTTP 404, or 400 carrying a
// GraphQL-shaped errors body): the committed request shape is wrong for every
// account (stale queryId, missing xtid, feature mismatch), so the serving
// account must not be charged — reads rotate to the next account. Match with
// errors.Is.
var ErrEndpoint = errors.New("twitter: endpoint fault")

// ErrOverloaded marks X-side overload (HTTP 5xx, or a LoadShed error entry —
// code -1 / message containing "LoadShed"): retryable on another account with
// backoff, never charged to the serving account. Match with errors.Is.
var ErrOverloaded = errors.New("twitter: x overloaded")

// errorClass categorizes Twitter API error responses for targeted handling.
type errorClass int

const (
	errNone          errorClass = iota
	errBanned                   // 88 with requests still remaining — account banned / rate-limit abuse (durable)
	errRateLimited              // HTTP 429, or 88 with an exhausted window — rate limit (transient, per endpoint)
	errSuspended                // 64 — account suspended
	errLocked                   // 326 — account locked (captcha needed)
	errCSRF                     // 353 — csrf token mismatch
	errAuthExpired              // 32 — could not authenticate
	errBlocked                  // 161 — blocked from performing action
	errNotAuthorized            // 179, 219 — not authorized
	errInternal                 // 131 — Twitter internal error
	errForbidden                // 403 — forbidden (no recognized error code)
	errEndpoint                 // HTTP 404 or GraphQL-shaped 400 — endpoint-wide fault, not the account's
	errOverloaded               // HTTP 5xx or a LoadShed error entry — X shedding load

	// errClassCount is one past the last class; metrics_test iterates [0, errClassCount)
	// to prove every class has a twitter.outcome mapping. Keep it LAST.
	errClassCount
)

// Rate-limit response headers (lowercase, as the transport delivers them).
const (
	xRateLimitReset     = "x-rate-limit-reset"
	xRateLimitRemaining = "x-rate-limit-remaining"
)

// classifyCode88 separates the two meanings of code 88 ("Rate limit exceeded").
// An exhausted window (x-rate-limit-remaining == 0) or a response without the
// header is a plain per-endpoint rate limit; 88 while requests are still
// remaining is not explained by the window and is treated as a ban (twscrape
// queue_client.py does the same). An unparseable header counts as absent.
func classifyCode88(hdrs map[string]string) errorClass {
	if n, ok := rateLimitRemaining(hdrs); ok && n > 0 {
		return errBanned
	}
	return errRateLimited
}

// rateLimitRemaining parses the x-rate-limit-remaining header; ok is false
// when the header is absent or unparseable. Callers distinguishing "the
// window explains this response" (header present, any value) from "the
// response arrived with no rate-limit bookkeeping" must use this — an absent
// header and a parseable one are NOT interchangeable (see
// Account.noteRateLimited88).
func rateLimitRemaining(hdrs map[string]string) (int, bool) {
	n, err := strconv.Atoi(hdrs[xRateLimitRemaining])
	if err != nil {
		return 0, false
	}
	return n, true
}

// headerless88BanThreshold bounds the run of consecutive code-88 responses
// carrying no parseable x-rate-limit-remaining header that is still tolerated
// as per-endpoint rate limiting. A banned account answers 88 with NO
// rate-limit headers on every endpoint: without this escalation it is charged
// no failure, never benched, and reported as rate_limited — invisible. One
// header-less 88 can be an endpoint-window quirk (the header genuinely
// missing); three consecutive with no successful response in between is the
// ban signature — cheap to reach (one request per count) and short enough to
// bench a dead account before it burns a full cooldown cycle of retries.
const headerless88BanThreshold = 3

// classifyError inspects a response body (and its headers, for code 88) for
// known Twitter error codes.
func classifyError(body []byte, hdrs map[string]string) errorClass {
	var errResp struct {
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &errResp) != nil || len(errResp.Errors) == 0 {
		return errNone
	}

	for _, e := range errResp.Errors {
		// X sheds load as a code -1 / "LoadShed" error entry on any status.
		// It is endpoint-side, never attributable to the serving account.
		if e.Code == -1 || strings.Contains(e.Message, "LoadShed") {
			return errOverloaded
		}
		switch e.Code {
		case 88:
			return classifyCode88(hdrs)
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

// Is reports sentinel membership: ErrEndpoint matches class errEndpoint and
// ErrOverloaded matches errOverloaded, so errors.Is(err, ErrEndpoint) reaches
// through fmt.Errorf("...: %w") wrappers.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrEndpoint:
		return e.Class == errEndpoint
	case ErrOverloaded:
		return e.Class == errOverloaded
	}
	return false
}

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

// hasGraphQLErrors reports whether the body is a GraphQL-shaped error
// envelope ({"errors":[...]}, non-empty): X answering that shape proves the
// request reached the GraphQL layer, so the rejection is about the committed
// request shape, not the account.
func hasGraphQLErrors(body []byte) bool {
	var probe struct {
		Errors []json.RawMessage `json:"errors"`
	}
	return json.Unmarshal(body, &probe) == nil && len(probe.Errors) > 0
}

// newAPIError builds an APIError from a response. It preserves the existing
// classifyError mapping for known JSON codes and folds HTTP statuses into the
// endpoint / overload / rate-limit / forbidden classes.
//
// The endpoint-fault folds (404, GraphQL-shaped 400, 5xx) WIN over body codes:
// a 404 says the route itself is stale for every account (a stale queryId,
// missing xtid or feature mismatch, see #61), a 5xx says X's edge is broken —
// neither can be charged to the serving account regardless of what the body
// claims. The 429/403 folds only fill in the errNone gap as before.
func newAPIError(status int, body []byte, hdrs map[string]string) *APIError {
	e := &APIError{
		Status: status,
		Codes:  extractErrorCodes(body),
		Class:  classifyError(body, hdrs),
	}
	switch {
	case status == 404, status == 400 && hasGraphQLErrors(body):
		e.Class = errEndpoint
	case status >= 500 && status < 600:
		e.Class = errOverloaded
	case e.Class == errNone && status == 429:
		e.Class = errRateLimited
	case e.Class == errNone && status == 403:
		e.Class = errForbidden
	}
	return e
}

// IsRateLimited reports a rate-limit condition (HTTP 429, or code 88 with an
// exhausted window).
func IsRateLimited(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Class == errRateLimited
	}
	return false
}

// IsBanned reports an account-banned condition (code 88 while the rate-limit
// window still has requests remaining).
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

// IsEndpointFault reports an endpoint-wide fault — the failure belongs to the
// committed endpoint shape (404, GraphQL-shaped 400) or to X itself (5xx,
// LoadShed), never to the serving account. Covers ErrEndpoint and
// ErrOverloaded; callers must not charge the account or report it as an
// auth_error.
func IsEndpointFault(err error) bool {
	return errors.Is(err, ErrEndpoint) || errors.Is(err, ErrOverloaded)
}

// partialSuccessClass reports the error classes a 200-with-usable-data
// response may still succeed under (#61d): X-side classes only — 131
// internal, LoadShed/overload, endpoint faults. Account-dead verdicts (326
// locked, 64 suspended, 88 banned/rate-limited, 353 csrf, 32 auth-expired)
// must take their class arm even when data rides along: treating them as
// success would mark a dead account healthy and skip its captcha/suspension
// handling.
func partialSuccessClass(c errorClass) bool {
	switch c {
	case errInternal, errOverloaded, errEndpoint:
		return true
	}
	return false
}

// minRateLimitBlock floors a parsed reset: a reset already in the past (clock
// skew, or a reset that landed between the response and now) would otherwise
// mark nothing and let the same account be retried at once.
const minRateLimitBlock = time.Second

// parseRateLimitReset parses the X-Rate-Limit-Reset unix timestamp header.
// Falls back to 15 minutes from now if missing or invalid; a parsed time less
// than minRateLimitBlock away is raised to now+minRateLimitBlock.
func parseRateLimitReset(v string) time.Time {
	if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
		if floor := time.Now().Add(minRateLimitBlock); time.Unix(ts, 0).Before(floor) {
			return floor
		}
		return time.Unix(ts, 0)
	}
	return time.Now().Add(15 * time.Minute)
}
