package provider

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// WebTools asks the vendor to give the model web access for one call.
//
// With Tools, the model gets Polza's server tools (polza:web_search,
// polza:web_fetch, polza:datetime): it decides when to search or open a
// page, Polza runs the tool and feeds the result back, all inside the one
// streamed request. Models without tool calling can't do that; for them
// SearchFirst makes Polza search before the model runs (web_search_options)
// -- no page reading, but still fresh results.
type WebTools struct {
	Tools       bool
	SearchFirst bool
}

// Per-call limits for the server tools. 10 results per search is the
// ceiling polza:web_search accepts.
const (
	WebSearchMaxResults   = 10
	WebSearchMaxUses      = 5
	WebFetchMaxUses       = 5
	WebFetchMaxCharacters = 30000
	// WebMaxToolCalls bounds the whole tool loop (searches + fetches +
	// datetime) for one answer.
	WebMaxToolCalls = 12
)

type webToolsKey struct{}

// WithWebTools enables web access for the next generation call.
func WithWebTools(ctx context.Context, w WebTools) context.Context {
	return context.WithValue(ctx, webToolsKey{}, w)
}

// WebToolsFromContext reports the web access requested for this call.
func WebToolsFromContext(ctx context.Context) WebTools {
	w, _ := ctx.Value(webToolsKey{}).(WebTools)
	return w
}

// ToolEvent is one step of the server-side tool loop as seen in the stream:
// the model calling a tool (Phase "call", with Query or URL) or the tool's
// output (Phase "result", with Results for a search).
type ToolEvent struct {
	ID      string
	Tool    string // "web_search" | "web_fetch" | "datetime" | other
	Phase   string // "call" | "result"
	Query   string
	URL     string
	Title   string
	Results []WebResult
	Error   string
}

// WebResult is one search hit or one cited source.
type WebResult struct {
	Title   string
	URL     string
	Snippet string
}

// Polza documents that server-tool events arrive in a `polza` field of
// stream chunks (tool_call, tool_result, step_end, budget_exhausted) but not
// their exact shape, so parseServerToolEvents reads them by meaning rather
// than by a fixed schema: it looks for the tool name, a phase word, the
// query/url arguments and any list of {url, title} objects, wherever they
// sit in the object. Unrecognized events are dropped, never fatal.
func parseServerToolEvents(raw json.RawMessage) []ToolEvent {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	var out []ToolEvent
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			for _, item := range t {
				walk(item)
			}
		case map[string]any:
			if events, ok := t["events"]; ok {
				walk(events)
				return
			}
			if event, ok := toolEventFromObject(t); ok {
				out = append(out, event)
			}
		}
	}
	walk(value)
	return out
}

func toolEventFromObject(obj map[string]any) (ToolEvent, bool) {
	tool := toolName(obj)
	phase := ""
	for _, key := range []string{"type", "event", "kind", "phase", "status", "object"} {
		word := strings.ToLower(stringAt(obj, key))
		switch {
		case strings.Contains(word, "result") || strings.Contains(word, "output") || strings.Contains(word, "complete") || strings.Contains(word, "done"):
			phase = "result"
		case strings.Contains(word, "call") || strings.Contains(word, "start") || strings.Contains(word, "use"):
			phase = "call"
		}
		if phase != "" {
			break
		}
	}
	id := firstString(obj, "id", "call_id", "tool_call_id", "tool_use_id")
	for _, nested := range []string{"tool_call", "call", "tool_result", "result"} {
		if id == "" {
			id = firstString(mapAt(obj, nested), "id", "call_id", "tool_call_id", "tool_use_id")
		}
	}
	// A result may name only the call it answers; keep it so it can be
	// matched to that call by id.
	if phase == "" || (tool == "" && (phase != "result" || id == "")) {
		return ToolEvent{}, false
	}
	event := ToolEvent{Tool: tool, Phase: phase, ID: id}
	args := argumentsOf(obj)
	event.Query = firstString(args, "query", "q", "search_query")
	if event.Query == "" {
		if queries, ok := args["queries"].([]any); ok && len(queries) > 0 {
			event.Query, _ = queries[0].(string)
		}
	}
	event.URL = firstString(args, "url", "link")
	if phase == "result" {
		event.Results = findResults(obj)
		if errValue, ok := obj["error"]; ok && errValue != nil {
			if text, ok := errValue.(string); ok {
				event.Error = text
			} else if m, ok := errValue.(map[string]any); ok {
				event.Error = stringAt(m, "message")
			}
		}
		if event.Tool == "web_fetch" {
			event.Title = firstStringDeep(obj, "title")
			if event.URL == "" {
				event.URL = firstStringDeep(obj, "url")
			}
		}
	}
	return event, true
}

// toolName finds "polza:web_search"-like names anywhere near the top of the
// event and normalizes them to "web_search".
func toolName(obj map[string]any) string {
	var name string
	var look func(v any, depth int)
	look = func(v any, depth int) {
		if name != "" || depth > 3 {
			return
		}
		switch t := v.(type) {
		case string:
			lower := strings.ToLower(t)
			for _, known := range []string{"web_search", "web_fetch", "datetime", "image_search"} {
				if strings.Contains(lower, known) {
					name = known
					return
				}
			}
		case map[string]any:
			for _, key := range []string{"tool", "name", "tool_name", "tool_type", "type", "function", "tool_call", "call"} {
				if child, ok := t[key]; ok {
					look(child, depth+1)
				}
			}
		}
	}
	look(obj, 0)
	return name
}

// argumentsOf returns the tool arguments as an object, whether they are an
// object or a JSON string, and wherever the event nests them.
func argumentsOf(obj map[string]any) map[string]any {
	for _, holder := range []map[string]any{obj, mapAt(obj, "tool_call"), mapAt(obj, "call"), mapAt(obj, "function"), mapAt(mapAt(obj, "tool_call"), "function")} {
		for _, key := range []string{"arguments", "args", "input", "parameters", "params"} {
			switch v := holder[key].(type) {
			case map[string]any:
				return v
			case string:
				var parsed map[string]any
				if json.Unmarshal([]byte(v), &parsed) == nil {
					return parsed
				}
			}
		}
	}
	return obj
}

// findResults returns the first list of objects with a url/link, searching
// the event depth-first; a JSON string result is decoded first, and as a
// last resort plain-text output is mined for URLs.
func findResults(v any) []WebResult {
	switch t := v.(type) {
	case string:
		trimmed := strings.TrimSpace(t)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var decoded any
			if json.Unmarshal([]byte(trimmed), &decoded) == nil {
				return findResults(decoded)
			}
		}
		return nil
	case []any:
		var results []WebResult
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if url := firstString(m, "url", "link", "href"); url != "" {
					results = append(results, WebResult{
						Title:   firstString(m, "title", "name"),
						URL:     url,
						Snippet: clip(firstString(m, "snippet", "description", "content", "text", "excerpt"), 300),
					})
				}
			}
		}
		if len(results) > 0 {
			return results
		}
		for _, item := range t {
			if found := findResults(item); len(found) > 0 {
				return found
			}
		}
	case map[string]any:
		for _, key := range []string{"results", "result", "output", "content", "data", "items", "search_results"} {
			if child, ok := t[key]; ok {
				if found := findResults(child); len(found) > 0 {
					return found
				}
			}
		}
		if text := firstString(t, "result", "output", "content"); text != "" {
			return urlsIn(text)
		}
	}
	return nil
}

var urlPattern = regexp.MustCompile(`https?://[^\s<>"'\])]+`)

func urlsIn(text string) []WebResult {
	seen := map[string]bool{}
	var out []WebResult
	for _, url := range urlPattern.FindAllString(text, 20) {
		url = strings.TrimRight(url, ".,;:")
		if !seen[url] {
			seen[url] = true
			out = append(out, WebResult{URL: url})
		}
	}
	return out
}

// parseCitations reads url_citation annotations ({type, url_citation:{url,
// title}}), the one web-search output Polza documents for every engine.
func parseCitations(raw json.RawMessage) []WebResult {
	var annotations []struct {
		Type        string `json:"type"`
		URLCitation struct {
			URL     string `json:"url"`
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"url_citation"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &annotations) != nil {
		return nil
	}
	var out []WebResult
	for _, a := range annotations {
		if a.URLCitation.URL != "" {
			out = append(out, WebResult{Title: a.URLCitation.Title, URL: a.URLCitation.URL, Snippet: clip(a.URLCitation.Content, 300)})
		}
	}
	return out
}

func appendCitations(list []WebResult, more []WebResult) []WebResult {
	for _, c := range more {
		duplicate := false
		for _, existing := range list {
			if existing.URL == c.URL {
				duplicate = true
				break
			}
		}
		if !duplicate {
			list = append(list, c)
		}
	}
	return list
}

// streamedToolCall accumulates an OpenAI-style delta.tool_calls entry, in
// case Polza also surfaces server-tool calls that way.
type streamedToolCall struct {
	id, name, arguments string
	emitted             bool
}

func (c *streamedToolCall) event() (ToolEvent, bool) {
	if c.emitted {
		return ToolEvent{}, false
	}
	tool := toolName(map[string]any{"name": c.name})
	if tool == "" {
		return ToolEvent{}, false
	}
	var args map[string]any
	if json.Unmarshal([]byte(c.arguments), &args) != nil {
		return ToolEvent{}, false // arguments still streaming
	}
	c.emitted = true
	return ToolEvent{ID: c.id, Tool: tool, Phase: "call", Query: firstString(args, "query", "q", "search_query"), URL: firstString(args, "url", "link")}, true
}

func mapAt(obj map[string]any, key string) map[string]any {
	if obj == nil {
		return nil
	}
	m, _ := obj[key].(map[string]any)
	return m
}

func stringAt(obj map[string]any, key string) string {
	if obj == nil {
		return ""
	}
	s, _ := obj[key].(string)
	return s
}

func firstString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := strings.TrimSpace(stringAt(obj, key)); s != "" {
			return s
		}
	}
	return ""
}

func firstStringDeep(v any, key string) string {
	switch t := v.(type) {
	case map[string]any:
		if s := stringAt(t, key); s != "" {
			return s
		}
		for _, child := range t {
			if s := firstStringDeep(child, key); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range t {
			if s := firstStringDeep(child, key); s != "" {
				return s
			}
		}
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
