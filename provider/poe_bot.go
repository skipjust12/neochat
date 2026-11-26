package provider

// Native Bot Query v1.2 follows poe-platform/fastapi_poe's client and types.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// BotAccessDenied is a pre-generation policy refusal, not an internal failure.
type BotAccessDenied struct{ Model string }

func (e *BotAccessDenied) Error() string {
	return fmt.Sprintf("Poe: bot %q does not support API access. Use a public prompt bot based on this model and add its link in Settings.", e.Model)
}

type BotServiceError struct{ Reference string }

func (e *BotServiceError) Error() string {
	message := "Poe returned an internal server error. The request was not automatically repeated."
	if e.Reference != "" {
		message += " Reference: " + e.Reference
	}
	return message
}

var poeErrorReference = regexp.MustCompile(`(?i)(?:error_id|err_id):\s*([a-f0-9-]{16,64})`)

type PoeAttachment struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Name        string `json:"name"`
}
type botUploadKey struct{}
type botUploadCache struct {
	mu    sync.Mutex
	files map[[32]byte]PoeAttachment
}

// Reuse an upload during one authenticated request's tool rounds only.
func WithPoeRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, botUploadKey{}, &botUploadCache{files: map[[32]byte]PoeAttachment{}})
}
func (c *PoeClient) cachedBotPart(ctx context.Context, p Part) (PoeAttachment, error) {
	cache, _ := ctx.Value(botUploadKey{}).(*botUploadCache)
	if cache == nil {
		return c.uploadBotPart(ctx, p)
	}
	key, err := c.key(ctx)
	if err != nil {
		return PoeAttachment{}, err
	}
	hash := sha256.New()
	hash.Write([]byte(key + "\x00" + p.MIME + "\x00" + p.Name + "\x00"))
	hash.Write(p.Data)
	var id [32]byte
	copy(id[:], hash.Sum(nil))
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if a, ok := cache.files[id]; ok {
		return a, nil
	}
	a, err := c.uploadBotPart(ctx, p)
	if err == nil {
		cache.files[id] = a
	}
	return a, err
}

type botParamsKey struct{}

func WithPoeBotParameters(ctx context.Context, p map[string]any) context.Context {
	return context.WithValue(ctx, botParamsKey{}, p)
}
func (c *PoeClient) botHTTP() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{CheckRedirect: func(r *http.Request, v []*http.Request) error { return http.ErrUseLastResponse }}
}
func (c *PoeClient) uploadBotPart(ctx context.Context, p Part) (PoeAttachment, error) {
	key, err := c.key(ctx)
	if err != nil {
		return PoeAttachment{}, err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	name := filepath.Base(p.Name)
	if name == "." || name == "" {
		name = "attachment"
	}
	headers := textproto.MIMEHeader{}
	headers.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": name}))
	kind := p.MIME
	if kind == "" {
		kind = "application/octet-stream"
	}
	headers.Set("Content-Type", kind)
	part, err := writer.CreatePart(headers)
	if err != nil {
		return PoeAttachment{}, err
	}
	if _, err = part.Write(p.Data); err != nil {
		return PoeAttachment{}, err
	}
	if err = writer.Close(); err != nil {
		return PoeAttachment{}, err
	}
	endpoint := c.UploadURL
	if endpoint == "" {
		endpoint = "https://www.quora.com/poe_api/file_upload_3RD_PARTY_POST"
	}
	request, err := http.NewRequestWithContext(ctx, "POST", endpoint, &body)
	if err != nil {
		return PoeAttachment{}, err
	}
	request.Header.Set("Authorization", key)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := c.botHTTP().Do(request)
	if err != nil {
		return PoeAttachment{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return PoeAttachment{}, &StatusError{StatusCode: response.StatusCode, Body: "Poe attachment upload failed"}
	}
	var wire struct {
		URL  string `json:"attachment_url"`
		MIME string `json:"mime_type"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&wire); err != nil {
		return PoeAttachment{}, err
	}
	u, err := url.Parse(wire.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return PoeAttachment{}, fmt.Errorf("invalid Poe upload response")
	}
	return PoeAttachment{URL: wire.URL, ContentType: wire.MIME, Name: name}, nil
}
func (c *PoeClient) botRequest(ctx context.Context, model string, messages []Message) (map[string]any, error) {
	query := []any{}
	calls := []any{}
	results := []any{}
	callNames := map[string]string{}
	// The SDK keeps the query before the latest tool round unchanged and puts
	// that round in top-level tool_calls/tool_results. Earlier rounds retain
	// their calls/results on the respective history messages.
	latest := -1
	for i, m := range messages {
		if len(m.ToolCalls) > 0 {
			latest = i
		}
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != "tool" {
		latest = -1
	}
	for i, m := range messages {
		role := m.Role
		if role == "assistant" {
			role = "bot"
		}
		attachments := []PoeAttachment{}
		text := m.Content
		for _, p := range m.Parts {
			if p.Type == PartText {
				if text == "" {
					text = p.Text
				}
				continue
			}
			a, err := c.cachedBotPart(ctx, p)
			if err != nil {
				return nil, err
			}
			attachments = append(attachments, a)
		}
		wireCalls := []any{}
		for _, call := range m.ToolCalls {
			callNames[call.ID] = call.Name
			wireCalls = append(wireCalls, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
		}
		if i == latest {
			calls = wireCalls
			// Match the SDK: the latest assistant tool turn is represented
			// by top-level tool_calls, not a second bot turn in query.
			continue
		}
		if role == "tool" && latest >= 0 && i > latest {
			results = append(results, map[string]any{"role": "tool", "name": callNames[m.ToolCallID], "tool_call_id": m.ToolCallID, "content": text})
			// Bot Query tool results are text. Keep the actual picture as a
			// supported ProtocolMessage attachment in query, alongside the
			// current tool_calls/tool_results rather than dropping its bytes.
			if len(attachments) > 0 {
				query = append(query, map[string]any{"role": "user", "content": "Image opened by " + callNames[m.ToolCallID] + " (" + m.ToolCallID + ") for your inspection: " + text, "content_type": "text/markdown", "attachments": attachments})
			}
			continue
		}
		// Historical calls are rendered as ordinary context above. Their
		// results must likewise be ordinary context: ProtocolMessage has no
		// tool_call_id field, so a historical role=tool becomes an orphaned
		// tool result when the bot forwards it to its underlying model.
		if role == "tool" {
			role = "user"
		}
		turn := map[string]any{"role": role, "content": text, "content_type": "text/markdown", "attachments": attachments}
		if len(wireCalls) > 0 && i != latest {
			encoded, _ := json.Marshal(wireCalls)
			turn["content"] = text + "\nPrevious tool calls: " + string(encoded)
		}
		if m.ToolCallID != "" {
			turn["content"] = "Tool result " + callNames[m.ToolCallID] + " (" + m.ToolCallID + "): " + text
		}
		query = append(query, turn)
	}
	params := map[string]any{}
	if limit := outputLimit(ctx, c.MaxTokens); limit > 0 {
		params["max_tokens"] = limit
	}
	for k, v := range c.chatRequest(ctx, model, nil, false) {
		if k != "model" && k != "messages" && k != "stream" && k != "tools" && k != "max_tokens" {
			params[k] = v
		}
	}
	if p, ok := ctx.Value(botParamsKey{}).(map[string]any); ok {
		for k, v := range p {
			params[k] = v
		}
	}
	if hasPoeWeb(ctx) {
		params["web_search"] = true
	}
	if len(query) > 0 {
		query[len(query)-1].(map[string]any)["parameters"] = params
	}
	req := map[string]any{"version": "1.2", "type": "query", "query": query, "user_id": "", "conversation_id": "", "message_id": "", "skip_system_prompt": true}
	tools := []any{}
	for _, tool := range functionToolsFromContext(ctx) {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters}})
	}
	if len(tools) > 0 {
		req["tools"] = tools
	}
	if len(calls) > 0 {
		req["tool_calls"] = calls
		req["tool_results"] = results
	}
	return req, nil
}
func (c *PoeClient) streamBot(ctx context.Context, model string, messages []Message) (<-chan StreamChunk, error) {
	model = c.resolveBot(model)
	if !poeHandlePattern.MatchString(model) {
		return nil, fmt.Errorf("invalid Poe bot handle")
	}
	timeout := c.StreamTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	payload, err := c.botRequest(ctx, model, messages)
	if err != nil {
		cancel()
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		cancel()
		return nil, err
	}
	base := c.BotBaseURL
	if base == "" {
		base = "https://api.poe.com/bot"
	}
	request, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/"+url.PathEscape(model), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	key, err := c.key(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	response, err := c.botHTTP().Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	if response.StatusCode != 200 {
		defer response.Body.Close()
		defer cancel()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, &StatusError{StatusCode: response.StatusCode, Body: string(data)}
	}
	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		defer cancel()
		defer response.Body.Close()
		send := func(chunk StreamChunk) bool {
			select {
			case ch <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		result := GenerateResult{Poe: true, UsageEstimated: true}
		filter := poeThinkingFilter{}
		emitText := func(text string, final bool) bool {
			answer, reasoning := filter.push(text, final)
			result.Text += answer
			result.Reasoning += reasoning
			if answer == "" && reasoning == "" {
				return true
			}
			return send(StreamChunk{Delta: answer, Reasoning: reasoning})
		}
		toolCalls := map[int]ToolCall{}
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 65536), 4<<20)
		event := ""
		data := []string{}
		finished := false
		terminated := false
		dispatch := func() bool {
			raw := strings.Join(data, "\n")
			data = nil
			if event == "done" {
				finished = true
				return false
			}
			if raw == "" {
				return true
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &wire); err != nil {
				send(StreamChunk{Err: fmt.Errorf("invalid Poe Bot Query %s event", event)})
				return false
			}
			switch event {
			case "done":
				finished = true
				return false
			case "error":
				var message string
				json.Unmarshal(wire["text"], &message)
				if message == "" {
					message = "Poe Bot Query refused the request"
				}
				if strings.EqualFold(strings.TrimSpace(message), "This bot does not support API access") && result.Text == "" && result.Reasoning == "" && len(result.Attachments) == 0 && len(toolCalls) == 0 {
					send(StreamChunk{Err: &BotAccessDenied{Model: model}})
				} else if strings.HasPrefix(strings.ToLower(strings.TrimSpace(message)), "internal server error") {
					reference := ""
					if match := poeErrorReference.FindStringSubmatch(message); match != nil {
						reference = match[1]
					}
					send(StreamChunk{Err: &BotServiceError{Reference: reference}})
				} else {
					send(StreamChunk{Err: fmt.Errorf("Poe Bot Query: %s", strings.ReplaceAll(message, key, "[redacted]"))})
				}
				return false
			case "file":
				var a PoeAttachment
				if err := json.Unmarshal([]byte(raw), &a); err != nil || a.URL == "" {
					send(StreamChunk{Err: fmt.Errorf("invalid Poe file event")})
					return false
				}
				result.Attachments = append(result.Attachments, a)
			case "text", "replace_response":
				var text string
				if json.Unmarshal(wire["text"], &text) != nil {
					send(StreamChunk{Err: fmt.Errorf("invalid Poe text event")})
					return false
				}
				if event == "replace_response" {
					filter = poeThinkingFilter{}
					answer, reasoning := filter.push(text, true)
					result.Text = answer
					if reasoning != "" && !strings.Contains(result.Reasoning, reasoning) {
						result.Reasoning += reasoning
						if !send(StreamChunk{Reasoning: reasoning}) {
							return false
						}
					}
					return send(StreamChunk{Reset: true, Delta: answer})
				}
				return emitText(text, false)
			case "json":
				if strings.EqualFold(model, "Perplexity-Search") && wire["choices"] == nil {
					return emitText(string(raw), false)
				}
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content   string `json:"content"`
							Reasoning string `json:"reasoning_content"`
							Calls     []struct {
								Index    int    `json:"index"`
								ID       string `json:"id"`
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
					send(StreamChunk{Err: err})
					return false
				}
				for _, choice := range chunk.Choices {
					d := choice.Delta
					result.Reasoning += d.Reasoning
					if d.Reasoning != "" && !send(StreamChunk{Reasoning: d.Reasoning}) {
						return false
					}
					if !emitText(d.Content, false) {
						return false
					}
					for _, delta := range d.Calls {
						call := toolCalls[delta.Index]
						if delta.ID != "" {
							call.ID = delta.ID
						}
						if delta.Function.Name != "" {
							call.Name = delta.Function.Name
						}
						call.Arguments += delta.Function.Arguments
						toolCalls[delta.Index] = call
						if fn := toolCallProgressFromContext(ctx); fn != nil {
							fn(ToolCallProgress{Index: delta.Index, Name: call.Name, Arguments: call.Arguments})
						}
					}
				}
			}
			return true
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if !dispatch() {
					terminated = true
					break
				}
				event = ""
				continue
			}
			if value, ok := strings.CutPrefix(line, "event:"); ok {
				event = strings.TrimSpace(value)
			}
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimSpace(value))
			}
		}
		if !terminated && len(data) > 0 {
			if !dispatch() {
				terminated = true
			}
		}
		if terminated && !finished {
			return
		}
		if !finished {
			if err := scanner.Err(); err != nil {
				send(StreamChunk{Err: err})
			} else if ctx.Err() != nil {
				send(StreamChunk{Err: ctx.Err()})
			} else {
				send(StreamChunk{Err: fmt.Errorf("Poe Bot Query ended without done")})
			}
			return
		}
		if !emitText("", true) {
			return
		}
		indices := []int{}
		for index := range toolCalls {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		for _, index := range indices {
			call := toolCalls[index]
			if call.ID == "" || call.Name == "" {
				send(StreamChunk{Err: fmt.Errorf("incomplete Poe tool call")})
				return
			}
			result.ToolCalls = append(result.ToolCalls, call)
		}
		result.FinishReason = "stop"
		if len(result.ToolCalls) > 0 {
			result.FinishReason = "tool_calls"
		}
		// Bot Query does not report token/cache usage. Mark conservative local
		// estimates explicitly; never invent provider-measured cache counters.
		result.InputTokens = len(body)
		result.OutputTokens = len(result.Text) + len(result.Reasoning) + 1
		for _, call := range result.ToolCalls {
			result.OutputTokens += len(call.Arguments) + len(call.Name)
		}
		send(StreamChunk{Done: true, Final: result})
	}()
	return ch, nil
}
func (c *PoeClient) generateBot(ctx context.Context, model string, messages []Message) (GenerateResult, error) {
	ch, err := c.streamBot(ctx, model, messages)
	if err != nil {
		return GenerateResult{}, err
	}
	for chunk := range ch {
		if chunk.Err != nil {
			return GenerateResult{}, chunk.Err
		}
		if chunk.Done {
			return chunk.Final, nil
		}
	}
	return GenerateResult{}, fmt.Errorf("Poe Bot Query returned no final response")
}

// Surface a proven pre-generation refusal before the public stream starts.
// Every other accepted stream remains non-replayable.
func (c *PoeClient) startBotStream(ctx context.Context, model string, messages []Message) (<-chan StreamChunk, error) {
	source, err := c.streamBot(ctx, model, messages)
	if err != nil {
		return nil, err
	}
	first, ok := <-source
	if !ok {
		return nil, fmt.Errorf("Poe Bot Query closed without a response")
	}
	if _, denied := first.Err.(*BotAccessDenied); denied {
		return nil, first.Err
	}
	out := make(chan StreamChunk, 1)
	out <- first
	go func() {
		defer close(out)
		for chunk := range source {
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
