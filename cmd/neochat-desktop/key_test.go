package main

import (
	"strings"
	"testing"
)

func TestKeyProblem(t *testing.T) {
	good := "nc_" + strings.Repeat("ab12", 16)
	if p := keyProblem(cleanKey(` "Bearer ` + good + `" `)); p != "" {
		t.Fatalf("a good key pasted with quotes: %s", p)
	}
	for key, want := range map[string]string{
		"":                               "empty",
		"pza_abc":                        "Polza",
		"sk-123":                         "start with nc_",
		good[:40]:                        "cut off",
		"nc_" + strings.Repeat("zz", 32): "a-f only",
	} {
		if p := keyProblem(key); !strings.Contains(p, want) {
			t.Errorf("keyProblem(%q) = %q, want it to mention %q", key, p, want)
		}
	}
}
