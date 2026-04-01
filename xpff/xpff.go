package xpff

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const baseKey = "0e6be1f1e21ffc33590b888fd4dc81b19713e570e805d4e5df80a493c9571a05"

// Generator produces x-xp-forwarded-for header values.
type Generator struct {
	guestID   string
	userAgent string
	key       [32]byte

	mu      sync.Mutex
	cached  string
	expires time.Time
}

// New creates a Generator for the given guest ID and user agent.
func New(guestID, userAgent string) *Generator {
	combined := baseKey + guestID
	key := sha256.Sum256([]byte(combined))
	return &Generator{
		guestID:   guestID,
		userAgent: userAgent,
		key:       key,
	}
}

// Generate returns a hex-encoded AES-GCM encrypted header value.
// Results are cached for 4 minutes (Twitter validates 5 min TTL).
func (g *Generator) Generate() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.cached != "" && time.Now().Before(g.expires) {
		return g.cached, nil
	}

	payload := fmt.Sprintf(
		`{"navigator_properties":{"hasBeenActive":"true","userAgent":%q,"webdriver":"false"},"created_at":%d}`,
		g.userAgent, time.Now().UnixMilli(),
	)

	block, err := aes.NewCipher(g.key[:])
	if err != nil {
		return "", fmt.Errorf("xpff: aes cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("xpff: gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("xpff: random nonce: %w", err)
	}

	sealed := gcm.Seal(nonce, nonce, []byte(payload), nil)
	// sealed = nonce || ciphertext || tag (GCM appends tag automatically)
	result := hex.EncodeToString(sealed)

	g.cached = result
	g.expires = time.Now().Add(4 * time.Minute)
	return result, nil
}

// GenerateGuestID creates a synthetic guest ID in Twitter's format.
func GenerateGuestID() string {
	ts := time.Now().UnixMilli()
	return fmt.Sprintf("v1%%3A%d", ts)
}
