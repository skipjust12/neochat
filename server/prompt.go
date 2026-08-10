package server

import (
	"fmt"
	"os"
	"strings"
)

// LoadSystemPrompt reads the chat system prompt from a plain text/markdown
// file (prompts/chat_system_prompt.md) and returns it trimmed. This is the
// prompt handle prepends to every generation call, ahead of conversation
// history and the user's new message -- see handle's SystemPrompt usage.
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
