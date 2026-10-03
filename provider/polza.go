package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PolzaClient calls Polza AI (polza.ai), an aggregator serving every
// catalog vendor behind one OpenAI-compatible chat-completions endpoint
// (https://polza.ai/api/v1/chat/completions). It replaced the earlier
// OpenRouter client: same request/response shape, different base URL, and
// the API key now normally comes from the user making the request (see
// WithAPIKey) instead of one server-wide secret.
//
// Polza keys look like "pza_..." and are sent as "Authorization: Bearer
// <key>". Prices are billed in rubles on Polza's side; configs/models.json
// keeps USD figures converted from Polza's catalog for spend accounting.
type PolzaClient struct {
	// apiKey is an optional server-wide fallback, used only when the
	// request context carries no per-user key (WithAPIKey). Empty means
	// every call must bring its own key.
	apiKey string
	// BaseURL is the API root; NewPolzaClient sets PolzaBaseURL. Tests
	// point it at a local stand-in.
	BaseURL string

	// MaxTokens, if > 0, is sent as every request's max_tokens -- a hard
	// ceiling on how many output tokens a single Generate/GenerateStream
	// call can produce, regardless of the model's own (often much larger,
	// see configs/models.json's max_output_tokens) default ceiling. Zero
	// sends no max_tokens at all -- see cmd/server/main.go for how a real
	// deployment sets this.
	MaxTokens int

	// RequestTimeout bounds one non-streaming Generate call end to end.
	// Zero uses defaultRequestTimeout.
	RequestTimeout time.Duration

	// StreamTimeout bounds one GenerateStream call end to end -- from the
	// request going out to the final chunk arriving, not per chunk. It
	// exists only to stop a wedged stream from holding a connection and a
	// goroutine forever, so it is deliberately far more generous than
	// RequestTimeout: a "max"-effort generation legitimately runs for
	// minutes. Zero uses defaultStreamTimeout.
	StreamTimeout time.Duration

	httpClient *http.Client
}

// Timeouts are applied per call, through the request context, rather than
// with http.Client.Timeout. Client.Timeout covers reading the response
// body too, which for a streamed generation means the whole generation.
// Connection-level protection (dial, TLS handshake, expect-continue) is
// unaffected: those come from http.DefaultTransport, which a nil
// Transport uses.
const (
	defaultRequestTimeout = 60 * time.Second
	defaultStreamTimeout  = 10 * time.Minute
)

// PolzaBaseURL is Polza AI's OpenAI-compatible API root.
const PolzaBaseURL = "https://polza.ai/api/v1"

// ErrMissingAPIKey is returned when neither the request context nor the
// client carries a Polza key -- the user has not added one in Settings.
var ErrMissingAPIKey = errors.New("provider: no Polza AI API key configured")

// NewPolzaClient builds a client against the real Polza API. fallbackKey
// may be empty: per-user keys arrive through WithAPIKey.
func NewPolzaClient(fallbackKey string) *PolzaClient {
	return &PolzaClient{
		apiKey:  fallbackKey,
		BaseURL: PolzaBaseURL,
		// No Client.Timeout on purpose -- see the constants above.
		httpClient: &http.Client{},
	}
}

func (c *PolzaClient) requestTimeout() time.Duration {
	if c.RequestTimeout > 0 {
		return c.RequestTimeout
	}
	return defaultRequestTimeout
}

func (c *PolzaClient) streamTimeout() time.Duration {
	if c.StreamTimeout > 0 {
		return c.StreamTimeout
	}
	return defaultStreamTimeout
}

func (c *PolzaClient) resolveKey(ctx context.Context) (string, error) {
	if key := apiKeyFromContext(ctx); key != "" {
		return key, nil
	}
	if c.apiKey != "" {
		return c.apiKey, nil
	}
	return "", ErrMissingAPIKey
}

// polzaChatMessage.Content is a plain string, or an array of content parts
// for a multimodal message (see encodeParts).
type polzaChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	// Function calling: an assistant turn's calls, or which call a "tool"
	// turn answers. Reasoning travels back with an assistant's calls.
	ToolCalls        []polzaToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
}

type polzaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type polzaContentPart struct {
	Type     string             `json:"type"`
	Text     string             `json:"text,omitempty"`
	ImageURL *polzaImageURL     `json:"image_url,omitempty"`
	File     *polzaFileContents `json:"file,omitempty"`
}

type polzaImageURL struct {
	URL string `json:"url"`
}

type polzaFileContents struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

// encodeParts turns Parts into Polza's OpenAI-style content array: images
// as image_url with a base64 data URL, documents as file with file_data.
func encodeParts(parts []Part) []polzaContentPart {
	out := make([]polzaContentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case PartImage:
			out = append(out, polzaContentPart{Type: "image_url", ImageURL: &polzaImageURL{URL: dataURL(p.MIME, p.Data)}})
		case PartFile:
			out = append(out, polzaContentPart{Type: "file", File: &polzaFileContents{Filename: p.Name, FileData: dataURL(p.MIME, p.Data)}})
		default:
			out = append(out, polzaContentPart{Type: "text", Text: p.Text})
		}
	}
	return out
}

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// polzaReasoning is the body-level "reasoning" object. Polza ignores a
// top-level reasoning_effort field silently, so this object is the only
// form sent. Adaptive (Claude Opus 4.7 and newer) models take
// type=adaptive + effort_level; everything else takes effort.
type polzaReasoning struct {
	Type        string `json:"type,omitempty"`
	Effort      string `json:"effort,omitempty"`
	EffortLevel string `json:"effort_level,omitempty"`
}

type polzaChatRequest struct {
	Model     string             `json:"model"`
	Messages  []polzaChatMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens,omitempty"`
	Reasoning *polzaReasoning    `json:"reasoning,omitempty"`
	Stream    bool               `json:"stream,omitempty"`
	// StreamOptions.IncludeUsage asks for one extra chunk at the end
	// carrying real token usage -- without it a streamed GenerateResult
	// would have no token counts to bill against.
	StreamOptions *polzaStreamOptions `json:"stream_options,omitempty"`

	// Functions the model may call (WithFunctionTools) and server-side
	// plugins such as web search (WithWebPlugin).
	Tools   []polzaFunctionTool `json:"tools,omitempty"`
	Plugins []polzaPlugin       `json:"plugins,omitempty"`
}

type polzaFunctionTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

type polzaPlugin struct {
	ID           string `json:"id"`
	Engine       string `json:"engine,omitempty"`
	MaxResults   int    `json:"max_results,omitempty"`
	SearchPrompt string `json:"search_prompt,omitempty"`
}

type polzaStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type polzaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ServerToolUse    *struct {
		WebSearchRequests int `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

type polzaError struct {
	Message string `json:"message"`
	Code    any    `json:"code"` // string ("UNAUTHORIZED") or number depending on the failure
}

type polzaChatResponse struct {
	Choices []struct {
		Message struct {
			Content     string          `json:"content"`
			Annotations json.RawMessage `json:"annotations"`
			ToolCalls   []polzaToolCall `json:"tool_calls"`
			Reasoning   string          `json:"reasoning"`
		} `json:"message"`
		FinishReason     string            `json:"finish_reason"`
		ReasoningDetails []json.RawMessage `json:"reasoning_details"`
	} `json:"choices"`
	Usage    polzaUsage  `json:"usage"`
	Error    *polzaError `json:"error"`
	Provider string      `json:"provider"`
}

// polzaStreamChunk is one `data: {...}` line of the SSE stream. Reasoning
// models also send delta.reasoning chunks: thinking text, not the answer,
// so it is never forwarded as text -- only kept to send back with tool
// calls (see Message.Reasoning).
type polzaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string            `json:"content"`
			Reasoning        string            `json:"reasoning"`
			ReasoningDetails []json.RawMessage `json:"reasoning_details"`
			Annotations      json.RawMessage   `json:"annotations"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message *struct {
			Annotations json.RawMessage `json:"annotations"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage    *polzaUsage `json:"usage"`
	Error    *polzaError `json:"error"`
	Provider string      `json:"provider"`
}

func (c *PolzaClient) buildRequest(ctx context.Context, apiModelID string, messages []Message, stream bool) polzaChatRequest {
	req := polzaChatRequest{Model: apiModelID, MaxTokens: outputLimit(ctx, c.MaxTokens), Stream: stream}
	if stream {
		req.StreamOptions = &polzaStreamOptions{IncludeUsage: true}
	}
	for _, tool := range functionToolsFromContext(ctx) {
		def := polzaFunctionTool{Type: "function"}
		def.Function.Name, def.Function.Description, def.Function.Parameters = tool.Name, tool.Description, tool.Parameters
		req.Tools = append(req.Tools, def)
	}
	if p, ok := WebPluginFromContext(ctx); ok {
		req.Plugins = []polzaPlugin{{ID: "web", Engine: p.Engine, MaxResults: p.MaxResults, SearchPrompt: p.SearchPrompt}}
	}
	if r, ok := reasoningFromContext(ctx); ok {
		switch {
		case r.Disabled && r.Adaptive:
			req.Reasoning = &polzaReasoning{Type: "disabled"}
		case r.Disabled:
			req.Reasoning = &polzaReasoning{Effort: "none"}
		case r.Adaptive:
			req.Reasoning = &polzaReasoning{Type: "adaptive", EffortLevel: r.Effort}
		default:
			req.Reasoning = &polzaReasoning{Effort: r.Effort}
		}
	}
	for _, m := range messages {
		msg := polzaChatMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		if len(m.Parts) > 0 {
			msg.Content = encodeParts(m.Parts)
		}
		if len(m.ToolCalls) > 0 {
			if m.Content == "" {
				msg.Content = nil // null content: a turn that only calls tools
			}
			for _, call := range m.ToolCalls {
				wire := polzaToolCall{ID: call.ID, Type: "function"}
				wire.Function.Name, wire.Function.Arguments = call.Name, call.Arguments
				msg.ToolCalls = append(msg.ToolCalls, wire)
			}
			msg.Reasoning, msg.ReasoningDetails = m.Reasoning, m.ReasoningDetails
		}
		req.Messages = append(req.Messages, msg)
	}
	return req
}

func (c *PolzaClient) newHTTPRequest(ctx context.Context, key string, body []byte, stream bool) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+key)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	return httpReq, nil
}

func (c *PolzaClient) Generate(ctx context.Context, apiModelID string, messages []Message) (GenerateResult, error) {
	key, err := c.resolveKey(ctx)
	if err != nil {
		return GenerateResult{}, err
	}
	body, err := json.Marshal(c.buildRequest(ctx, apiModelID, messages, false))
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: marshal polza request: %w", err)
	}

	// Deadline on the caller's context rather than on the shared
	// http.Client, so it bounds this one call without also bounding
	// streamed ones (see defaultRequestTimeout).
	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()

	httpReq, err := c.newHTTPRequest(reqCtx, key, body, false)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: build polza request: %w", err)
	}
	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: polza request failed: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("provider: read polza response: %w", err)
	}

	// A non-2xx status becomes a *StatusError before anything else is
	// inspected, so RetryClient can tell a rate limit (worth another
	// attempt) from a bad request (never worth one) without parsing error
	// text.
	if httpResp.StatusCode != http.StatusOK {
		return GenerateResult{}, &StatusError{
			StatusCode: httpResp.StatusCode,
			RetryAfter: parseRetryAfter(httpResp.Header),
			Body:       string(respBody),
		}
	}

	var resp polzaChatResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return GenerateResult{}, fmt.Errorf("provider: parse polza response (status %d): %w, body=%s", httpResp.StatusCode, err, respBody)
	}
	if resp.Error != nil {
		return GenerateResult{}, fmt.Errorf("provider: polza error (status %d, code=%v): %s", httpResp.StatusCode, resp.Error.Code, resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return GenerateResult{}, fmt.Errorf("provider: polza response has no choices (status %d): %s", httpResp.StatusCode, respBody)
	}

	choice := resp.Choices[0]
	text := choice.Message.Content
	if paragraphBreakFromContext(ctx) {
		if text = strings.TrimLeft(text, " \n"); text != "" {
			text = "\n\n" + text
		}
	}
	result := GenerateResult{
		Text:         text,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
		Citations:    parseCitations(choice.Message.Annotations),
		FinishReason: choice.FinishReason,
		Reasoning:    choice.Message.Reasoning,
	}
	for _, call := range choice.Message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	var details reasoningDetails
	details.add(choice.ReasoningDetails)
	result.ReasoningDetails = details.raw()
	if use := resp.Usage.ServerToolUse; use != nil {
		result.WebSearches = use.WebSearchRequests
	}
	logWebPlugin(ctx, apiModelID, resp.Provider, result)
	return result, nil
}

// GenerateStream is Generate's streaming counterpart: same request, but
// with stream:true, parsing the vendor's `data: {...}` SSE lines as they
// arrive. The returned channel is fed by a background goroutine and closed
// once the stream ends -- see StreamChunk's doc comment for the contract.
// Canceling ctx (the user pressing Stop) aborts the upstream HTTP request,
// which is what stops generation on Polza's side.
func (c *PolzaClient) GenerateStream(ctx context.Context, apiModelID string, messages []Message) (<-chan StreamChunk, error) {
	key, err := c.resolveKey(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(c.buildRequest(ctx, apiModelID, messages, true))
	if err != nil {
		return nil, fmt.Errorf("provider: marshal polza stream request: %w", err)
	}

	// streamCtx outlives this function -- the reader goroutine below owns
	// it -- so cancel is deferred inside that goroutine, not here. Every
	// early return between here and launching it has to cancel explicitly.
	streamCtx, cancel := context.WithTimeout(ctx, c.streamTimeout())

	httpReq, err := c.newHTTPRequest(streamCtx, key, body, true)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("provider: build polza stream request: %w", err)
	}
	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("provider: polza stream request failed: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		respBody, _ := io.ReadAll(httpResp.Body)
		retryAfter := parseRetryAfter(httpResp.Header)
		cancel()
		// Returned before the stream channel exists, which is exactly the
		// window RetryClient is allowed to retry a stream in.
		return nil, &StatusError{StatusCode: httpResp.StatusCode, RetryAfter: retryAfter, Body: string(respBody)}
	}

	ch := make(chan StreamChunk)
	go func() {
		// Registered first so it runs last: the stream's context stays
		// alive for as long as the goroutine reading it does.
		defer cancel()
		defer close(ch)
		defer httpResp.Body.Close()

		send := func(chunk StreamChunk) bool {
			select {
			case ch <- chunk:
				return true
			case <-streamCtx.Done():
				return false
			}
		}

		// A panic here would otherwise escape this goroutine and take the
		// whole server process down, since it belongs to no request's
		// handler. Report it to the consumer as an ordinary stream error.
		defer func() {
			if rec := recover(); rec != nil {
				send(StreamChunk{Err: fmt.Errorf("provider: panic in polza stream reader: %v", rec)})
			}
		}()

		var text strings.Builder
		var inputTokens, outputTokens int
		var final GenerateResult
		var servedBy string
		var reasoning strings.Builder
		var details reasoningDetails
		var callOrder []int
		calls := map[int]*streamedToolCall{}
		// A call continuing an answer already on screen opens with a
		// paragraph break (see WithParagraphBreak).
		pendingBreak := paragraphBreakFromContext(ctx)

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				break
			}
			if data == "" {
				continue
			}

			var chunk polzaStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				send(StreamChunk{Err: fmt.Errorf("provider: parse polza stream chunk: %w (data=%s)", err, data)})
				return
			}
			if chunk.Error != nil {
				send(StreamChunk{Err: fmt.Errorf("provider: polza stream error (code=%v): %s", chunk.Error.Code, chunk.Error.Message)})
				return
			}
			if chunk.Provider != "" {
				servedBy = chunk.Provider
			}
			if len(chunk.Choices) > 0 {
				choice := chunk.Choices[0]
				for _, call := range choice.Delta.ToolCalls {
					acc := calls[call.Index]
					if acc == nil {
						acc = &streamedToolCall{}
						calls[call.Index] = acc
						callOrder = append(callOrder, call.Index)
					}
					if call.ID != "" {
						acc.id = call.ID
					}
					acc.name += call.Function.Name
					acc.arguments.WriteString(call.Function.Arguments)
				}
				reasoning.WriteString(choice.Delta.Reasoning)
				details.add(choice.Delta.ReasoningDetails)
				final.Citations = appendCitations(final.Citations, parseCitations(choice.Delta.Annotations))
				if choice.Message != nil {
					final.Citations = appendCitations(final.Citations, parseCitations(choice.Message.Annotations))
				}
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					final.FinishReason = *choice.FinishReason
				}
				delta := choice.Delta.Content
				if pendingBreak && delta != "" {
					if delta = strings.TrimLeft(delta, " \n"); delta != "" {
						delta = "\n\n" + delta
						pendingBreak = false
					}
				}
				if delta != "" {
					text.WriteString(delta)
					if !send(StreamChunk{Delta: delta}) {
						return
					}
				}
			}
			if chunk.Usage != nil {
				inputTokens = chunk.Usage.PromptTokens
				outputTokens = chunk.Usage.CompletionTokens
				if use := chunk.Usage.ServerToolUse; use != nil {
					final.WebSearches = use.WebSearchRequests
				}
			}
		}
		if err := scanner.Err(); err != nil {
			send(StreamChunk{Err: fmt.Errorf("provider: read polza stream: %w", err)})
			return
		}

		for _, index := range callOrder {
			acc := calls[index]
			final.ToolCalls = append(final.ToolCalls, ToolCall{ID: acc.id, Name: acc.name, Arguments: acc.arguments.String()})
		}
		final.Text, final.InputTokens, final.OutputTokens = text.String(), inputTokens, outputTokens
		final.Reasoning, final.ReasoningDetails = reasoning.String(), details.raw()
		logWebPlugin(ctx, apiModelID, servedBy, final)
		send(StreamChunk{Done: true, Final: final})
	}()

	return ch, nil
}

// VendorMessage extracts the human-readable message from a Polza error
// body ({"error":{"message":...}}), or "" if the body isn't one.
func VendorMessage(body string) string {
	var parsed struct {
		Error *polzaError `json:"error"`
	}
	if json.Unmarshal([]byte(body), &parsed) != nil || parsed.Error == nil {
		return ""
	}
	return strings.TrimSpace(parsed.Error.Message)
}
