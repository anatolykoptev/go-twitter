package twitter

import (
	"fmt"
	"testing"
	"time"
)

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected errorClass
	}{
		{"no errors", `{"data":{"user":{}}}`, errNone},
		{"empty errors", `{"errors":[]}`, errNone},
		{"banned 88", `{"errors":[{"code":88}]}`, errBanned},
		{"suspended 64", `{"errors":[{"code":64}]}`, errSuspended},
		{"locked 326", `{"errors":[{"code":326}]}`, errLocked},
		{"csrf 353", `{"errors":[{"code":353}]}`, errCSRF},
		{"auth expired 32", `{"errors":[{"code":32}]}`, errAuthExpired},
		{"blocked 161", `{"errors":[{"code":161}]}`, errBlocked},
		{"not authorized 179", `{"errors":[{"code":179}]}`, errNotAuthorized},
		{"not authorized 219", `{"errors":[{"code":219}]}`, errNotAuthorized},
		{"internal 131", `{"errors":[{"code":131}]}`, errInternal},
		{"unknown code", `{"errors":[{"code":999}]}`, errNone},
		{"invalid json", `{invalid`, errNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifyError([]byte(tt.body), nil)
			if result != tt.expected {
				t.Fatalf("classifyError(%s) = %d, want %d", tt.body, result, tt.expected)
			}
		})
	}
}

func TestAPIErrorPredicates_WrappedError(t *testing.T) {
	tests := []struct {
		name string
		pred func(error) bool
		ae   *APIError
	}{
		{"IsRateLimited", IsRateLimited, &APIError{Status: 429, Class: errBanned}},
		{"IsForbidden", IsForbidden, &APIError{Status: 403, Class: errForbidden}},
		{"IsSuspended", IsSuspended, &APIError{Status: 200, Class: errSuspended}},
		{"IsLocked", IsLocked, &APIError{Status: 200, Class: errLocked}},
		{"IsCSRF", IsCSRF, &APIError{Status: 401, Class: errCSRF}},
		{"IsAuthExpired", IsAuthExpired, &APIError{Status: 401, Class: errAuthExpired}},
		{"IsBlocked", IsBlocked, &APIError{Status: 200, Class: errBlocked}},
		{"IsNotAuthorized", IsNotAuthorized, &APIError{Status: 200, Class: errNotAuthorized}},
		{"IsInternal", IsInternal, &APIError{Status: 200, Class: errInternal}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := fmt.Errorf("pool request failed: %w", tt.ae)
			if !tt.pred(wrapped) {
				t.Errorf("%s must detect a wrapped APIError", tt.name)
			}
		})
	}
}

func TestAPIError_Classification(t *testing.T) {
	allPreds := []struct {
		name string
		pred func(error) bool
	}{
		{"IsRateLimited", IsRateLimited},
		{"IsForbidden", IsForbidden},
		{"IsSuspended", IsSuspended},
		{"IsLocked", IsLocked},
		{"IsCSRF", IsCSRF},
		{"IsAuthExpired", IsAuthExpired},
		{"IsBlocked", IsBlocked},
		{"IsNotAuthorized", IsNotAuthorized},
		{"IsInternal", IsInternal},
	}

	tests := []struct {
		name    string
		status  int
		body    string
		class   errorClass
		trueFor string
	}{
		{"rate limit code 88", 200, `{"errors":[{"code":88}]}`, errBanned, "IsRateLimited"},
		{"rate limit HTTP 429", 429, ``, errBanned, "IsRateLimited"},
		{"forbidden HTTP 403", 403, ``, errForbidden, "IsForbidden"},
		{"suspended 64", 200, `{"errors":[{"code":64}]}`, errSuspended, "IsSuspended"},
		{"locked 326", 200, `{"errors":[{"code":326}]}`, errLocked, "IsLocked"},
		{"csrf 353", 200, `{"errors":[{"code":353}]}`, errCSRF, "IsCSRF"},
		{"auth expired 32", 200, `{"errors":[{"code":32}]}`, errAuthExpired, "IsAuthExpired"},
		{"blocked 161", 200, `{"errors":[{"code":161}]}`, errBlocked, "IsBlocked"},
		{"not authorized 179", 200, `{"errors":[{"code":179}]}`, errNotAuthorized, "IsNotAuthorized"},
		{"not authorized 219", 200, `{"errors":[{"code":219}]}`, errNotAuthorized, "IsNotAuthorized"},
		{"internal 131", 200, `{"errors":[{"code":131}]}`, errInternal, "IsInternal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ae := newAPIError(tt.status, []byte(tt.body), nil)
			if ae.Class != tt.class {
				t.Fatalf("newAPIError class = %d, want %d", ae.Class, tt.class)
			}
			for _, p := range allPreds {
				got := p.pred(ae)
				if p.name == tt.trueFor {
					if !got {
						t.Errorf("%s must be true", p.name)
					}
					continue
				}
				if got {
					t.Errorf("%s must be false for %s", p.name, tt.name)
				}
			}
		})
	}
}

func TestParseRateLimitReset(t *testing.T) {
	// Valid timestamp
	now := time.Now()
	ts := now.Add(15 * time.Minute)
	result := parseRateLimitReset(time.Unix(ts.Unix(), 0).Format(""))
	// Should fallback since empty string
	if time.Until(result) < 14*time.Minute {
		t.Fatal("expected ~15min fallback")
	}

	// Invalid
	result = parseRateLimitReset("not-a-number")
	if time.Until(result) < 14*time.Minute {
		t.Fatal("expected ~15min fallback for invalid input")
	}
}
