package summarizer

import (
	"fmt"
	"os"
	"strings"
)

// LoadSystemPrompt reads the summarizer system prompt from a markdown
// file (prompts/summarizer_system_prompt.md). Mirrors
// classifier.LoadSystemPrompt/moderation.LoadSystemPrompt.
func LoadSystemPrompt(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("summarizer: read system prompt file: %w", err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("summarizer: system prompt file %q is empty", path)
	}
	return prompt, nil
}
