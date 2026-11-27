package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Bot parameters are flattened into the HTTP body, as OpenAI SDK extra_body
// does. Never use Polza's reasoning or plugins extensions on this endpoint.
func (c *PoeClient) chatRequest(ctx context.Context, model string, messages []Message, stream bool) map[string]any {
	messages = toolImageMessages(messages)
	turns := []any{}
	for _, m := range messages {
		turn := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.Parts) > 0 {
			parts := []any{}
			for _, p := range m.Parts {
				switch p.Type {
				case PartText:
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				case PartImage:
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": dataURL(p.MIME, p.Data)}})
				case PartFile, PartVideo:
					parts = append(parts, map[string]any{"type": "file", "file": map[string]string{"filename": p.Name, "file_data": dataURL(p.MIME, p.Data)}})
				}
			}
			turn["content"] = parts
		}
		if m.ToolCallID != "" {
			turn["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			calls := []any{}
			for _, t := range m.ToolCalls {
				calls = append(calls, map[string]any{"id": t.ID, "type": "function", "function": map[string]string{"name": t.Name, "arguments": t.Arguments}})
			}
			turn["tool_calls"] = calls
		}
		if m.Reasoning != "" {
			turn["reasoning_content"] = m.Reasoning
		}
		if len(m.ReasoningDetails) > 0 {
			turn["reasoning_details"] = m.ReasoningDetails
		}
		turns = append(turns, turn)
	}
	req := map[string]any{"model": model, "messages": turns, "stream": stream}
	if limit := outputLimit(ctx, c.MaxTokens); limit > 0 {
		req["max_tokens"] = limit
	}
	if stream {
		req["stream_options"] = map[string]bool{"include_usage": true}
	}
	tools := []any{}
	for _, t := range functionToolsFromContext(ctx) {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters}})
	}
	if len(tools) > 0 {
		req["tools"] = tools
	}
	info := c.requestModelInfo(ctx, model)
	// Poe bots can enable paid search by default. NeoChat owns the search loop
	// and supplies explicit web_search tools, so do not enable hidden searches.
	req["web_search"] = false
	if r, ok := reasoningFromContext(ctx); ok && info.ReasoningParameter != "" {
		value := r.Effort
		if r.Disabled {
			value = "none"
			if !hasString(info.ReasoningValues, value) {
				if hasString(info.ReasoningValues, "minimal") {
					value = "minimal"
				} else if len(info.ReasoningValues) > 0 {
					value = info.ReasoningValues[0]
				}
			}
		}
		if info.ReasoningParameter == "thinking_budget" {
			budget := 4096
			if r.Disabled {
				budget = 0
			} else {
				switch r.Effort {
				case "low":
					budget = 1024
				case "high", "xhigh", "max":
					budget = 8192
				}
				if limit := outputLimit(ctx, c.MaxTokens); limit > 0 && budget >= limit {
					budget = limit - 1
				}
				if budget < 1024 {
					budget = 0
				}
			}
			req["thinking_budget"] = budget
		} else if hasString(info.ReasoningValues, value) {
			req[info.ReasoningParameter] = value
		}
	}
	return req
}
func hasString(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

type poeChatMessage struct {
	Content   json.RawMessage `json:"content"`
	Reasoning string          `json:"reasoning_content"`
	Details   json.RawMessage `json:"reasoning_details"`
	Calls     []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func chatText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("Poe chat content: %w", err)
	}
	for _, p := range parts {
		if p.Type == "text" || p.Type == "output_text" {
			text += p.Text
		}
	}
	return text, nil
}

type poeChatResponse struct {
	Choices []struct {
		Message poeChatMessage `json:"message"`
		Delta   poeChatMessage `json:"delta"`
		Finish  string         `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		Input  int `json:"prompt_tokens"`
		Output int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *PoeClient) generateChat(ctx context.Context, model string, messages []Message) (GenerateResult, error) {
	resp, cancel, err := c.request(ctx, "/chat/completions", c.chatRequest(ctx, model, messages, false), false)
	if err != nil {
		return GenerateResult{}, err
	}
	defer cancel()
	defer resp.Body.Close()
	var wire poeChatResponse
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&wire); err != nil {
		return GenerateResult{}, err
	}
	if wire.Error != nil {
		return GenerateResult{}, fmt.Errorf("Poe: %s", wire.Error.Message)
	}
	if len(wire.Choices) != 1 || wire.Choices[0].Finish == "" {
		return GenerateResult{}, fmt.Errorf("Poe chat response is incomplete")
	}
	m := wire.Choices[0].Message
	text, err := chatText(m.Content)
	if err != nil {
		return GenerateResult{}, err
	}
	out := GenerateResult{Text: text, Reasoning: m.Reasoning, ReasoningDetails: m.Details, FinishReason: wire.Choices[0].Finish}
	for _, t := range m.Calls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: t.ID, Name: t.Function.Name, Arguments: t.Function.Arguments})
	}
	if wire.Usage != nil {
		out.InputTokens = wire.Usage.Input
		out.OutputTokens = wire.Usage.Output
	}
	if paragraphBreakFromContext(ctx) && out.Text != "" {
		out.Text = "\n\n" + strings.TrimLeft(out.Text, " \n")
	}
	c.decorate(model, &out)
	return out, nil
}
func (c *PoeClient) streamChat(ctx context.Context, model string, messages []Message) (<-chan StreamChunk, error) {
	resp, cancel, err := c.request(ctx, "/chat/completions", c.chatRequest(ctx, model, messages, true), true)
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
		out := GenerateResult{}
		calls := map[int]*ToolCall{}
		prefix := paragraphBreakFromContext(ctx)
		finished := false
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		for scanner.Scan() {
			line, ok := strings.CutPrefix(scanner.Text(), "data:")
			if !ok {
				continue
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if line == "[DONE]" {
				if !finished {
					send(StreamChunk{Err: io.ErrUnexpectedEOF})
					return
				}
				for i := 0; i < len(calls); i++ {
					t := calls[i]
					if t == nil || t.ID == "" || t.Name == "" {
						send(StreamChunk{Err: fmt.Errorf("Poe: incomplete function call")})
						return
					}
					out.ToolCalls = append(out.ToolCalls, *t)
				}
				c.decorate(model, &out)
				send(StreamChunk{Done: true, Final: out})
				return
			}
			var event poeChatResponse
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				send(StreamChunk{Err: err})
				return
			}
			if event.Error != nil {
				send(StreamChunk{Err: fmt.Errorf("Poe: %s", event.Error.Message)})
				return
			}
			if event.Usage != nil {
				out.InputTokens = event.Usage.Input
				out.OutputTokens = event.Usage.Output
			}
			for _, choice := range event.Choices {
				if choice.Finish != "" {
					finished = true
					out.FinishReason = choice.Finish
				}
				m := choice.Delta
				delta, err := chatText(m.Content)
				if err != nil {
					send(StreamChunk{Err: err})
					return
				}
				if prefix {
					delta = strings.TrimLeft(delta, " \n")
					if delta != "" {
						delta = "\n\n" + delta
						prefix = false
					}
				}
				if delta != "" {
					out.Text += delta
					if !send(StreamChunk{Delta: delta}) {
						return
					}
				}
				if m.Reasoning != "" {
					out.Reasoning += m.Reasoning
					if !send(StreamChunk{Reasoning: m.Reasoning}) {
						return
					}
				}
				if len(m.Details) > 0 && string(m.Details) != "null" {
					out.ReasoningDetails = m.Details
				}
				for _, t := range m.Calls {
					call := calls[t.Index]
					if call == nil {
						call = &ToolCall{}
						calls[t.Index] = call
					}
					if t.ID != "" {
						call.ID = t.ID
					}
					call.Name += t.Function.Name
					call.Arguments += t.Function.Arguments
					if fn := toolCallProgressFromContext(ctx); fn != nil {
						fn(ToolCallProgress{Index: t.Index, Name: call.Name, Arguments: call.Arguments})
					}
				}
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
