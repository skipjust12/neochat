package classifier

import (
	"fmt"
	"os"
	"strings"
)

// LoadSystemPrompt reads the classifier system prompt from a markdown file
// (prompts/classifier_system_prompt.md), stripping the leading title/intro
// so only the actual instructions remain. The file wraps the prompt in a
// ```json fenced schema block as documentation, so no fence-stripping
// happens here -- the whole file body (including that block) is the
// intended prompt.
func LoadSystemPrompt(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("classifier: read system prompt file: %w", err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("classifier: system prompt file %q is empty", path)
	}
	return prompt, nil
}
