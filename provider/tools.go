package provider

import (
	"context"
	"encoding/json"
	"log"
	"strings"
)

// FunctionTool is a function the model may call during a generation. The
// vendor never runs it: the call comes back in GenerateResult.ToolCalls,
// the caller runs it and sends the result as a "tool" message (see
// Message.ToolCallID) in a follow-up call.
type FunctionTool struct {
	Name        string
	Description string
	// Parameters is the JSON Schema of the arguments object.
	Parameters map[string]any
}

type functionToolsKey struct{}

// WithFunctionTools offers tools to the model for the next generation call.
func WithFunctionTools(ctx context.Context, tools []FunctionTool) context.Context {
	return context.WithValue(ctx, functionToolsKey{}, tools)
}

func functionToolsFromContext(ctx context.Context) []FunctionTool {
	tools, _ := ctx.Value(functionToolsKey{}).([]FunctionTool)
	return tools
}

// WebPlugin asks Polza to search the web before the model runs (the "web"
// plugin, plugins:[{id:"web"}]) and put the results into its context;
// they come back as url_citation annotations (GenerateResult.Citations).
// Unlike Polza's server tools it is open to every account.
type WebPlugin struct {
	// Engine is "yandex" (Polza's own search, the default), or "native" /
	// "exa" (run by the model's provider).
	Engine string
	// MaxResults is 1-20; zero leaves Polza's default (5).
	MaxResults int
	// SearchPrompt is the query; empty makes Polza search for the last
	// user message.
	SearchPrompt string
}

// WebPluginMaxResults is the most results the web plugin returns.
const WebPluginMaxResults = 20

type webPluginKey struct{}

// WithWebPlugin enables the web plugin for the next generation call.
func WithWebPlugin(ctx context.Context, p WebPlugin) context.Context {
	return context.WithValue(ctx, webPluginKey{}, p)
}

// WebPluginFromContext reports the web plugin settings for this call.
func WebPluginFromContext(ctx context.Context) (WebPlugin, bool) {
	p, ok := ctx.Value(webPluginKey{}).(WebPlugin)
	return p, ok
}

type paragraphBreakKey struct{}

// WithParagraphBreak marks the next call as continuing an answer whose
// earlier text the user already has (the model wrote "let me check",
// called a tool, and is now going on): its text starts with a paragraph
// break instead of running on from the previous sentence.
func WithParagraphBreak(ctx context.Context) context.Context {
	return context.WithValue(ctx, paragraphBreakKey{}, true)
}

func paragraphBreakFromContext(ctx context.Context) bool {
	on, _ := ctx.Value(paragraphBreakKey{}).(bool)
	return on
}

// WebResult is one search hit or one cited source.
type WebResult struct {
	Title   string
	URL     string
	Snippet string
}

// parseCitations reads url_citation annotations ({type, url_citation:{url,
// title, content}}), which is how Polza returns web search results.
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

// streamedToolCall accumulates one OpenAI-style delta.tool_calls entry:
// the id and name arrive first, the arguments JSON in pieces.
type streamedToolCall struct {
	id, name  string
	arguments strings.Builder
}

// reasoningDetails merges streamed reasoning_details pieces into the
// list the vendor expects back: pieces of one entry share an index (or
// arrive in order without one) and carry the text in slices, with the
// signature on the last piece. Entries are kept as raw objects since
// their fields differ by vendor (text, summary, signature, data, ...).
type reasoningDetails struct {
	entries []map[string]any
	byIndex map[float64]int
}

func (r *reasoningDetails) add(raw []json.RawMessage) {
	for _, piece := range raw {
		var item map[string]any
		if json.Unmarshal(piece, &item) != nil || item == nil {
			continue
		}
		at := -1
		if index, ok := item["index"].(float64); ok {
			if r.byIndex == nil {
				r.byIndex = map[float64]int{}
			}
			if i, seen := r.byIndex[index]; seen {
				at = i
			} else {
				r.byIndex[index] = len(r.entries)
			}
		}
		if at < 0 {
			r.entries = append(r.entries, item)
			continue
		}
		entry := r.entries[at]
		for key, value := range item {
			text, isText := value.(string)
			if prev, ok := entry[key].(string); ok && isText && (key == "text" || key == "summary" || key == "data") {
				entry[key] = prev + text
			} else if value != nil {
				entry[key] = value
			}
		}
	}
}

// reasoningText is the readable thinking in one streamed delta: the
// reasoning text, or else the text and summary pieces of its
// reasoning_details (encrypted pieces carry nothing to show).
func reasoningText(text string, details []json.RawMessage) string {
	if text != "" {
		return text
	}
	var out strings.Builder
	for _, piece := range details {
		var item struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Summary string `json:"summary"`
		}
		if json.Unmarshal(piece, &item) != nil {
			continue
		}
		switch item.Type {
		case "reasoning.text":
			out.WriteString(item.Text)
		case "reasoning.summary":
			out.WriteString(item.Summary)
		}
	}
	return out.String()
}

func (r *reasoningDetails) raw() json.RawMessage {
	if len(r.entries) == 0 {
		return nil
	}
	out, err := json.Marshal(r.entries)
	if err != nil {
		return nil
	}
	return out
}

// logWebPlugin leaves one line per call that used the web plugin: which
// provider Polza routed it to and how many results came back -- the way
// to tell from the logs alone whether the search ran.
func logWebPlugin(ctx context.Context, apiModelID, servedBy string, result GenerateResult) {
	p, ok := WebPluginFromContext(ctx)
	if !ok {
		return
	}
	if servedBy == "" {
		servedBy = "?"
	}
	log.Printf("provider: polza web plugin model=%s provider=%s engine=%s results=%d searches=%d", apiModelID, servedBy, p.Engine, len(result.Citations), result.WebSearches)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
