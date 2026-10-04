package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"neochat/conversation"
	"neochat/limits"
	"neochat/pagestore"
	"neochat/provider"
	"neochat/router"
	"neochat/webfetch"
)

// Web access, from the composer's Web search toggle.
//
// Models that can call functions get two tools and decide themselves when
// to use them, and the server runs the loop: the model asks for a tool,
// the server runs it (showing the step live), sends back the result, and
// the model goes on -- up to webMaxRounds model calls per answer.
//   - web_search runs Polza's web plugin (Yandex search) through a small
//     helper model call and returns up to webSearchResults results; see
//     searchWeb. (Polza's own server tools, polza:*, are not open to every
//     account; the plugin is.)
//   - web_fetch downloads the page on this server (package webfetch) and
//     keeps its text per chat (package pagestore), so the model can read a
//     long page in parts or come back to it in a later message.
//
// Modes:
//   - "auto": tool-calling models get the tools; others get a note saying
//     they can't search, so they point the user to "on".
//   - "on": tool-calling models are also told to search first; others get
//     one web plugin search for the user's message before they answer.
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

// Limits for one answer.
const (
	webMaxRounds     = 8  // model calls, tool steps in between
	webMaxSearches   = 5  // web_search calls
	webMaxFetches    = 6  // web_fetch calls
	webSearchResults = 10 // results per search
	webFetchWindow   = 20_000
	webToolWorkers   = 4 // tool calls of one step run in parallel
	webStepTimeout   = 45 * time.Second
)

// defaultWebSearchModelID runs web searches: the cheapest current catalog
// model, with reasoning off. It only copies search results.
const defaultWebSearchModelID = "gpt-6-luna"

// webPluginFeeUSD is what Polza charges per web plugin search on top of
// tokens: 0 RUB at the time of writing ("уточняется" in /v2/plugins).
const webPluginFeeUSD = 0.0

var webFunctionTools = []provider.FunctionTool{
	{
		Name:        "web_search",
		Description: "Search the web. Returns up to 10 results, each with title, URL and a snippet. Write the query the way you would type it into a search engine; search again with different words if the results miss.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "The search query."},
			},
			"required": []string{"query"},
		},
	},
	{
		Name:        "web_fetch",
		Description: "Read a web page or text document by its URL (from search results or the user) and get its readable text. Pages are read as served, without running JavaScript. Long pages come in parts: pass offset to continue where the previous part ended.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":    map[string]any{"type": "string", "description": "Full http(s) URL of the page."},
				"offset": map[string]any{"type": "integer", "description": "Character offset to read from; 0 or omitted for the start."},
			},
			"required": []string{"url"},
		},
	},
}

const (
	// webToolsNote goes to every tool-calling model with web access.
	webToolsNote = "You can search the web (web_search) and read web pages (web_fetch). Use them for anything that may have changed since your training -- news, prices, releases, current versions, schedules -- and when the user asks you to look something up or gives you a link; otherwise answer directly. Cite the pages you used with Markdown links. Today is %s."
	// webSearchInstruction is added in "on" mode.
	webSearchInstruction = "Web search is turned on for this message: search the web before answering, read the most relevant pages when the snippets aren't enough, and cite the pages you used."
	// webPluginNote: "on" mode for a model without function calling.
	webPluginNote = "Web search results for the user's message were added to your context. Use them where they help, and cite the pages you used with Markdown links."
	// noWebToolsNote: "auto" mode for a model without function calling.
	noWebToolsNote = "Web search is on Auto, but this model can't call web tools, so you have no internet access for this answer. If the user asks you to search or check something online, say in one sentence that this model can't search by itself and that switching Web search to On (in the composer's + menu) makes it search before answering; then help with what you know."
	// webUnavailableNote: the vendor refused web access for this call.
	webUnavailableNote = "Web search was requested but isn't available for this model right now, so you have no internet access for this answer. If the user asked you to look something up, say in one sentence that search didn't work with this model this time, then help with what you know."
)

// webAnswer is the outcome of answering with (or without) web access.
type webAnswer struct {
	provider.GenerateResult // text, usage and citations summed over all model calls
	Activity                []conversation.ToolActivity
	// Continued: a model call after the first one ran, so text may already
	// be on screen and the answer must not be retried on another model.
	Continued bool
	// Questionnaire: the model ended the answer by asking the user
	// questions (ask_user).
	Questionnaire *conversation.Questionnaire
}

// webRun answers one message, running web tools as the model asks.
type webRun struct {
	s          *Server
	req        chatRequest
	prepared   preparedRequest
	model      router.Model
	generate   func(ctx context.Context, messages []provider.Message) (provider.GenerateResult, error)
	onActivity func([]conversation.ToolActivity)

	mu       sync.Mutex
	activity []conversation.ToolActivity
	searches int
	fetches  int
}

// answer is routeAndCall's call for one model. Function-calling models go
// through the tool loop with ask_user, plus the web tools when the Web
// search toggle allows; the rest answer plainly, or after a search in
// "on" mode.
func (s *Server) answer(ctx context.Context, req chatRequest, prepared preparedRequest, model router.Model, messages []provider.Message, generate func(context.Context, []provider.Message) (provider.GenerateResult, error), onActivity func([]conversation.ToolActivity)) (webAnswer, error) {
	r := &webRun{s: s, req: req, prepared: prepared, model: model, generate: generate, onActivity: onActivity}
	web := req.WebSearch == webSearchAuto || req.WebSearch == webSearchOn
	switch {
	case model.ToolCalling:
		return r.loop(ctx, messages, web)
	case req.WebSearch == webSearchAuto:
		res, err := generate(ctx, withSystemNote(messages, noWebToolsNote))
		return webAnswer{GenerateResult: res}, err
	case req.WebSearch == webSearchOn:
		return r.searchFirst(ctx, messages)
	}
	res, err := generate(ctx, messages)
	return webAnswer{GenerateResult: res}, err
}

// loop is the tool loop for a function-calling model: ask_user always, the
// web tools when web is on.
func (r *webRun) loop(ctx context.Context, base []provider.Message, web bool) (webAnswer, error) {
	tools := []provider.FunctionTool{askUserTool}
	messages := base
	if web {
		tools = append(append([]provider.FunctionTool(nil), webFunctionTools...), askUserTool)
		messages = withSystemNote(messages, fmt.Sprintf(webToolsNote, time.Now().UTC().Format("Monday, 2 January 2006")))
		if r.req.WebSearch == webSearchOn {
			messages = withSystemNote(messages, webSearchInstruction)
		}
	}
	var out webAnswer
	reasoningOff := false
	for round := 0; round < webMaxRounds; round++ {
		last := round == webMaxRounds-1
		callCtx := ctx
		if !last {
			callCtx = provider.WithFunctionTools(callCtx, tools)
		}
		if out.Text != "" {
			callCtx = provider.WithParagraphBreak(callCtx)
		}
		if reasoningOff {
			callCtx = provider.WithReasoning(callCtx, provider.Reasoning{Disabled: true, Adaptive: r.model.Reasoning == "adaptive"})
		}
		res, err := r.generate(callCtx, messages)
		if err != nil && rejectedRequest(err) {
			if round == 0 {
				return r.withoutTools(ctx, base, err, web)
			}
			if !reasoningOff {
				// Some routes can't take a model's reasoning back with its
				// tool calls; carry on with reasoning off instead.
				log.Printf("server: vendor refused a tool-loop continuation for model_id=%s, retrying with reasoning off: %v", r.model.ID, err)
				reasoningOff = true
				messages = withoutReasoning(messages)
				round--
				continue
			}
			if !last {
				// Not even that: hand the model what the tools found as plain
				// text and let it answer without tools.
				log.Printf("server: vendor refused tool results for model_id=%s, answering from them as text: %v", r.model.ID, err)
				messages = flattenToolTurns(messages)
				round = webMaxRounds - 2
				continue
			}
		}
		out.Continued = round > 0
		if err != nil {
			out.Activity = r.snapshot()
			return out, err
		}
		out.Text += res.Text
		out.InputTokens += res.InputTokens
		out.OutputTokens += res.OutputTokens
		out.WebSearches += res.WebSearches
		out.Citations = append(out.Citations, res.Citations...)
		if len(res.ToolCalls) == 0 {
			break
		}
		calls := res.ToolCalls
		for i := range calls {
			if calls[i].ID == "" {
				calls[i].ID = fmt.Sprintf("call_%d_%d", round, i)
			}
		}
		// A questionnaire ends the turn: the user answers it, in their own
		// time, as their next message. Other calls of the same step are
		// dropped; the model can make them again after the answers.
		var askErrors map[int]string
		for i, call := range calls {
			if call.Name != askUserTool.Name {
				continue
			}
			questionnaire, err := parseQuestionnaire(call.Arguments)
			if err != nil {
				if askErrors == nil {
					askErrors = map[int]string{}
				}
				askErrors[i] = "Error: " + err.Error() + ". Fix it and call ask_user again."
				continue
			}
			out.Questionnaire = questionnaire
			out.Activity = r.snapshot()
			return out, nil
		}
		assistant := provider.Message{Role: "assistant", Content: res.Text, ToolCalls: calls}
		if !reasoningOff {
			assistant.Reasoning, assistant.ReasoningDetails = res.Reasoning, res.ReasoningDetails
		}
		messages = append(messages, assistant)
		for i, result := range r.runTools(ctx, calls, askErrors) {
			messages = append(messages, provider.Message{Role: "tool", ToolCallID: calls[i].ID, Content: result})
		}
		if err := ctx.Err(); err != nil {
			out.Activity = r.snapshot()
			return out, err
		}
	}
	out.Activity = r.snapshot()
	return out, nil
}

// searchFirst is "on" mode for a model without function calling: Polza's
// web plugin searches for the user's message before the model answers.
func (r *webRun) searchFirst(ctx context.Context, messages []provider.Message) (webAnswer, error) {
	step := r.start(conversation.ToolActivity{Tool: "web_search", Query: clipRunes(strings.TrimSpace(r.req.Message), 200)})
	searchPrompt := clipRunes(r.req.Message, 400)
	if r.req.Quote != "" {
		searchPrompt += "\n(about: " + clipRunes(r.req.Quote, 200) + ")"
	}
	callCtx := provider.WithWebPlugin(ctx, provider.WebPlugin{Engine: "yandex", MaxResults: webSearchResults, SearchPrompt: searchPrompt})
	// The search runs before the model, so it is over by the first text.
	callCtx = withFirstDelta(callCtx, func() { r.finish(step, func(*conversation.ToolActivity) {}) })
	res, err := r.generate(callCtx, withSystemNote(messages, webPluginNote))
	if err != nil && rejectedRequest(err) {
		r.finish(step, func(a *conversation.ToolActivity) { a.Error = webRefusal(err) })
		res, err = r.generate(ctx, withSystemNote(messages, webUnavailableNote))
		return webAnswer{GenerateResult: res, Activity: r.snapshot()}, err
	}
	// The results are shown with the search step, not again as sources.
	r.finish(step, func(a *conversation.ToolActivity) { a.Results = webLinks(res.Citations) })
	res.Citations = nil
	return webAnswer{GenerateResult: res, Activity: r.snapshot()}, err
}

// withoutTools answers after the vendor refused the request with tools:
// no tools; with web on, also a note so the model says why and the
// refusal shown as a failed step. messages are the request's own, without
// the web notes.
func (r *webRun) withoutTools(ctx context.Context, messages []provider.Message, refused error, web bool) (webAnswer, error) {
	log.Printf("server: vendor rejected tools for model_id=%s, answering without them: %v", r.model.ID, refused)
	if !web {
		res, err := r.generate(ctx, messages)
		return webAnswer{GenerateResult: res}, err
	}
	step := r.start(conversation.ToolActivity{Tool: "web_search"})
	r.finish(step, func(a *conversation.ToolActivity) { a.Error = webRefusal(refused) })
	res, err := r.generate(ctx, withSystemNote(withoutWebInstruction(messages), webUnavailableNote))
	return webAnswer{GenerateResult: res, Activity: r.snapshot()}, err
}

// runTools runs one step's tool calls and returns their results in call
// order. The calls are checked and their steps shown in order first, then
// run a few at a time.
func (r *webRun) runTools(ctx context.Context, calls []provider.ToolCall, preset map[int]string) []string {
	results := make([]string, len(calls))
	jobs := make([]func(context.Context) string, len(calls))
	for i, call := range calls {
		if result, ok := preset[i]; ok {
			results[i] = result
			continue
		}
		results[i], jobs[i] = r.planTool(call)
	}
	slots := make(chan struct{}, webToolWorkers)
	var wg sync.WaitGroup
	for i, job := range jobs {
		if job == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverToolPanic(&results[i])
			slots <- struct{}{}
			defer func() { <-slots }()
			stepCtx, cancel := context.WithTimeout(ctx, webStepTimeout)
			defer cancel()
			results[i] = job(stepCtx)
		}()
	}
	wg.Wait()
	return results
}

func recoverToolPanic(result *string) {
	if rec := recover(); rec != nil {
		log.Printf("server: panic in web tool: %v", rec)
		*result = "Error: the tool failed."
	}
}

// planTool checks one call: it returns either an immediate result (bad
// arguments, unknown tool, limit reached) or the job that runs it, with
// its step already shown as running.
func (r *webRun) planTool(call provider.ToolCall) (string, func(context.Context) string) {
	var args struct {
		Query  string `json:"query"`
		URL    string `json:"url"`
		Offset int    `json:"offset"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil && strings.TrimSpace(call.Arguments) != "" {
		return "Error: the arguments are not valid JSON.", nil
	}
	switch call.Name {
	case "web_search":
		query := strings.TrimSpace(args.Query)
		if query == "" {
			return "Error: query is empty.", nil
		}
		if !r.take(&r.searches, webMaxSearches) {
			return fmt.Sprintf("Search limit for this answer reached (%d searches). Answer with what you have.", webMaxSearches), nil
		}
		step := r.start(conversation.ToolActivity{Tool: "web_search", Query: clipRunes(query, 300)})
		return "", func(ctx context.Context) string {
			results, err := r.s.searchWeb(ctx, r.req, r.prepared, query)
			r.finish(step, func(a *conversation.ToolActivity) {
				if err != nil {
					a.Error = toolErrorMessage(err)
				} else {
					a.Results = webLinks(results)
				}
			})
			if err != nil {
				return "Error: " + toolErrorMessage(err)
			}
			return formatSearchResults(results)
		}
	case "web_fetch":
		rawURL := strings.TrimSpace(args.URL)
		if rawURL == "" {
			return "Error: url is empty.", nil
		}
		if !r.take(&r.fetches, webMaxFetches) {
			return fmt.Sprintf("Page limit for this answer reached (%d pages). Answer with what you have.", webMaxFetches), nil
		}
		step := r.start(conversation.ToolActivity{Tool: "web_fetch", URL: clipRunes(rawURL, 2000)})
		return "", func(ctx context.Context) string {
			page, err := r.s.loadPage(ctx, r.req, r.prepared, rawURL)
			window, from, to := pageWindow(page.Text, args.Offset)
			r.finish(step, func(a *conversation.ToolActivity) {
				if err != nil {
					a.Error = toolErrorMessage(err)
					return
				}
				a.URL, a.Title, a.Chars = page.FinalURL, page.Title, len([]rune(page.Text))
				a.Excerpt = plainExcerpt(window, 280)
			})
			if err != nil {
				return "Error: " + toolErrorMessage(err)
			}
			return formatPage(page, window, from, to)
		}
	}
	return fmt.Sprintf("Error: there is no tool named %q.", call.Name), nil
}

// take counts one use of a limited tool; false once the limit is used up.
func (r *webRun) take(counter *int, limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if *counter >= limit {
		return false
	}
	*counter++
	return true
}

func (r *webRun) start(step conversation.ToolActivity) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	step.Pending = true
	r.activity = append(r.activity, step)
	r.emitLocked()
	return len(r.activity) - 1
}

func (r *webRun) finish(index int, update func(*conversation.ToolActivity)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	step := &r.activity[index]
	update(step)
	step.Pending = false
	r.emitLocked()
}

// emitLocked sends the whole list each time (a dozen small entries): the
// client just re-renders it. Called with r.mu held, which also keeps the
// SSE writes of parallel tools from interleaving.
func (r *webRun) emitLocked() {
	if r.onActivity != nil {
		r.onActivity(append([]conversation.ToolActivity(nil), r.activity...))
	}
}

func (r *webRun) snapshot() []conversation.ToolActivity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return finishedSteps(r.activity)
}

// finishedSteps is a copy of activity fit to store: a step still running
// when the answer ended (the user pressed Stop) is shown as stopped.
func finishedSteps(activity []conversation.ToolActivity) []conversation.ToolActivity {
	if len(activity) == 0 {
		return nil
	}
	out := append([]conversation.ToolActivity(nil), activity...)
	for i := range out {
		if out[i].Pending && out[i].Error == "" && len(out[i].Results) == 0 && out[i].Title == "" {
			out[i].Error = "stopped"
		}
		out[i].Pending = false
	}
	return out
}

// --- web_search ---

const searchExtractPrompt = `The web search results for the user's query were added to your context. Copy them out as JSON, in the order given, and nothing else:
{"results":[{"title":"...","url":"...","snippet":"..."}]}
Copy every URL exactly. The snippet is the result's own text, at most 300 characters. Never add a result that isn't in your context; if there are none, output {"results":[]}.`

// searchWeb runs one search: a call to the web search model with Polza's
// web plugin, which puts the results into that call's context. They come
// back as url_citation annotations; the model also copies them out as
// JSON, used when a route returns no annotations.
func (s *Server) searchWeb(ctx context.Context, req chatRequest, prepared preparedRequest, query string) ([]provider.WebResult, error) {
	modelID := s.WebSearchModelID
	if modelID == "" {
		modelID = defaultWebSearchModelID
	}
	model, ok := s.Router.Catalog.FindModel(modelID)
	if !ok {
		return nil, fmt.Errorf("web search model %q is not in the catalog", modelID)
	}
	gen, ok := s.Generators[model.Provider]
	if !ok {
		return nil, fmt.Errorf("no generator for web search model %q", modelID)
	}
	callCtx := provider.WithWebPlugin(ctx, provider.WebPlugin{Engine: "yandex", MaxResults: webSearchResults, SearchPrompt: query})
	withReasoningOff := callCtx
	if model.Reasoning != "" {
		withReasoningOff = provider.WithReasoning(callCtx, provider.Reasoning{Disabled: true, Adaptive: model.Reasoning == "adaptive"})
	}
	messages := []provider.Message{{Role: "system", Content: searchExtractPrompt}, {Role: "user", Content: query}}
	call := func(ctx context.Context) (provider.GenerateResult, error) {
		return s.billedCall(ctx, req, prepared.plan, limits.PoolInstant, model.CostInputPerMTok, model.CostOutputPerMTok, 3000, gen, model.ResolveAPIModelID(), messages, directGenerate)
	}
	res, err := call(withReasoningOff)
	if err != nil && rejectedRequest(err) && model.Reasoning != "" {
		res, err = call(callCtx)
	}
	if err != nil {
		return nil, err
	}
	s.recordAuxCostLog(ctx, req.UserID, prepared.requestID, "web_search", model.ResolveAPIModelID(), model.CostInputPerMTok, model.CostOutputPerMTok, &res)
	return mergeSearchResults(res.Citations, parseSearchJSON(res.Text)), nil
}

var jsonObject = regexp.MustCompile(`(?s)\{.*\}`)

// parseSearchJSON reads the search model's {"results":[...]} reply,
// tolerating code fences and text around it.
func parseSearchJSON(text string) []provider.WebResult {
	raw := jsonObject.FindString(text)
	if raw == "" {
		return nil
	}
	var parsed struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return nil
	}
	out := make([]provider.WebResult, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		out = append(out, provider.WebResult{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	return out
}

// mergeSearchResults prefers the annotations (URLs straight from the
// search) and fills in missing titles and snippets from the model's copy;
// with no annotations the copy stands alone. Only http(s) URLs, no
// duplicates, at most webSearchResults.
func mergeSearchResults(citations, copied []provider.WebResult) []provider.WebResult {
	byURL := map[string]provider.WebResult{}
	for _, c := range copied {
		byURL[c.URL] = c
	}
	base := citations
	if len(base) == 0 {
		base = copied
	}
	seen := map[string]bool{}
	var out []provider.WebResult
	for _, r := range base {
		r.URL = strings.TrimSpace(r.URL)
		if !strings.HasPrefix(r.URL, "https://") && !strings.HasPrefix(r.URL, "http://") || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		if c, ok := byURL[r.URL]; ok {
			if r.Title == "" {
				r.Title = c.Title
			}
			if r.Snippet == "" {
				r.Snippet = c.Snippet
			}
		}
		r.Title = clipRunes(strings.Join(strings.Fields(r.Title), " "), 200)
		r.Snippet = clipRunes(strings.Join(strings.Fields(r.Snippet), " "), 300)
		out = append(out, r)
		if len(out) == webSearchResults {
			break
		}
	}
	return out
}

func formatSearchResults(results []provider.WebResult) string {
	if len(results) == 0 {
		return "No results. Try a different query."
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n%s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			b.WriteString(r.Snippet + "\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// --- web_fetch ---

// loadPage returns a page's text: this chat's stored copy if it is fresh,
// otherwise a new fetch, stored for later (never for incognito chats).
func (s *Server) loadPage(ctx context.Context, req chatRequest, prepared preparedRequest, rawURL string) (pagestore.Page, error) {
	fetcher := s.Fetcher
	if fetcher == nil {
		fetcher = defaultFetcher
	}
	url, err := fetcher.Normalize(rawURL)
	if err != nil {
		return pagestore.Page{}, err
	}
	keep := s.Pages != nil && !req.Incognito && prepared.conversationID != ""
	if keep {
		page, ok, err := s.Pages.Get(ctx, req.UserID, prepared.conversationID, url, time.Now().Add(-pagestore.TTL))
		if err != nil {
			log.Printf("server: read stored page for conversation_id=%s: %v", prepared.conversationID, err)
		} else if ok {
			return page, nil
		}
	}
	fetched, err := fetcher.Fetch(ctx, url)
	if err != nil {
		return pagestore.Page{}, err
	}
	page := pagestore.Page{
		UserID: req.UserID, ConversationID: prepared.conversationID,
		URL: url, FinalURL: fetched.FinalURL, Title: fetched.Title, Text: fetched.Text,
		Truncated: fetched.Truncated, FetchedAt: fetched.FetchedAt,
	}
	if keep {
		if err := s.Pages.Put(context.WithoutCancel(ctx), page); err != nil {
			log.Printf("server: store page for conversation_id=%s: %v", prepared.conversationID, err)
		}
	}
	return page, nil
}

var defaultFetcher = &webfetch.Fetcher{}

// pageWindow is the part of a page one web_fetch returns, in characters.
func pageWindow(text string, offset int) (window string, from, to int) {
	runes := []rune(text)
	from = max(0, min(offset, len(runes)))
	to = min(len(runes), from+webFetchWindow)
	return string(runes[from:to]), from, to
}

func formatPage(page pagestore.Page, window string, from, to int) string {
	total := len([]rune(page.Text))
	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\n", page.FinalURL)
	if page.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", page.Title)
	}
	switch {
	case from >= total && total > 0:
		fmt.Fprintf(&b, "[Nothing at offset %d: the page has %d characters.]\n", from, total)
	case to < total:
		fmt.Fprintf(&b, "[Characters %d-%d of %d. Call web_fetch with offset=%d to read on.]\n", from, to, total, to)
	case from > 0:
		fmt.Fprintf(&b, "[Characters %d-%d of %d: the end of the page.]\n", from, to, total)
	}
	if page.Truncated && to >= total {
		b.WriteString("[The page was longer; the rest of it could not be read.]\n")
	}
	b.WriteString("\n")
	b.WriteString(window)
	return b.String()
}

// --- helpers ---

// toolErrorMessage is what the model and the user see for a failed step:
// fetch errors and vendor refusals as such, anything else generically.
func toolErrorMessage(err error) string {
	var fetchErr *webfetch.Error
	var statusErr *provider.StatusError
	switch {
	case errors.As(err, &fetchErr):
		return fetchErr.Message
	case errors.As(err, &statusErr):
		if msg := provider.VendorMessage(statusErr.Body); msg != "" {
			return fmt.Sprintf("the search service refused (HTTP %d): %s", statusErr.StatusCode, msg)
		}
		return fmt.Sprintf("the search service refused (HTTP %d)", statusErr.StatusCode)
	case errors.Is(err, context.DeadlineExceeded):
		return "it took too long"
	case errors.Is(err, context.Canceled):
		return "stopped"
	case errors.Is(err, limits.ErrBudgetExceeded):
		return "the spending limit was reached"
	}
	log.Printf("server: web tool step failed: %v", err)
	return "it failed"
}

func webLinks(results []provider.WebResult) []conversation.WebLink {
	if len(results) == 0 {
		return nil
	}
	out := make([]conversation.WebLink, 0, len(results))
	for _, r := range results {
		out = append(out, conversation.WebLink{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	return out
}

// rejectedRequest reports a vendor refusal of the request itself, as
// opposed to auth, balance or availability problems: bad parameters
// (400/422), or no provider for the model that supports them (404).
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

// webRefusal describes a vendor's refusal of the web settings for the
// user: the status and Polza's own message.
func webRefusal(err error) string {
	var statusErr *provider.StatusError
	if !errors.As(err, &statusErr) {
		return "Web search is unavailable for this model right now."
	}
	reason := provider.VendorMessage(statusErr.Body)
	if reason == "" {
		return fmt.Sprintf("Polza refused web search for this model (HTTP %d).", statusErr.StatusCode)
	}
	return fmt.Sprintf("Polza refused web search for this model (HTTP %d): %s", statusErr.StatusCode, reason)
}

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

// flattenToolTurns rewrites the tool calls and their results as plain
// messages, for a route that can't continue a function-calling exchange:
// the model's own words stay an assistant turn, the results become one
// system note after it.
func flattenToolTurns(messages []provider.Message) []provider.Message {
	var out []provider.Message
	var results strings.Builder
	for _, m := range messages {
		switch {
		case len(m.ToolCalls) > 0:
			if strings.TrimSpace(m.Content) != "" {
				out = append(out, provider.Message{Role: "assistant", Content: m.Content})
			}
		case m.Role == "tool":
			results.WriteString(m.Content)
			results.WriteString("\n\n---\n\n")
		default:
			out = append(out, m)
		}
	}
	if results.Len() > 0 {
		out = append(out, provider.Message{Role: "system", Content: "Results of the web searches and pages you asked for (tools are no longer available; answer from these):\n\n" + strings.TrimSuffix(results.String(), "\n\n---\n\n")})
	}
	return out
}

// withoutReasoning strips the reasoning sent back with tool calls.
func withoutReasoning(messages []provider.Message) []provider.Message {
	out := append([]provider.Message(nil), messages...)
	for i := range out {
		out[i].Reasoning, out[i].ReasoningDetails = "", nil
	}
	return out
}

var (
	markdownLink   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	markdownMarker = regexp.MustCompile("(?m)^\\s*(#{1,6}\\s+|[-*]\\s+|```.*$)")
)

// plainExcerpt is the start of a page's text for display, without the
// Markdown marks the extractor writes (headings, bullets, links, fences).
func plainExcerpt(text string, n int) string {
	text = markdownLink.ReplaceAllString(text, "$1")
	text = markdownMarker.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "`", "")
	return clipRunes(strings.Join(strings.Fields(text), " "), n)
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

type firstDeltaKey struct{}

// withFirstDelta asks streamingGenerate to call fn once, when the call's
// first text arrives.
func withFirstDelta(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, firstDeltaKey{}, fn)
}

func firstDeltaHook(ctx context.Context) func() {
	fn, _ := ctx.Value(firstDeltaKey{}).(func())
	return fn
}
