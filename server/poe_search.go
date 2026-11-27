package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"neochat/conversation"
	"neochat/limits"
	"neochat/provider"
	"neochat/router"
)

const poeSearchBot = "Perplexity-Search"
const defaultPoeSearchCostUSD = 0.0061 // operator estimate from Poe's 200-point request rate, not token pricing

type poeSearchGroup struct {
	once    sync.Once
	queries []string
	text    string
	links   []provider.WebResult
	err     error
}

// All searches requested in the same model step are known before execution.
// Merge even separate singleton tool calls into native five-query requests.
func (r *webRun) preparePoeSearchGroups(ctx context.Context) {
	if r.s.PoeSearch == nil {
		return
	}
	if r.poeSearchGroups == nil {
		r.poeSearchGroups = map[string]*poeSearchGroup{}
	}
	var pending []string
	for _, q := range r.poeSearchOrder {
		if r.poeSearchGroups[q] == nil {
			pending = append(pending, q)
		}
	}
	for start := 0; start < len(pending); start += 5 {
		queries := append([]string(nil), pending[start:min(start+5, len(pending))]...)
		group := &poeSearchGroup{queries: queries}
		for _, q := range queries {
			r.poeSearchGroups[q] = group
		}
		go group.load(ctx, r)
	}
}

func (r *webRun) executePoeSearch(ctx context.Context, q string, index int) webOutcome {
	group := r.poeSearchGroups[q]
	if group == nil {
		group = &poeSearchGroup{queries: []string{q}}
	}
	group.load(ctx, r)
	r.finish(index, func(a *conversation.ToolActivity) {
		if group.err != nil {
			a.Error = toolErrorMessage(group.err)
			return
		}
		a.Results = webLinks(group.links)
		a.Purpose = fmt.Sprintf("Poe batch: %d queries", len(group.queries))
		if q == group.queries[0] {
			a.Output = group.text
		} else {
			a.Output = "Shared batch results are recorded under: " + group.queries[0]
		}
	})
	if group.err != nil {
		return webOutcome{Query: q, Result: "Error: " + toolErrorMessage(group.err)}
	}
	result := group.text
	if q != group.queries[0] {
		result = "This query was included in the same native Poe batch as " + group.queries[0] + ". Use the shared results returned for that query; it was not a separate provider request."
	}
	return webOutcome{Query: q, Result: result, short: "[Older shared search output compacted.]\n" + formatSearchResults(group.links)}
}

func (s *Server) callPoeSearch(ctx context.Context, req chatRequest, prepared preparedRequest, queries []string) (string, []provider.WebResult, error) {
	if len(queries) == 0 || len(queries) > 5 {
		return "", nil, fmt.Errorf("Perplexity-Search expects 1-5 queries")
	}
	params := map[string]any{"query": queries[0], "max_results": 10, "max_tokens": 5000, "max_tokens_per_page": 512}
	if len(queries) > 1 {
		params["multi_query"] = strings.Join(queries, "\n")
	}
	callCtx := provider.WithPoeBotParameters(ctx, params)
	if req.providerKey != "" {
		callCtx = provider.WithAPIKey(callCtx, req.providerKey)
	}
	// No system prompt, tool definitions, chat history or LLM extraction pass:
	// this bot is an existing search endpoint, not a generation model.
	messages := []provider.Message{{Role: "user", Content: queries[0]}}
	fee := s.PoeSearchCostUSD
	if fee <= 0 {
		fee = defaultPoeSearchCostUSD
	}
	reservation, err := s.Store.Reserve(ctx, req.UserID, limits.PoolInstant, fee, prepared.plan.InstantExtraCapUSD)
	if err != nil {
		return "", nil, err
	}
	res, callErr := s.PoeSearch.Generate(callCtx, poeSearchBot, messages)
	charged := fee
	var denied *provider.BotAccessDenied
	var status *provider.StatusError
	if errors.Is(callErr, provider.ErrMissingAPIKey) || errors.As(callErr, &denied) || (errors.As(callErr, &status) && status.StatusCode >= 400 && status.StatusCode < 500) {
		charged = 0
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Store.Settle(settleCtx, reservation, charged); err != nil {
		return "", nil, fmt.Errorf("settle search: %w", err)
	}
	if charged > 0 {
		entry := router.CostLogEntry{UserID: req.UserID, RequestID: prepared.requestID, ModelID: poeSearchBot, Mode: "web_search", CostUSD: charged, InputTokens: res.InputTokens, OutputTokens: res.OutputTokens, Timestamp: time.Now()}
		if err := s.CostLog.Record(settleCtx, entry); err != nil {
			log.Printf("server: record search accounting: %v", err)
		}
	}
	if callErr != nil {
		return "", nil, callErr
	}
	text := strings.TrimSpace(res.Text)
	if text == "" && len(res.Citations) == 0 {
		return "", nil, fmt.Errorf("Perplexity-Search returned no search output")
	}
	links := parsePoeSearchLinks(text, res.Citations)
	return text, links, nil
}

var searchMarkdownLink = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
var searchPlainURL = regexp.MustCompile(`https?://[^\s<>"\x60]+`)

// Preserve the original evidence even when the bot's output changes format.
// Parse JSON recursively (flat or per-query groups), then Markdown/plain URLs
// for the source cards. No extraction LLM and no silent SearXNG fallback.
func parsePoeSearchLinks(text string, citations []provider.WebResult) []provider.WebResult {
	results := append([]provider.WebResult(nil), citations...)
	var tree any
	raw := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(text), "```json"), "```"), "```"))
	if json.Unmarshal([]byte(raw), &tree) != nil {
		_ = json.Unmarshal([]byte(jsonObject.FindString(text)), &tree)
	}
	if tree != nil {
		var walk func(any)
		walk = func(v any) {
			switch v := v.(type) {
			case map[string]any:
				u, _ := v["url"].(string)
				if u == "" {
					u, _ = v["link"].(string)
				}
				if u != "" {
					title, _ := v["title"].(string)
					snippet, _ := v["snippet"].(string)
					if snippet == "" {
						snippet, _ = v["content"].(string)
					}
					results = append(results, provider.WebResult{URL: u, Title: title, Snippet: snippet})
				}
				for _, key := range []string{"results", "search_results", "data", "queries"} {
					if nested, ok := v[key]; ok {
						walk(nested)
					}
				}
			case []any:
				for _, item := range v {
					walk(item)
				}
			}
		}
		walk(tree)
	}
	for _, m := range searchMarkdownLink.FindAllStringSubmatch(text, -1) {
		results = append(results, provider.WebResult{Title: m[1], URL: m[2]})
	}
	for _, u := range searchPlainURL.FindAllString(text, -1) {
		u = strings.TrimRight(u, ".,;:)]}")
		results = append(results, provider.WebResult{URL: u})
	}
	seen := map[string]bool{}
	var out []provider.WebResult
	for _, r := range results {
		u, err := url.Parse(r.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		if r.Title == "" {
			r.Title = u.Host
		}
		r.Title = clipRunes(r.Title, 200)
		r.Snippet = clipRunes(strings.Join(strings.Fields(r.Snippet), " "), 300)
		out = append(out, r)
		if len(out) == 20 {
			break
		}
	}
	return out
}

func (g *poeSearchGroup) load(ctx context.Context, r *webRun) {
	g.once.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, webStepTimeout)
		defer cancel()
		defer func() {
			if rec := recover(); rec != nil {
				g.err = fmt.Errorf("Perplexity search failed")
			}
		}()
		g.err = r.s.withWebSlot(ctx, func() error {
			var err error
			g.text, g.links, err = r.s.callPoeSearch(ctx, r.req, r.prepared, g.queries)
			return err
		})
	})
}
