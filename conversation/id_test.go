package conversation

import "testing"

func TestNewID_ReturnsNonEmptyUniqueIDs(t *testing.T) {
	a := NewID()
	b := NewID()
	if a == "" || b == "" {
		t.Fatal("expected non-empty IDs")
	}
	if a == b {
		t.Fatal("expected two calls to return different IDs")
	}
	if len(a) != 32 { // 16 bytes hex-encoded = 32 chars
		t.Errorf("len(NewID()) = %d, want 32", len(a))
	}
}
