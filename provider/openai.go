package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAIClient calls the OpenAI Chat Completions API directly. It is the
// first concrete Client implementation -- a second vendor (Anthropic,
// Google, ...) is a sibling file implementing the same interface, not a
// rewrite of this one.
type OpenAIClient struct {
	apiKey     string
	baseURL    string // overridable for tests; defaults to the real API
	httpClient *http.Client
}

// NewOpenAIClient builds a client against the real OpenAI API. apiKey must
// be non-empty -- callers get it from an environment variable
// (OPENAI_API_KEY), never a hardcoded string or a committed file.
func NewOpenAIClient(apiKey string) *OpenAIClient {
	return &OpenAIClient{
		apiKey:     apiKey,
		baseURL:    "https://api.openai.com/v1",
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

type openAIChatRequest struct {
	Model    string              `json:"model"`
	Messages []openAIChatMessage `json:"messages"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message openAIChatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (c *OpenAIClient) Generate(ctx context.Context, apiModelID string, messages []Message) (GenerateResult, error) {
	reqBody := openAIChatRequest{Model: apiModelID}
	for _, m := range messages {
		reqBody.Messages = append(reqBody.Messages, openAIChatMessage{Role: m.Role, Content: m.Content})
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: marshal openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: build openai request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: openai request failed: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: read openai response: %w", err)
	}

	var resp openAIChatResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return GenerateResult{}, fmt.Errorf("provider: parse openai response (status %d): %w, body=%s", httpResp.StatusCode, err, respBody)
	}
	if resp.Error != nil {
		return GenerateResult{}, fmt.Errorf("provider: openai error (status %d, type=%s): %s", httpResp.StatusCode, resp.Error.Type, resp.Error.Message)
	}
	if httpResp.StatusCode != http.StatusOK {
		return GenerateResult{}, fmt.Errorf("provider: openai request failed with status %d: %s", httpResp.StatusCode, respBody)
	}
	if len(resp.Choices) == 0 {
		return GenerateResult{}, fmt.Errorf("provider: openai response has no choices (status %d): %s", httpResp.StatusCode, respBody)
	}

	return GenerateResult{
		Text:         resp.Choices[0].Message.Content,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}, nil
}
