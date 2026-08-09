package moderation

import (
	"fmt"
	"os"
	"strings"
)

// LoadSystemPrompt reads the moderation system prompt from a markdown file
// (prompts/moderation_system_prompt.md).
func LoadSystemPrompt(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("moderation: read system prompt file: %w", err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("moderation: system prompt file %q is empty", path)
	}
	return prompt, nil
}
