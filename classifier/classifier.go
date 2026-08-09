// Package classifier turns a user message into a router.ClassifierOutput
// by calling a cheap model with the system prompt in
// prompts/classifier_system_prompt.md.
package classifier

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"neochat/provider"
	"neochat/router"
)

// Classifier calls one provider.Client/model pair with the classifier
// system prompt and parses its reply into a router.ClassifierOutput.
type Classifier struct {
	Client       provider.Client
	APIModelID   string
	SystemPrompt string
}

// New builds a Classifier from an already-loaded system prompt (see
// LoadSystemPrompt).
func New(client provider.Client, apiModelID, systemPrompt string) Classifier {
	return Classifier{Client: client, APIModelID: apiModelID, SystemPrompt: systemPrompt}
}

// Classify sends userMessage to the classifier model and parses its JSON
// reply. The system prompt tells the model to return only a JSON object
// with nothing around it, but real models occasionally wrap it in a
// ```json fence anyway -- stripFences defends against that specific,
// observed failure mode rather than guessing at others.
func (c Classifier) Classify(ctx context.Context, userMessage string) (router.ClassifierOutput, error) {
	result, err := c.Client.Generate(ctx, c.APIModelID, []provider.Message{
		{Role: "system", Content: c.SystemPrompt},
		{Role: "user", Content: userMessage},
	})
	if err != nil {
		return router.ClassifierOutput{}, fmt.Errorf("classifier: generate: %w", err)
	}

	var out router.ClassifierOutput
	if err := json.Unmarshal([]byte(stripFences(result.Text)), &out); err != nil {
		return router.ClassifierOutput{}, fmt.Errorf("classifier: parse model reply as JSON: %w (reply=%q)", err, result.Text)
	}
	return out, nil
}

// stripFences removes a wrapping ```json ... ``` or ``` ... ``` fence, if
// present, and trims surrounding whitespace. Returns the input unchanged
// if there's no fence to strip.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
