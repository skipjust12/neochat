package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSystemPrompts_SkipsMissingAndEmptyPersonas(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Default.md"), []byte("be helpful"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Expert.md"), []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Friendly, Cynical, Direct: no file at all.

	prompts := LoadSystemPrompts(dir)

	if got, want := prompts["Default"], "be helpful"; got != want {
		t.Errorf("prompts[Default] = %q, want %q", got, want)
	}
	for _, name := range []string{"Expert", "Friendly", "Cynical", "Direct"} {
		if _, ok := prompts[name]; ok {
			t.Errorf("expected no entry for persona %q (empty or missing file), got %q", name, prompts[name])
		}
	}
	if len(prompts) != 1 {
		t.Errorf("expected exactly 1 loaded persona, got %d: %+v", len(prompts), prompts)
	}
}

func TestLoadSystemPrompts_AllFiveNamesConsidered(t *testing.T) {
	dir := t.TempDir()
	for _, name := range SystemPromptNames {
		if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte("prompt for "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	prompts := LoadSystemPrompts(dir)
	if len(prompts) != len(SystemPromptNames) {
		t.Fatalf("expected all %d personas loaded, got %d: %+v", len(SystemPromptNames), len(prompts), prompts)
	}
	for _, name := range SystemPromptNames {
		if prompts[name] != "prompt for "+name {
			t.Errorf("prompts[%s] = %q, want %q", name, prompts[name], "prompt for "+name)
		}
	}
}
