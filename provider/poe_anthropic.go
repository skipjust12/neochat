package provider

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func claudeCache(blocks []map[string]any) {
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i]["type"] == "text" || blocks[i]["type"] == "image" {
			blocks[i]["cache_control"] = map[string]string{"type": "ephemeral"}
			return
		}
	}
}

func (c *PoeClient) claudeRequest(ctx context.Context, model string, messages []Message, stream bool) map[string]any {
	messages = toolImageMessages(messages)
	system := []map[string]any{}
	turns := []map[string]any{}
	cacheUsers := map[int]bool{}
	count := 0
	for i := len(messages) - 1; i >= 0 && count < 2; i-- {
		if messages[i].Role == "user" {
			cacheUsers[i] = true
			count++
		}
	}
	for index, m := range messages {
		if m.Role == "system" {
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, map[string]any{"type": "text", "text": m.Content})
			}
			continue
		}
		role := m.Role
		blocks := []map[string]any{}
		if role == "tool" {
			role = "user"
			blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content})
		} else {
			if role == "assistant" && len(m.ReasoningDetails) > 0 {
				var details []map[string]any
				if json.Unmarshal(m.ReasoningDetails, &details) == nil {
					for _, d := range details {
						if d["type"] == "thinking" || d["type"] == "redacted_thinking" {
							blocks = append(blocks, d)
						}
					}
				}
			}
			if len(m.Parts) > 0 {
				for _, p := range m.Parts {
					switch p.Type {
					case PartText:
						if p.Text != "" {
							blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
						}
					case PartImage:
						blocks = append(blocks, map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": p.MIME, "data": base64.StdEncoding.EncodeToString(p.Data)}})
					case PartFile:
						blocks = append(blocks, map[string]any{"type": "document", "title": p.Name, "source": map[string]string{"type": "base64", "media_type": p.MIME, "data": base64.StdEncoding.EncodeToString(p.Data)}})
					}
				}
			} else if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, call := range m.ToolCalls {
				var input any
				_ = json.Unmarshal([]byte(call.Arguments), &input)
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": input})
			}
		}
		if cacheUsers[index] {
			claudeCache(blocks)
		}
		if len(blocks) > 0 {
			turns = append(turns, map[string]any{"role": role, "content": blocks})
		}
	}
	claudeCache(system)
	limit := outputLimit(ctx, c.MaxTokens)
	if limit <= 0 {
		limit = 16000
	}
	req := map[string]any{"model": model, "system": system, "messages": turns, "max_tokens": limit, "stream": stream}
	tools := []any{}
	for _, t := range functionToolsFromContext(ctx) {
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Parameters})
	}
	if len(tools) > 0 {
		req["tools"] = tools
	}
	if r, ok := reasoningFromContext(ctx); ok {
		switch {
		case r.Disabled:
			req["thinking"] = map[string]any{"type": "disabled"}
		case r.Adaptive:
			req["thinking"] = map[string]any{"type": "adaptive"}
			req["output_config"] = map[string]any{"effort": r.Effort}
		default:
			budget := 4096
			switch r.Effort {
			case "low":
				budget = 1024
			case "high", "xhigh", "max":
				budget = 8192
			}
			if budget >= limit {
				budget = limit - 1
			}
			if budget >= 1024 {
				req["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
			}
		}
	}
	return req
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
}
type claudeUsage struct {
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
	Read   int `json:"cache_read_input_tokens"`
	Write  int `json:"cache_creation_input_tokens"`
}

func (u claudeUsage) apply(out *GenerateResult) {
	out.InputTokens = u.Input + u.Read + u.Write
	out.OutputTokens = u.Output
	out.CachedTokens = u.Read
	out.CacheWriteTokens = u.Write
}

type claudeResponse struct {
	Content []claudeBlock `json:"content"`
	Usage   claudeUsage   `json:"usage"`
	Stop    string        `json:"stop_reason"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (r claudeResponse) result() (GenerateResult, error) {
	out := GenerateResult{FinishReason: "stop"}
	if r.Error != nil {
		return out, fmt.Errorf("Poe: %s", r.Error.Message)
	}
	switch r.Stop {
	case "tool_use":
		out.FinishReason = "tool_calls"
	case "max_tokens":
		out.FinishReason = "length"
	}
	details := []claudeBlock{}
	for _, block := range r.Content {
		switch block.Type {
		case "text":
			out.Text += block.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: block.ID, Name: block.Name, Arguments: string(block.Input)})
		case "thinking", "redacted_thinking":
			out.Reasoning += block.Thinking
			details = append(details, block)
		}
	}
	if len(details) > 0 {
		out.ReasoningDetails, _ = json.Marshal(details)
	}
	r.Usage.apply(&out)
	return out, nil
}
func (c *PoeClient) generateClaude(ctx context.Context, model string, messages []Message) (GenerateResult, error) {
	resp, cancel, err := c.request(ctx, "/messages", c.claudeRequest(ctx, model, messages, false), false)
	if err != nil {
		return GenerateResult{}, err
	}
	defer cancel()
	defer resp.Body.Close()
	var wire claudeResponse
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&wire); err != nil {
		return GenerateResult{}, err
	}
	out, err := wire.result()
	c.decorate(model, &out)
	if paragraphBreakFromContext(ctx) && out.Text != "" {
		out.Text = "\n\n" + strings.TrimLeft(out.Text, " \n")
	}
	return out, err
}
func (c *PoeClient) streamClaude(ctx context.Context, model string, messages []Message) (<-chan StreamChunk, error) {
	resp, cancel, err := c.request(ctx, "/messages", c.claudeRequest(ctx, model, messages, true), true)
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
		wire := claudeResponse{}
		blocks := map[int]*claudeBlock{}
		order := []int{}
		positions := map[int]int{}
		arguments := map[int]string{}
		prefix := paragraphBreakFromContext(ctx)
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "" {
				continue
			}
			var event struct {
				Type    string         `json:"type"`
				Index   int            `json:"index"`
				Message claudeResponse `json:"message"`
				Block   claudeBlock    `json:"content_block"`
				Delta   struct {
					Text      string `json:"text"`
					Thinking  string `json:"thinking"`
					Signature string `json:"signature"`
					JSON      string `json:"partial_json"`
					Stop      string `json:"stop_reason"`
				} `json:"delta"`
				Usage claudeUsage `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				send(StreamChunk{Err: err})
				return
			}
			switch event.Type {
			case "message_start":
				wire.Usage = event.Message.Usage
			case "content_block_start":
				block := event.Block
				blocks[event.Index] = &block
				order = append(order, event.Index)
				if block.Type == "tool_use" {
					positions[event.Index] = len(positions)
				}
			case "content_block_delta":
				block := blocks[event.Index]
				if block == nil {
					send(StreamChunk{Err: fmt.Errorf("Poe: delta before content block")})
					return
				}
				if event.Delta.Text != "" {
					block.Text += event.Delta.Text
					delta := event.Delta.Text
					if prefix {
						delta = strings.TrimLeft(delta, " \n")
						if delta == "" {
							continue
						}
						delta = "\n\n" + delta
						prefix = false
					}
					if !send(StreamChunk{Delta: delta}) {
						return
					}
				}
				if event.Delta.Thinking != "" {
					block.Thinking += event.Delta.Thinking
					if !send(StreamChunk{Reasoning: event.Delta.Thinking}) {
						return
					}
				}
				block.Signature += event.Delta.Signature
				if event.Delta.JSON != "" {
					arguments[event.Index] += event.Delta.JSON
					if fn := toolCallProgressFromContext(ctx); fn != nil {
						fn(ToolCallProgress{Index: positions[event.Index], Name: block.Name, Arguments: arguments[event.Index]})
					}
				}
			case "message_delta":
				if event.Usage.Read > 0 {
					wire.Usage.Read = event.Usage.Read
				}
				if event.Usage.Write > 0 {
					wire.Usage.Write = event.Usage.Write
				}
				wire.Stop = event.Delta.Stop
				wire.Usage.Output = event.Usage.Output
			case "message_stop":
				for _, index := range order {
					block := *blocks[index]
					if args := arguments[index]; args != "" {
						if !json.Valid([]byte(args)) {
							send(StreamChunk{Err: fmt.Errorf("Poe: malformed tool arguments")})
							return
						}
						block.Input = json.RawMessage(args)
					}
					wire.Content = append(wire.Content, block)
				}
				out, err := wire.result()
				c.decorate(model, &out)
				if err != nil {
					send(StreamChunk{Err: err})
					return
				}
				if paragraphBreakFromContext(ctx) && out.Text != "" {
					out.Text = "\n\n" + strings.TrimLeft(out.Text, " \n")
				}
				send(StreamChunk{Done: true, Final: out})
				return
			case "error":
				message := "upstream stream failed"
				if event.Error != nil {
					message = event.Error.Message
				}
				send(StreamChunk{Err: fmt.Errorf("Poe: %s", message)})
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
