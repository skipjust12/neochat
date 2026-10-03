package server

import (
	"errors"
	"net/http"

	"neochat/conversation"
	"neochat/provider"
	"neochat/router"
)

// Web search modes, from the composer's Web search toggle.
//   - "auto": models that can call tools get web search and page fetch and
//     decide themselves when to use them; others get nothing extra.
//   - "on": the user wants the web consulted: tool-calling models are told
//     to search first, the rest get a search run before they answer.
//   - "off" (and empty, for API callers that never set it): no web access.
const (
	webSearchAuto = "auto"
	webSearchOn   = "on"
	webSearchOff  = "off"
)

func validWebSearchMode(mode string) bool {
	switch mode {
	case "", webSearchAuto, webSearchOn, webSearchOff:
		return true
	}
	return false
}

// webSearchInstruction is added as a system message in "on" mode.
const webSearchInstruction = "Web search is turned on for this message: search the web before answering, open the most relevant pages when the snippets aren't enough, and cite the pages you used."

// webSearchFeeUSD is Polza's per-call price for polza:web_search (0.5 RUB)
// at the catalog's RUB/USD rate (see configs/models.json); page fetches
// are free.
const webSearchFeeUSD = 0.5 / 117.068

func webToolsFor(model router.Model, mode string) provider.WebTools {
	switch mode {
	case webSearchAuto:
		return provider.WebTools{Tools: model.ToolCalling}
	case webSearchOn:
		if model.ToolCalling {
			return provider.WebTools{Tools: true}
		}
		return provider.WebTools{SearchFirst: true}
	}
	return provider.WebTools{}
}

// buildActivity folds the stream's tool events into one entry per step: a
// call opens an entry, its result fills it in (matched by call id, else to
// the latest open entry of the same tool). Datetime lookups are left out --
// they're bookkeeping, not something the user needs to see.
func buildActivity(events []provider.ToolEvent) []conversation.ToolActivity {
	var out []conversation.ToolActivity
	ids := map[string]int{}
	for _, e := range events {
		if e.Tool == "datetime" {
			continue
		}
		index := -1
		if e.ID != "" {
			if i, ok := ids[e.ID]; ok {
				index = i
			}
		}
		if index < 0 && e.Phase == "result" {
			for i := len(out) - 1; i >= 0; i-- {
				if (e.Tool == "" || out[i].Tool == e.Tool) && len(out[i].Results) == 0 && out[i].Error == "" {
					index = i
					break
				}
			}
		}
		if index < 0 {
			if e.Tool == "" {
				continue // a result we can't place
			}
			out = append(out, conversation.ToolActivity{Tool: e.Tool})
			index = len(out) - 1
			if e.ID != "" {
				ids[e.ID] = index
			}
		}
		entry := &out[index]
		if entry.Query == "" {
			entry.Query = e.Query
		}
		if entry.URL == "" {
			entry.URL = e.URL
		}
		if entry.Title == "" {
			entry.Title = e.Title
		}
		if len(e.Results) > 0 {
			entry.Results = webLinks(e.Results)
		}
		if e.Error != "" {
			entry.Error = e.Error
		}
	}
	return out
}

func webLinks(results []provider.WebResult) []conversation.WebLink {
	if len(results) == 0 {
		return nil
	}
	out := make([]conversation.WebLink, len(results))
	for i, r := range results {
		out[i] = conversation.WebLink{Title: r.Title, URL: r.URL, Snippet: r.Snippet}
	}
	return out
}

// rejectedRequest reports a vendor refusal of the request itself, as
// opposed to auth, balance or availability problems: bad parameters
// (400/422), or no provider for the model that supports them (404, which
// is how require_parameters routing fails).
func rejectedRequest(err error) bool {
	var statusErr *provider.StatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	switch statusErr.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// Notes for a model that ends up without web access, so that a user who
// asks it to look something up gets told why it can't and what to do,
// not just "I have no search tool here".
const (
	// Auto mode, model without tool calling (Polza can't give it tools).
	noWebToolsNote = "Web search is on Auto, but this model can't call web tools, so you have no internet access for this answer. If the user asks you to search or check something online, say in one sentence that this model can't search by itself and that switching Web search to On (in the composer's + menu) makes it search before answering; then help with what you know."
	// Web tools were requested but the vendor refused them for this call.
	webUnavailableNote = "Web search was requested but isn't available for this model right now, so you have no internet access for this answer. If the user asked you to look something up, say in one sentence that search didn't work with this model this time, then help with what you know."
)

// withSystemNote adds a system message after the leading system messages
// (persona, instructions, project), ahead of the conversation itself.
func withSystemNote(messages []provider.Message, note string) []provider.Message {
	at := 0
	for at < len(messages) && messages[at].Role == "system" {
		at++
	}
	out := make([]provider.Message, 0, len(messages)+1)
	out = append(out, messages[:at]...)
	out = append(out, provider.Message{Role: "system", Content: note})
	return append(out, messages[at:]...)
}

// withoutWebInstruction drops the "search the web" system message, for a
// call that ends up without web access: told to search with no tools, a
// model tends to pretend it did.
func withoutWebInstruction(messages []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" && m.Content == webSearchInstruction {
			continue
		}
		out = append(out, m)
	}
	return out
}
