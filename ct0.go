package twitter

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// GenerateCT0 generates a random 32-byte hex string for use as a ct0 CSRF token.
func GenerateCT0() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000000000000000000000000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// ct0MaxAge is the maximum age of a ct0 token before proactive rotation.
const ct0MaxAge = 4 * time.Hour

// extractCT0FromHeaders parses ct0 value from a set-cookie response header.
func extractCT0FromHeaders(headers map[string]string) string {
	cookie := headers["set-cookie"]
	if cookie == "" {
		return ""
	}
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "ct0=") {
			val := strings.TrimPrefix(part, "ct0=")
			if val != "" {
				return val
			}
		}
	}
	return ""
}
