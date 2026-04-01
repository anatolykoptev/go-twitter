package xpff

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerate_Format(t *testing.T) {
	g := New("v1%3A174849298500261196", "Mozilla/5.0 Test")
	header, err := g.Generate()
	require.NoError(t, err)
	assert.NotEmpty(t, header)

	raw, err := hex.DecodeString(header)
	require.NoError(t, err)

	// Min length: 12 (nonce) + 1 (min ciphertext) + 16 (tag) = 29 bytes
	assert.GreaterOrEqual(t, len(raw), 29)
}

func TestGenerate_Decryptable(t *testing.T) {
	guestID := "v1%3A174849298500261196"
	g := New(guestID, "Mozilla/5.0 Test")
	header, err := g.Generate()
	require.NoError(t, err)

	raw, err := hex.DecodeString(header)
	require.NoError(t, err)

	// Derive key same way
	combined := baseKey + guestID
	hash := sha256.Sum256([]byte(combined))

	block, err := aes.NewCipher(hash[:])
	require.NoError(t, err)

	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)

	nonceSize := gcm.NonceSize()
	nonce := raw[:nonceSize]
	ciphertextWithTag := raw[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, ciphertextWithTag, nil)
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(plaintext, &payload))

	nav, ok := payload["navigator_properties"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "false", nav["webdriver"])
	assert.Equal(t, "true", nav["hasBeenActive"])
	assert.Equal(t, "Mozilla/5.0 Test", nav["userAgent"])
	assert.NotNil(t, payload["created_at"])
}

func TestGenerate_Cached(t *testing.T) {
	g := New("v1%3A12345", "ua")
	h1, err := g.Generate()
	require.NoError(t, err)
	h2, err := g.Generate()
	require.NoError(t, err)
	assert.Equal(t, h1, h2)
}

func TestGenerateGuestID(t *testing.T) {
	id := GenerateGuestID()
	assert.Contains(t, id, "v1%3A")
	assert.Greater(t, len(id), 15)
}
