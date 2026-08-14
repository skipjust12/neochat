package auth

import (
	"strings"
	"testing"
)

func TestGenerateToken_HasPrefixAndIsRandom(t *testing.T) {
	a, err := generateToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(a, tokenPrefix) {
		t.Errorf("generateToken() = %q, want prefix %q", a, tokenPrefix)
	}

	b, err := generateToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == b {
		t.Fatalf("two calls to generateToken returned the same value: %q", a)
	}
}

func TestHashToken_DeterministicAndDistinct(t *testing.T) {
	h1 := hashToken("nc_same-input")
	h2 := hashToken("nc_same-input")
	if h1 != h2 {
		t.Errorf("hashToken is not deterministic: %q != %q", h1, h2)
	}
	if hashToken("nc_a") == hashToken("nc_b") {
		t.Error("hashToken produced the same digest for two different inputs")
	}
	if strings.Contains(h1, "same-input") {
		t.Error("hashToken output contains the raw token text")
	}
}
