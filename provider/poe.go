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

const PoeBaseURL = "https://api.poe.com/v1"

// PoeClient speaks Poe's Responses API. Credentials stay in the request
// context; no conversation or user-key cache is shared between callers.
type PoeModelInfo struct {
	CacheReadPrice     *float64
	CacheWritePrice    *float64
	NativeMessages     bool
	UseResponses       bool
	ReasoningParameter string
	ReasoningValues    []string
}

func (c *PoeClient) info(id string) PoeModelInfo {
	if c.ModelInfo != nil {
		return c.ModelInfo(id)
	}
	return PoeModelInfo{}
}
func hasPoeWeb(ctx context.Context) bool { _, ok := WebPluginFromContext(ctx); return ok }

type PoeClient struct {
	BaseURL        string
	MaxTokens      int
	RequestTimeout time.Duration
	StreamTimeout  time.Duration
	HTTP           *http.Client
	fallbackKey    string
	ModelInfo      func(string) PoeModelInfo
}

func NewPoeClient(key string) *PoeClient {
	return &PoeClient{BaseURL: PoeBaseURL, fallbackKey: strings.TrimSpace(key)}
}

func (c *PoeClient) key(ctx context.Context) (string, error) {
	if key := apiKeyFromContext(ctx); key != "" {
		return key, nil
	}
	if c.fallbackKey != "" {
		return c.fallbackKey, nil
	}
	return "", ErrMissingAPIKey
}

func (c *PoeClient) request(ctx context.Context, path string, payload any, stream bool) (*http.Response, context.CancelFunc, error) {
	key, err := c.key(ctx)
	if err != nil {
		return nil, nil, err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	timeout := c.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	if stream {
		timeout = c.StreamTimeout
		if timeout <= 0 {
			timeout = defaultStreamTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = PoeBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(data))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if path == "/messages" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safeClient.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		return nil, nil, &StatusError{StatusCode: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header), Body: string(body)}
	}
	return resp, cancel, nil
}

func (c *PoeClient) responseRequest(ctx context.Context, model string, messages []Message, stream bool) map[string]any {
	input := []any{}
	for _, m := range messages {
		if m.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
			continue
		}
		if m.Role == "assistant" && len(m.ReasoningDetails) > 0 {
			var reasoning []map[string]any
			if json.Unmarshal(m.ReasoningDetails, &reasoning) == nil {
				for _, item := range reasoning {
					if item["type"] == "reasoning" {
						input = append(input, item)
					}
				}
			}
		}
		if m.Content != "" || len(m.Parts) > 0 {
			content := any(m.Content)
			if len(m.Parts) > 0 {
				parts := []any{}
				for _, p := range m.Parts {
					switch p.Type {
					case PartText:
						parts = append(parts, map[string]any{"type": "input_text", "text": p.Text})
					case PartImage:
						parts = append(parts, map[string]any{"type": "input_image", "image_url": dataURL(p.MIME, p.Data)})
					default:
						parts = append(parts, map[string]any{"type": "input_file", "filename": p.Name, "file_data": dataURL(p.MIME, p.Data)})
					}
				}
				content = parts
			}
			input = append(input, map[string]any{"role": m.Role, "content": content})
		}
		for _, call := range m.ToolCalls {
			input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": call.Arguments})
		}
	}
	req := map[string]any{"model": model, "input": input, "stream": stream, "store": false, "include": []string{"reasoning.encrypted_content"}}
	if limit := outputLimit(ctx, c.MaxTokens); limit > 0 {
		req["max_output_tokens"] = limit
	}
	if r, ok := reasoningFromContext(ctx); ok {
		effort := r.Effort
		if r.Disabled {
			effort = "none"
		}
		req["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	tools := []any{}
	for _, t := range functionToolsFromContext(ctx) {
		tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Parameters, "strict": false})
	}
	if _, ok := WebPluginFromContext(ctx); ok {
		tools = append(tools, map[string]any{"type": "web_search_preview"})
		req["include"] = []string{"reasoning.encrypted_content", "web_search_call.action.sources"}
	}
	if len(tools) > 0 {
		req["tools"] = tools
	}
	return req
}

type poeResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Output []struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Refusal     string `json:"refusal"`
			Annotations []struct {
				Type  string `json:"type"`
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"annotations"`
		} `json:"content"`
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
		Action struct {
			Sources []struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"sources"`
		} `json:"action"`
		EncryptedContent string `json:"encrypted_content"`
	} `json:"output"`
	Usage *struct {
		Input   int `json:"input_tokens"`
		Output  int `json:"output_tokens"`
		Details *struct {
			Cached int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (r poeResponse) result() (GenerateResult, error) {
	if r.Error != nil {
		return GenerateResult{}, fmt.Errorf("Poe: %s", r.Error.Message)
	}
	if r.Status != "completed" && r.Status != "incomplete" {
		return GenerateResult{}, fmt.Errorf("Poe response ended with status %q", r.Status)
	}
	out := GenerateResult{FinishReason: "stop"}
	if r.Status == "incomplete" {
		out.FinishReason = "length"
	}
	reasoning := []any{}
	for _, item := range r.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					out.Text += part.Text
				}
				if part.Refusal != "" {
					out.Text += part.Refusal
				}
				for _, a := range part.Annotations {
					if a.Type == "url_citation" {
						out.Citations = append(out.Citations, WebResult{URL: a.URL, Title: a.Title})
					}
				}
			}
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
			out.FinishReason = "tool_calls"
		case "web_search_call":
			out.WebSearches++
			for _, source := range item.Action.Sources {
				out.Citations = append(out.Citations, WebResult{URL: source.URL, Title: source.Title})
			}
		case "reasoning":
			for _, s := range item.Summary {
				out.Reasoning += s.Text
			}
			reasoning = append(reasoning, map[string]any{"type": "reasoning", "id": item.ID, "summary": item.Summary, "encrypted_content": item.EncryptedContent})
		}
	}
	if len(reasoning) > 0 {
		out.ReasoningDetails, _ = json.Marshal(reasoning)
	}
	if r.Usage != nil {
		out.InputTokens = r.Usage.Input
		out.OutputTokens = r.Usage.Output
		if r.Usage.Details != nil {
			out.CachedTokens = r.Usage.Details.Cached
		}
	}
	return out, nil
}

func (c *PoeClient) Generate(ctx context.Context, model string, messages []Message) (GenerateResult, error) {
	model = poeModelName(model)
	if err := validatePoeMessages(messages); err != nil {
		return GenerateResult{}, err
	}
	if c.info(model).NativeMessages && !hasPoeWeb(ctx) && nativeClaudeInputs(messages) {
		if err := validateNativeToolInputs(messages); err != nil {
			return GenerateResult{}, err
		}
		return c.generateClaude(ctx, model, messages)
	}
	if !hasPoeWeb(ctx) && !c.info(model).UseResponses {
		return c.generateChat(ctx, model, messages)
	}
	resp, cancel, err := c.request(ctx, "/responses", c.responseRequest(ctx, model, messages, false), false)
	if err != nil {
		return GenerateResult{}, err
	}
	defer cancel()
	defer resp.Body.Close()
	var wire poeResponse
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&wire); err != nil {
		return GenerateResult{}, fmt.Errorf("Poe response: %w", err)
	}
	out, err := wire.result()
	c.decorate(model, &out)
	if paragraphBreakFromContext(ctx) && out.Text != "" {
		out.Text = "\n\n" + strings.TrimLeft(out.Text, " \n")
	}
	return out, err
}

func (c *PoeClient) GenerateStream(ctx context.Context, model string, messages []Message) (<-chan StreamChunk, error) {
	model = poeModelName(model)
	if err := validatePoeMessages(messages); err != nil {
		return nil, err
	}
	if c.info(model).NativeMessages && !hasPoeWeb(ctx) && nativeClaudeInputs(messages) {
		if err := validateNativeToolInputs(messages); err != nil {
			return nil, err
		}
		return c.streamClaude(ctx, model, messages)
	}
	if !hasPoeWeb(ctx) && !c.info(model).UseResponses {
		return c.streamChat(ctx, model, messages)
	}
	resp, cancel, err := c.request(ctx, "/responses", c.responseRequest(ctx, model, messages, true), true)
	if err != nil {
		return nil, err
	}
	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		defer cancel()
		defer resp.Body.Close()
		send := func(chunk StreamChunk) bool {
			select {
			case ch <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		prefix := paragraphBreakFromContext(ctx)
		emitted := false
		calls := map[string]struct {
			position   int
			name, args string
		}{}
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "" || data == "[DONE]" {
				continue
			}
			var event struct {
				Type   string `json:"type"`
				Delta  string `json:"delta"`
				ItemID string `json:"item_id"`
				Item   struct {
					ID        string `json:"id"`
					Type      string `json:"type"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"item"`
				Response poeResponse `json:"response"`
				Message  string      `json:"message"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				send(StreamChunk{Err: err})
				return
			}
			switch event.Type {
			case "response.output_text.delta":
				delta := event.Delta
				if prefix {
					delta = strings.TrimLeft(delta, " \n")
					if delta == "" {
						continue
					}
					delta = "\n\n" + delta
					prefix = false
				}
				emitted = emitted || delta != ""
				if !send(StreamChunk{Delta: delta}) {
					return
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				if !send(StreamChunk{Reasoning: event.Delta}) {
					return
				}
			case "response.output_item.added":
				if event.Item.Type == "function_call" {
					calls[event.Item.ID] = struct {
						position   int
						name, args string
					}{len(calls), event.Item.Name, event.Item.Arguments}
				}
			case "response.function_call_arguments.delta":
				call := calls[event.ItemID]
				call.args += event.Delta
				calls[event.ItemID] = call
				if fn := toolCallProgressFromContext(ctx); fn != nil {
					fn(ToolCallProgress{Index: call.position, Name: call.name, Arguments: call.args})
				}
			case "response.completed", "response.incomplete":
				out, err := event.Response.result()
				c.decorate(model, &out)
				if err != nil {
					send(StreamChunk{Err: err})
					return
				}
				if paragraphBreakFromContext(ctx) && out.Text != "" {
					out.Text = "\n\n" + strings.TrimLeft(out.Text, " \n")
				}
				if !emitted && out.Text != "" {
					if !send(StreamChunk{Delta: out.Text}) {
						return
					}
				}
				send(StreamChunk{Done: true, Final: out})
				return
			case "response.failed":
				_, err := event.Response.result()
				if err == nil {
					err = fmt.Errorf("Poe response failed")
				}
				send(StreamChunk{Err: err})
				return
			case "error":
				send(StreamChunk{Err: fmt.Errorf("Poe: %s", event.Message)})
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		send(StreamChunk{Err: err})
	}()
	return ch, nil
}

func (c *PoeClient) decorate(model string, out *GenerateResult) {
	info := c.info(model)
	out.Poe = true
	out.CacheReadPrice = info.CacheReadPrice
	out.CacheWritePrice = info.CacheWritePrice
	logCache(model, *out)
}

func nativeClaudeInputs(messages []Message) bool {
	for _, m := range messages {
		for _, p := range m.Parts {
			if p.Type == PartVideo || (p.Type == PartFile && p.MIME != "application/pdf" && p.MIME != "text/plain") {
				return false
			}
		}
	}
	return true
}
func validatePoeMessages(messages []Message) error {
	for _, m := range messages {
		for _, t := range m.ToolCalls {
			if t.ID == "" || t.Name == "" {
				return fmt.Errorf("Poe: invalid function call in history")
			}
		}
		for _, p := range m.Parts {
			switch p.Type {
			case PartText, PartImage, PartFile, PartVideo:
			default:
				return fmt.Errorf("Poe: unsupported attachment kind %q", p.Type)
			}
		}
	}
	return nil
}

func poeModelName(model string) string {
	model = strings.TrimSpace(model)
	prefix, tail, ok := strings.Cut(model, "/")
	if ok {
		switch strings.ToLower(prefix) {
		case "anthropic", "openai", "google", "x-ai", "xai", "moonshotai", "deepseek":
			return tail
		}
	}
	return model
}

// Native tool_use inputs must be objects. Compatibility transports retain the
// original argument string so NeoChat can send a tool error and let a bot fix it.
func validateNativeToolInputs(messages []Message) error {
	for _, m := range messages {
		for _, t := range m.ToolCalls {
			var args map[string]json.RawMessage
			if json.Unmarshal([]byte(t.Arguments), &args) != nil || args == nil {
				return fmt.Errorf("Poe: invalid native function input in history")
			}
		}
	}
	return nil
}
