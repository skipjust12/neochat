package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// SystemPromptNames are the personas a client can pick via
// chatRequest.Persona -- the (not yet built) UI offers exactly these five
// as a picker, matched exactly (case-sensitive) against the request
// field. "Default" is used whenever a request doesn't specify one.
var SystemPromptNames = []string{"Default", "Expert", "Friendly", "Cynical", "Direct"}

// LoadSystemPrompt reads one persona's system prompt from a plain
// text/markdown file and returns it trimmed. This is prepended to every
// generation call, ahead of conversation history and the user's new
// message -- see prepare's SystemPrompts lookup.
func LoadSystemPrompt(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("server: read system prompt file: %w", err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("server: system prompt file %q is empty", path)
	}
	return prompt, nil
}

// LoadSystemPrompts loads every persona in SystemPromptNames from
// dir/<Name>.md (e.g. prompts/Expert.md), for Server.SystemPrompts.
// Unlike LoadSystemPrompt, a missing or empty file here is not an error --
// expected while a persona's real prompt text hasn't been written yet --
// it's just skipped (logged, not fatal), so the returned map only
// contains personas that actually have prompt text right now. Selecting a
// persona with no entry sends no system message at all (see prepare).
func LoadSystemPrompts(dir string) map[string]string {
	prompts := make(map[string]string, len(SystemPromptNames))
	for _, name := range SystemPromptNames {
		prompt, err := LoadSystemPrompt(filepath.Join(dir, name+".md"))
		if err != nil {
			log.Printf("server: persona %q has no prompt yet, will send no system message when selected: %v", name, err)
			continue
		}
		prompts[name] = prompt
	}
	return prompts
}
