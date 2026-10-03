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
	"log"
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

	// DebugToolEvents logs every raw server-tool event and tool_call delta
	// (truncated). Polza doesn't document the event shape, so this is how to
	// see what actually arrives when the web-activity list looks wrong.
	DebugToolEvents bool

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

	// Web access (see WebTools): Polza server tools the model may call,
	// the cap on how many tool calls one answer may make, or a search Polza
	// runs before the model for models without tool calling.
	Tools            []polzaServerTool `json:"tools,omitempty"`
	MaxToolCalls     int               `json:"max_tool_calls,omitempty"`
	WebSearchOptions *struct{}         `json:"web_search_options,omitempty"`
	// Provider routing. Like OpenRouter, Polza by default sends a request
	// to a provider even if it doesn't support some of its parameters and
	// drops those -- for tools that means a model that never sees them and
	// says it has no web access. require_parameters rules such providers
	// out; if the request is then refused, it is retried without it, and
	// then without web tools (see server.routeAndCall).
	Provider *polzaProviderPrefs `json:"provider,omitempty"`
}

type polzaProviderPrefs struct {
	RequireParameters bool `json:"require_parameters"`
}

type polzaServerTool struct {
	Type       string         `json:"type"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

type polzaStreamOptions struct {
	IncludeUsage            bool `json:"include_usage"`
	IncludeServerToolEvents bool `json:"include_server_tool_events,omitempty"`
}

type polzaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ServerToolUse    *struct {
		WebSearchRequests int `json:"web_search_requests"`
		WebFetchRequests  int `json:"web_fetch_requests"`
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
		} `json:"message"`
	} `json:"choices"`
	Usage    polzaUsage  `json:"usage"`
	Error    *polzaError `json:"error"`
	Provider string      `json:"provider"`
}

// polzaStreamChunk is one `data: {...}` line of the SSE stream. Reasoning
// models also send delta.reasoning chunks; those are thinking text, not
// the answer, and are deliberately not forwarded.
type polzaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content     string          `json:"content"`
			Annotations json.RawMessage `json:"annotations"`
			ToolCalls   []struct {
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
	} `json:"choices"`
	Usage    *polzaUsage `json:"usage"`
	Error    *polzaError `json:"error"`
	Provider string      `json:"provider"`
	// Polza stores server-tool loop events here when
	// include_server_tool_events is set; see parseServerToolEvents.
	Polza json.RawMessage `json:"polza"`
}

func (c *PolzaClient) buildRequest(ctx context.Context, apiModelID string, messages []Message, stream bool) polzaChatRequest {
	req := polzaChatRequest{Model: apiModelID, MaxTokens: outputLimit(ctx, c.MaxTokens), Stream: stream}
	if stream {
		req.StreamOptions = &polzaStreamOptions{IncludeUsage: true}
	}
	if web := WebToolsFromContext(ctx); web.Tools {
		req.Tools = []polzaServerTool{
			{Type: "polza:web_search", Parameters: map[string]any{"max_results": WebSearchMaxResults, "max_uses": WebSearchMaxUses}},
			{Type: "polza:web_fetch", Parameters: map[string]any{"max_uses": WebFetchMaxUses, "max_characters": WebFetchMaxCharacters}},
			{Type: "polza:datetime"},
		}
		req.MaxToolCalls = WebMaxToolCalls
		if !web.AnyProvider {
			req.Provider = &polzaProviderPrefs{RequireParameters: true}
		}
		if req.StreamOptions != nil {
			req.StreamOptions.IncludeServerToolEvents = true
		}
	} else if web.SearchFirst {
		req.WebSearchOptions = &struct{}{}
	}
	if r, ok := reasoningFromContext(ctx); ok {
		if r.Adaptive {
			req.Reasoning = &polzaReasoning{Type: "adaptive", EffortLevel: r.Effort}
		} else {
			req.Reasoning = &polzaReasoning{Effort: r.Effort}
		}
	}
	for _, m := range messages {
		msg := polzaChatMessage{Role: m.Role, Content: m.Content}
		if len(m.Parts) > 0 {
			msg.Content = encodeParts(m.Parts)
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

	result := GenerateResult{
		Text:         resp.Choices[0].Message.Content,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
		Citations:    parseCitations(resp.Choices[0].Message.Annotations),
	}
	if use := resp.Usage.ServerToolUse; use != nil {
		result.WebSearches, result.WebFetches = use.WebSearchRequests, use.WebFetchRequests
	}
	logWebCall(ctx, apiModelID, resp.Provider, result)
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
		toolCalls := map[int]*streamedToolCall{}
		// toolSinceText marks a tool step after some answer text: the model
		// said something like "let me check", searched, and is now going on.
		// The two pieces get a paragraph break instead of running together.
		toolSinceText := false
		emitTool := func(event ToolEvent) bool {
			final.ToolEvents = append(final.ToolEvents, event)
			if text.Len() > 0 {
				toolSinceText = true
			}
			return send(StreamChunk{Tool: &event})
		}

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
			if len(chunk.Polza) > 0 {
				if c.DebugToolEvents {
					log.Printf("provider: polza server-tool event: %s", clip(string(chunk.Polza), 2000))
				}
				for _, event := range parseServerToolEvents(chunk.Polza) {
					if !emitTool(event) {
						return
					}
				}
			}
			if len(chunk.Choices) > 0 {
				choice := chunk.Choices[0]
				for _, call := range choice.Delta.ToolCalls {
					if c.DebugToolEvents {
						log.Printf("provider: polza tool_call delta: index=%d id=%s name=%s args=%s", call.Index, call.ID, call.Function.Name, clip(call.Function.Arguments, 500))
					}
					acc := toolCalls[call.Index]
					if acc == nil {
						acc = &streamedToolCall{}
						toolCalls[call.Index] = acc
					}
					if call.ID != "" {
						acc.id = call.ID
					}
					acc.name += call.Function.Name
					acc.arguments += call.Function.Arguments
					if event, ok := acc.event(); ok && !emitTool(event) {
						return
					}
				}
				final.Citations = appendCitations(final.Citations, parseCitations(choice.Delta.Annotations))
				if choice.Message != nil {
					final.Citations = appendCitations(final.Citations, parseCitations(choice.Message.Annotations))
				}
			}
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				delta := chunk.Choices[0].Delta.Content
				if toolSinceText {
					delta = "\n\n" + strings.TrimLeft(delta, " ")
					toolSinceText = false
				}
				text.WriteString(delta)
				if !send(StreamChunk{Delta: delta}) {
					return
				}
			}
			if chunk.Usage != nil {
				inputTokens = chunk.Usage.PromptTokens
				outputTokens = chunk.Usage.CompletionTokens
				if use := chunk.Usage.ServerToolUse; use != nil {
					final.WebSearches, final.WebFetches = use.WebSearchRequests, use.WebFetchRequests
				}
			}
		}
		if err := scanner.Err(); err != nil {
			send(StreamChunk{Err: fmt.Errorf("provider: read polza stream: %w", err)})
			return
		}

		final.Text, final.InputTokens, final.OutputTokens = text.String(), inputTokens, outputTokens
		logWebCall(ctx, apiModelID, servedBy, final)
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
