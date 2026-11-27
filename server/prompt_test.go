package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStylesKeepLinuxToolsWithoutWindowsOrMCPGuidance(t *testing.T) {
	for name, prompt := range LoadSystemPrompts("../prompts") {
		for _, removed := range []string{"windows", "powershell", "mcp"} {
			if strings.Contains(strings.ToLower(prompt), removed) {
				t.Errorf("%s still includes %s guidance", name, removed)
			}
		}
		for _, retained := range []string{"bash", "Python", "run_command", "run_script", "timeout_seconds", "send_file", "show_image", "non-zero exit code", "working directory", "read_file", "write_file", "list_dir"} {
			if !strings.Contains(prompt, retained) {
				t.Errorf("%s lost %s guidance", name, retained)
			}
		}
	}
}

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
