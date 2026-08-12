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

	// CostInputPerMTok/CostOutputPerMTok price Classify's real token usage
	// for cost_log (see Classify's returned *provider.GenerateResult and
	// README pre-launch checklist item 7). Zero value logs every call at
	// $0 rather than failing -- fine for tests, but a real deployment
	// should set these to match whatever model APIModelID actually is
	// (cmd/server/main.go's CLASSIFIER_COST_*_PER_MTOK), since nothing
	// here cross-checks the two stay in sync.
	CostInputPerMTok  float64
	CostOutputPerMTok float64
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
//
// The returned *provider.GenerateResult is real token usage for cost
// logging, nil only when Client.Generate itself never completed (no
// tokens were ever billed); it's still populated on a JSON-parse failure
// below, since that failure happens after a real, billed vendor call.
func (c Classifier) Classify(ctx context.Context, userMessage string) (router.ClassifierOutput, *provider.GenerateResult, error) {
	result, err := c.Client.Generate(ctx, c.APIModelID, []provider.Message{
		{Role: "system", Content: c.SystemPrompt},
		{Role: "user", Content: userMessage},
	})
	if err != nil {
		return router.ClassifierOutput{}, nil, fmt.Errorf("classifier: generate: %w", err)
	}

	var out router.ClassifierOutput
	if err := json.Unmarshal([]byte(stripFences(result.Text)), &out); err != nil {
		return router.ClassifierOutput{}, &result, fmt.Errorf("classifier: parse model reply as JSON: %w (reply=%q)", err, result.Text)
	}
	return out, &result, nil
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
