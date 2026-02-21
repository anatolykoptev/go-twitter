package twitter

import (
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
