package twitter

import (
	"errors"
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
		{"IsRateLimited", IsRateLimited, &APIError{Status: 429, Class: errRateLimited}},
		{"IsBanned", IsBanned, &APIError{Status: 200, Class: errBanned, Codes: []int{88}}},
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
		{"IsBanned", IsBanned},
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
		{"rate limit code 88", 200, `{"errors":[{"code":88}]}`, errBanned, "IsBanned"},
		{"rate limit HTTP 429", 429, ``, errRateLimited, "IsRateLimited"},
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

func TestIsBannedAndRateLimited(t *testing.T) {
	banned := newAPIError(200, []byte(`{"errors":[{"code":88}]}`), nil)
	if !IsBanned(banned) {
		t.Fatal("IsBanned must be true for code 88")
	}
	if IsRateLimited(banned) {
		t.Fatal("IsRateLimited must be false for code 88 after split")
	}

	rateLimited := newAPIError(429, nil, nil)
	if !IsRateLimited(rateLimited) {
		t.Fatalf("IsRateLimited must be true for HTTP 429, got class %d", rateLimited.Class)
	}
	if IsBanned(rateLimited) {
		t.Fatal("IsBanned must be false for HTTP 429")
	}
}

func TestAPIError_DoubleWrap(t *testing.T) {
	cause := errors.New("underlying relogin error")
	tests := []struct {
		name      string
		pred      func(error) bool
		ae        *APIError
		wantCause error
	}{
		{"IsRateLimited", IsRateLimited, &APIError{Status: 429, Class: errRateLimited, Message: "429 rate limited"}, nil},
		{"IsSuspended", IsSuspended, &APIError{Status: 200, Class: errSuspended, Message: "account suspended"}, nil},
		{"IsAuthExpired", IsAuthExpired, &APIError{Status: 401, Class: errAuthExpired, Message: "relogin failed: " + cause.Error(), Err: cause}, cause},
	}

	endpoint := "TestEndpoint"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := fmt.Errorf("inner: %w", tt.ae)
			outer := fmt.Errorf("pool exhausted for %s (requires auth): %w", endpoint, inner)
			if !tt.pred(outer) {
				t.Errorf("%s must detect APIError through two wrappers", tt.name)
			}
			if tt.wantCause != nil && !errors.Is(outer, tt.wantCause) {
				t.Errorf("outer error must wrap cause %v", tt.wantCause)
			}
		})
	}
}

func TestAPIError_Error(t *testing.T) {
	msg := "exact message passthrough"
	ae := &APIError{Status: 429, Class: errRateLimited, Message: msg}
	if got := ae.Error(); got != msg {
		t.Fatalf("Error() = %q, want %q", got, msg)
	}

	ae2 := &APIError{Status: 403, Class: errForbidden}
	want := fmt.Sprintf("twitter: HTTP %d: class %d", ae2.Status, ae2.Class)
	if got := ae2.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
