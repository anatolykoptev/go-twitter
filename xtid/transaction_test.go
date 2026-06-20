package xtid

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

const userByScreenNamePath = "/i/api/graphql/x/UserByScreenName"

// fixedCT builds a ClientTransaction with deterministic now/randByte seams so
// GenerateID is reproducible. keyBytes length 6 -> a 28-byte payload -> a 38-char
// base64 string after '=' trimming.
func fixedCT() *ClientTransaction {
	return &ClientTransaction{
		keyBytes:     []byte{1, 2, 3, 4, 5, 6},
		animationKey: "abc123",
		now:          func() time.Time { return time.UnixMilli(1700000000000) },
		randByte:     func() byte { return 0x42 },
	}
}

func TestGenerateID_Structural(t *testing.T) {
	id := fixedCT().GenerateID("GET", userByScreenNamePath)

	if id == "" {
		t.Fatal("GenerateID returned empty")
	}
	if strings.HasSuffix(id, "=") {
		t.Fatalf("GenerateID = %q must be '='-trimmed", id)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9+/]+$`).MatchString(id) {
		t.Fatalf("GenerateID = %q is not base64-ish", id)
	}
	const wantLen = 38 // 28 payload bytes -> base64 40 chars -> 38 after trimming "=="
	if len(id) != wantLen {
		t.Fatalf("len(GenerateID) = %d want %d (id=%q)", len(id), wantLen, id)
	}
}

func TestGenerateID_DeterministicWithSeams(t *testing.T) {
	ct := fixedCT()
	first := ct.GenerateID("GET", userByScreenNamePath)
	second := ct.GenerateID("GET", userByScreenNamePath)
	if first != second {
		t.Fatalf("fixed seams must be deterministic: %q != %q", first, second)
	}
}

func TestGenerateID_RandByteWired(t *testing.T) {
	a := fixedCT()
	b := fixedCT()
	b.randByte = func() byte { return 0x99 } // different seed -> different id
	if a.GenerateID("GET", userByScreenNamePath) == b.GenerateID("GET", userByScreenNamePath) {
		t.Fatal("changing randByte must change the id (seam not wired)")
	}
}

func TestGenerateID_NowWired(t *testing.T) {
	a := fixedCT()
	b := fixedCT()
	b.now = func() time.Time { return time.UnixMilli(1800000000000) } // different clock
	if a.GenerateID("GET", userByScreenNamePath) == b.GenerateID("GET", userByScreenNamePath) {
		t.Fatal("changing now must change the id (seam not wired)")
	}
}

// TestGenerateID_ZeroValueSafe ensures a ClientTransaction not built via
// newClientTransaction (nil seams) still generates an id rather than panicking.
func TestGenerateID_ZeroValueSafe(t *testing.T) {
	ct := &ClientTransaction{keyBytes: []byte{1, 2, 3, 4, 5, 6}, animationKey: "x"}
	if id := ct.GenerateID("GET", userByScreenNamePath); id == "" {
		t.Fatal("zero-value ct produced empty id")
	}
}
