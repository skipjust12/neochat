package provider

import (
	"log"
	"strconv"
	"strings"
)

// Prompt caching. A vendor that caches charges a fraction of the input
// price for the part of a prompt it has seen recently (Claude 0.1x,
// DeepSeek 0.1x, OpenAI and Gemini 0.25-0.5x), and a chat resends its
// whole history every turn, so in a long chat most of every request is
// that part. OpenAI, DeepSeek, Gemini and Grok cache on their own; Polza
// passes it through. Claude caches only up to the points a request marks
// with cache_control, and writing the cache costs 1.25x, read back within
// five minutes (each read starts the five minutes again).
//
// So a Claude request marks three points: the end of the leading system
// messages (persona, instructions, project, notes -- the same every turn,
// tool definitions included, since they come first) and the last two user
// messages. The last one writes this turn's prefix for the next turn; the
// one before reads what the previous turn wrote, however many blocks a
// tool loop or attachments put in between. Claude allows four.
//
// A prefix under the model's minimum (1024-4096 tokens) is never cached
// and costs nothing extra.

// cacheControl is the cache_control object on a content part.
type cacheControl struct {
	Type string `json:"type"`
}

var ephemeral = &cacheControl{Type: "ephemeral"}

// cachesExplicitly reports whether a model caches only where the request
// says (cache_control).
func cachesExplicitly(apiModelID string) bool {
	return strings.HasPrefix(apiModelID, "anthropic/")
}

// markCacheBreakpoints sets cache_control on the last leading system
// message and on the last two user messages.
func markCacheBreakpoints(messages []polzaChatMessage) {
	lastSystem := -1
	for i := range messages {
		if messages[i].Role != "system" {
			break
		}
		lastSystem = i
	}
	if lastSystem >= 0 {
		markCached(&messages[lastSystem])
	}
	users := 0
	for i := len(messages) - 1; i > lastSystem && users < 2; i-- {
		if messages[i].Role == "user" {
			markCached(&messages[i])
			users++
		}
	}
}

// markCached puts cache_control on a message's last text or image part,
// turning plain string content into one text part. Empty text can't carry
// it (the vendor refuses), and a document part is left alone: not every
// route takes the mark there. The mark then sits on the part before it,
// and the next turn's mark covers the document.
func markCached(m *polzaChatMessage) {
	switch content := m.Content.(type) {
	case string:
		if strings.TrimSpace(content) != "" {
			m.Content = []polzaContentPart{{Type: "text", Text: content, CacheControl: ephemeral}}
		}
	case []polzaContentPart:
		for i := len(content) - 1; i >= 0; i-- {
			part := &content[i]
			if (part.Type == "text" && strings.TrimSpace(part.Text) != "") || part.Type == "image_url" {
				part.CacheControl = ephemeral
				return
			}
		}
	}
}

// polzaPromptDetails is usage.prompt_tokens_details: how much of the
// prompt was read from the vendor's cache and how much written to it.
type polzaPromptDetails struct {
	CachedTokens     looseNumber `json:"cached_tokens"`
	CacheWriteTokens looseNumber `json:"cache_write_tokens"`
}

// looseNumber is a usage figure that may come as a number, a numeric
// string or null. Anything else reads as 0 rather than failing the whole
// response: these figures only refine the bill.
type looseNumber float64

func (n *looseNumber) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value < 0 {
		value = 0
	}
	*n = looseNumber(value)
	return nil
}

// applyTo copies a response's usage into result.
func (u polzaUsage) applyTo(result *GenerateResult) {
	result.InputTokens, result.OutputTokens = u.PromptTokens, u.CompletionTokens
	if d := u.PromptTokensDetails; d != nil {
		result.CachedTokens, result.CacheWriteTokens = int(d.CachedTokens), int(d.CacheWriteTokens)
	}
	result.CostRUB = float64(u.CostRUB)
	if result.CostRUB == 0 {
		result.CostRUB = float64(u.Cost)
	}
	if use := u.ServerToolUse; use != nil {
		result.WebSearches = use.WebSearchRequests
	}
}

// logCache notes a call that read or wrote the prompt cache.
func logCache(apiModelID string, result GenerateResult) {
	if result.CachedTokens == 0 && result.CacheWriteTokens == 0 {
		return
	}
	log.Printf("provider: prompt cache model=%s input_tokens=%d cache_read=%d cache_write=%d", apiModelID, result.InputTokens, result.CachedTokens, result.CacheWriteTokens)
}
