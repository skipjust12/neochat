package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// openRouterStreamRequest mirrors openRouterChatRequest plus the two
// fields that turn streaming on: Stream itself, and StreamOptions'
// IncludeUsage, which asks OpenRouter to emit one extra chunk at the end
// carrying the real token usage (mirroring OpenAI's streaming contract) --
// without it, a streamed GenerateResult would have no token counts for
// router.ComputeCostUSD / limits.RecordThinkingMaxSpend to bill against.
type openRouterStreamRequest struct {
	Model         string                  `json:"model"`
	Messages      []openRouterChatMessage `json:"messages"`
	Stream        bool                    `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// openRouterStreamChunk is one `data: {...}` line of an OpenAI-compatible
// SSE chat-completions stream. Choices carries the incremental text
// (empty on the final usage-only chunk); Usage is nil on every chunk
// except that final one.
type openRouterStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// GenerateStream is Generate's streaming counterpart: same request, but
// with stream:true, parsing the vendor's `data: {...}` SSE lines as they
// arrive instead of waiting for one complete JSON body. The returned
// channel is fed by a background goroutine and closed once the stream
// ends, one way or another -- see StreamChunk's doc comment for the exact
// contract.
func (c *OpenRouterClient) GenerateStream(ctx context.Context, apiModelID string, messages []Message) (<-chan StreamChunk, error) {
	reqBody := openRouterStreamRequest{Model: apiModelID, Stream: true}
	reqBody.StreamOptions.IncludeUsage = true
	for _, m := range messages {
		reqBody.Messages = append(reqBody.Messages, openRouterChatMessage{Role: m.Role, Content: m.Content})
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("provider: marshal openrouter stream request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provider: build openrouter stream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.Referer != "" {
		httpReq.Header.Set("HTTP-Referer", c.Referer)
	}
	if c.Title != "" {
		httpReq.Header.Set("X-Title", c.Title)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider: openrouter stream request failed: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		respBody, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("provider: openrouter stream request failed with status %d: %s", httpResp.StatusCode, respBody)
	}

	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		defer httpResp.Body.Close()

		send := func(chunk StreamChunk) bool {
			select {
			case ch <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var text strings.Builder
		var inputTokens, outputTokens int

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			if data == "[DONE]" {
				break
			}

			var chunk openRouterStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				send(StreamChunk{Err: fmt.Errorf("provider: parse openrouter stream chunk: %w (data=%s)", err, data)})
				return
			}
			if chunk.Error != nil {
				send(StreamChunk{Err: fmt.Errorf("provider: openrouter stream error (code=%v): %s", chunk.Error.Code, chunk.Error.Message)})
				return
			}
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				delta := chunk.Choices[0].Delta.Content
				text.WriteString(delta)
				if !send(StreamChunk{Delta: delta}) {
					return
				}
			}
			if chunk.Usage != nil {
				inputTokens = chunk.Usage.PromptTokens
				outputTokens = chunk.Usage.CompletionTokens
			}
		}
		if err := scanner.Err(); err != nil {
			send(StreamChunk{Err: fmt.Errorf("provider: read openrouter stream: %w", err)})
			return
		}

		send(StreamChunk{Done: true, Final: GenerateResult{
			Text:         text.String(),
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
		}})
	}()

	return ch, nil
}
