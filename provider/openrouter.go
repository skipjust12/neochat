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

// OpenRouterClient calls the OpenRouter API (openrouter.ai), an
// aggregator proxying most vendors behind one OpenAI-compatible
// chat-completions endpoint -- see README's "Model access: OpenRouter at
// launch" decision. A direct-vendor Client (Anthropic, Google, ...) is a
// sibling file implementing the same Client interface, not a rewrite of
// this one, if/when a hybrid-sourcing setup needs one.
type OpenRouterClient struct {
	apiKey  string
	baseURL string // overridable for tests; defaults to the real API

	// Referer and Title are sent as optional OpenRouter-specific headers
	// (HTTP-Referer, X-Title) used for attribution/rankings on
	// openrouter.ai -- harmless to omit, so both default empty and are
	// only sent when set.
	Referer string
	Title   string

	httpClient *http.Client
}

// NewOpenRouterClient builds a client against the real OpenRouter API.
// apiKey must be non-empty -- callers get it from an environment variable
// (OPENROUTER_API_KEY), never a hardcoded string or a committed file.
func NewOpenRouterClient(apiKey string) *OpenRouterClient {
	return &OpenRouterClient{
		apiKey:     apiKey,
		baseURL:    "https://openrouter.ai/api/v1",
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// OpenRouter's request/response shapes are OpenAI-compatible, so these
// structs are unchanged from the earlier direct-OpenAI client -- only the
// base URL, auth header value, and these two optional headers differ.
type openRouterChatRequest struct {
	Model    string                  `json:"model"`
	Messages []openRouterChatMessage `json:"messages"`
}

type openRouterChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openRouterChatResponse struct {
	Choices []struct {
		Message openRouterChatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"` // OpenRouter uses a numeric or string error code depending on the failure -- kept loosely typed since it's only for the error message text below
	} `json:"error"`
}

func (c *OpenRouterClient) Generate(ctx context.Context, apiModelID string, messages []Message) (GenerateResult, error) {
	reqBody := openRouterChatRequest{Model: apiModelID}
	for _, m := range messages {
		reqBody.Messages = append(reqBody.Messages, openRouterChatMessage{Role: m.Role, Content: m.Content})
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: marshal openrouter request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: build openrouter request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.Referer != "" {
		httpReq.Header.Set("HTTP-Referer", c.Referer)
	}
	if c.Title != "" {
		httpReq.Header.Set("X-Title", c.Title)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: openrouter request failed: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: read openrouter response: %w", err)
	}

	var resp openRouterChatResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return GenerateResult{}, fmt.Errorf("provider: parse openrouter response (status %d): %w, body=%s", httpResp.StatusCode, err, respBody)
	}
	if resp.Error != nil {
		return GenerateResult{}, fmt.Errorf("provider: openrouter error (status %d, code=%v): %s", httpResp.StatusCode, resp.Error.Code, resp.Error.Message)
	}
	if httpResp.StatusCode != http.StatusOK {
		return GenerateResult{}, fmt.Errorf("provider: openrouter request failed with status %d: %s", httpResp.StatusCode, respBody)
	}
	if len(resp.Choices) == 0 {
		return GenerateResult{}, fmt.Errorf("provider: openrouter response has no choices (status %d): %s", httpResp.StatusCode, respBody)
	}

	return GenerateResult{
		Text:         resp.Choices[0].Message.Content,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}, nil
}
