package conversation

import (
	"strings"
	"time"
	"unicode"
)

// SearchResult is a chat whose title or messages contain a search query,
// with a snippet of the newest matching message (empty when only the
// title matched).
type SearchResult struct {
	ID        string    `json:"conversation_id"`
	Title     string    `json:"title"`
	Pinned    bool      `json:"pinned,omitempty"`
	ProjectID string    `json:"project_id,omitempty"`
	Snippet   string    `json:"snippet,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// snippetRunes is about how much text a search result shows around the
// match.
const snippetRunes = 160

// snippet cuts text down to the part around the first match of query,
// on word boundaries where it can, with "…" where it cut.
func snippet(text, query string) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	lower := []rune(strings.ToLower(string(runes)))
	at := strings.Index(string(lower), strings.ToLower(query))
	start := 0
	if at > 0 {
		start = len([]rune(string(lower)[:at])) - snippetRunes/3
	}
	if start < 0 {
		start = 0
	}
	end := start + snippetRunes
	if end > len(runes) {
		end = len(runes)
	}
	for start > 0 && start < end && !unicode.IsSpace(runes[start-1]) {
		start++
	}
	out := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}
