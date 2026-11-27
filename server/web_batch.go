package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"neochat/conversation"
	"neochat/pagestore"
	"neochat/provider"
)

const searchModeNote = "Search mode is active. Research the request using up to 30 searches and 20 page reads; these are ceilings, not targets. Batch independent queries/pages in groups of up to 5 searches or up to 4 pages. Read useful sources, compare evidence, and cite exact URLs. Recent web outputs stay verbatim. Only when older evidence accumulates beyond the context budget is it replaced by a source-grounded summary; failed summarization keeps the originals. Use notes for optional working hypotheses, not as the sole record of evidence. Re-read cached pages with offset when exact details are needed; do not invent missing details."

type webGate struct {
	slots, slow chan struct{}
	mu          sync.Mutex
	until, next time.Time
}

// Shared across answers, including batches: never multiply outbound concurrency
// by the number of tool calls or open chats.
func (s *Server) withWebSlot(ctx context.Context, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.webGateOnce.Do(func() { s.webGate = &webGate{slots: make(chan struct{}, webToolWorkers), slow: make(chan struct{}, 1)} })
	g := s.webGate
	select {
	case g.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.slots }()
	g.mu.Lock()
	slow := time.Now().Before(g.until)
	g.mu.Unlock()
	if slow {
		select {
		case g.slow <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		defer func() { <-g.slow }()
		g.mu.Lock()
		delay := time.Until(g.next)
		g.mu.Unlock()
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := run()
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil && (strings.Contains(err.Error(), "429") || strings.Contains(strings.ToLower(err.Error()), "too many requests") || strings.Contains(err.Error(), "no search engine answered")) {
		g.until = time.Now().Add(15 * time.Second)
	}
	if slow || time.Now().Before(g.until) {
		g.next = time.Now().Add(500 * time.Millisecond)
	}
	return err
}

func (s *Server) searchWeb(ctx context.Context, req chatRequest, prepared preparedRequest, query string) (results []provider.WebResult, err error) {
	err = s.withWebSlot(ctx, func() error { results, err = s.searchWebNow(ctx, req, prepared, query); return err })
	return
}

type webOutcome struct {
	Query  string `json:"query,omitempty"`
	URL    string `json:"url,omitempty"`
	Result string `json:"result"`
	short  string
}
type webPromise struct {
	once   sync.Once
	run    func(context.Context) webOutcome
	result webOutcome
}

func (p *webPromise) get(ctx context.Context) webOutcome {
	p.once.Do(func() {
		p.result.Result = "Error: the web tool failed."
		defer recoverToolPanic(&p.result.Result)
		p.result = p.run(ctx)
	})
	return p.result
}

type webRecord struct {
	round   int
	compact string // source index retained alongside a successful summary
	summary string
}
type webPageRequest struct {
	URL    string `json:"url"`
	Offset int    `json:"offset,omitempty"`
}

func (r *webRun) webLimits() (int, int) {
	if r.req.SearchMode {
		return 30, 20
	}
	return webMaxSearches, webMaxFetches
}

func (r *webRun) planWebTool(call provider.ToolCall) (string, func(context.Context) string) {
	var a struct {
		Query   string           `json:"query"`
		Queries []string         `json:"queries"`
		URL     string           `json:"url"`
		Offset  int              `json:"offset"`
		Pages   []webPageRequest `json:"pages"`
		Notes   string           `json:"notes"`
	}
	if json.Unmarshal([]byte(call.Arguments), &a) != nil {
		return "Error: the arguments are not valid JSON.", nil
	}
	search := call.Name == "web_search"
	var items []webPageRequest
	batch := false
	if search {
		batch = a.Queries != nil
		if batch {
			if a.Query != "" || len(a.Queries) < 1 || len(a.Queries) > 5 {
				return "Error: supply query OR queries (1-5 queries).", nil
			}
			for _, q := range a.Queries {
				items = append(items, webPageRequest{URL: strings.Join(strings.Fields(q), " ")})
			}
		} else {
			items = []webPageRequest{{URL: strings.TrimSpace(a.Query)}}
		}
	} else {
		batch = a.Pages != nil
		if batch {
			if a.URL != "" || len(a.Pages) < 1 || len(a.Pages) > 4 {
				return "Error: supply url OR pages (1-4 pages).", nil
			}
			items = a.Pages
		} else {
			items = []webPageRequest{{URL: a.URL, Offset: a.Offset}}
		}
	}
	if strings.TrimSpace(a.Notes) != "" {
		r.searchNotes = clipRunes(strings.TrimSpace(a.Notes), 4000)
	}
	var jobs []func(context.Context) webOutcome
	seen := map[string]bool{}
	for _, item := range items {
		item.URL = strings.TrimSpace(item.URL)
		item.Offset = max(0, item.Offset)
		if item.URL == "" {
			if !batch {
				if search {
					return "Error: query is empty.", nil
				}
				return "Error: url is empty.", nil
			}
			jobs = append(jobs, func(context.Context) webOutcome { return webOutcome{Result: "Error: empty query or URL."} })
			continue
		}
		key := "search:" + strings.ToLower(strings.Join(strings.Fields(item.URL), " "))
		if !search {
			f := r.s.Fetcher
			if f == nil {
				f = defaultFetcher
			}
			normalized, err := f.Normalize(item.URL)
			if err != nil {
				result := webOutcome{URL: item.URL, Result: "Error: " + toolErrorMessage(err)}
				if !batch {
					return result.Result, nil
				}
				jobs = append(jobs, func(context.Context) webOutcome { return result })
				continue
			}
			item.URL = normalized
			key = fmt.Sprintf("page:%s:%d", normalized, item.Offset)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		r.mu.Lock()
		if r.webCache == nil {
			r.webCache = map[string]*webPromise{}
		}
		promise := r.webCache[key]
		if promise == nil {
			searches, pages := r.webLimits()
			counter, limit, label := &r.searches, searches, "Search"
			if !search {
				counter, limit, label = &r.fetches, pages, "Page"
			}
			if *counter >= limit {
				r.mu.Unlock()
				unit := "searches"
				if !search {
					unit = "pages"
				}
				result := webOutcome{Result: fmt.Sprintf("%s limit for this answer reached (%d %s). Answer with what you have.", label, limit, unit), URL: item.URL}
				if !batch {
					return result.Result, nil
				}
				jobs = append(jobs, func(context.Context) webOutcome { return result })
				continue
			}
			*counter++
			step := conversationWebStep(search, item.URL)
			r.mu.Unlock()
			index := r.start(step)
			promise = &webPromise{run: func(ctx context.Context) webOutcome { return r.executeWeb(ctx, search, item, index) }}
			r.mu.Lock()
			r.webCache[key] = promise
			if search && r.s.PoeSearch != nil {
				r.poeSearchOrder = append(r.poeSearchOrder, item.URL)
			}
		}
		r.mu.Unlock()
		jobs = append(jobs, promise.get)
	}
	return "", func(ctx context.Context) string {
		results := make([]webOutcome, len(jobs))
		tasks := make(chan int, len(jobs))
		for i := range jobs {
			tasks <- i
		}
		close(tasks)
		var wg sync.WaitGroup
		for n := 0; n < min(webToolWorkers, len(jobs)); n++ {
			wg.Go(func() {
				for i := range tasks {
					results[i] = jobs[i](ctx)
				}
			})
		}
		wg.Wait()
		full, short := encodeWebOutcomes(results, batch, false), encodeWebOutcomes(results, batch, true)
		r.mu.Lock()
		if r.webRecords == nil {
			r.webRecords = map[string]webRecord{}
		}
		r.webRecords[call.ID] = webRecord{round: r.round, compact: short}
		r.mu.Unlock()
		return full
	}
}

func encodeWebOutcomes(results []webOutcome, batch, compact bool) string {
	if len(results) == 1 && !batch {
		if compact && results[0].short != "" {
			return results[0].short
		}
		return results[0].Result
	}
	copyResults := append([]webOutcome(nil), results...)
	for i := range copyResults {
		if compact && copyResults[i].short != "" {
			copyResults[i].Result = copyResults[i].short
		}
	}
	data, _ := json.Marshal(map[string]any{"results": copyResults})
	return string(data)
}

func (r *webRun) executeWeb(ctx context.Context, search bool, item webPageRequest, index int) (out webOutcome) {
	if search && r.s.PoeSearch != nil {
		return r.executePoeSearch(ctx, item.URL, index)
	}
	if search {
		out.Query = item.URL
		results, err := r.s.searchWeb(ctx, r.req, r.prepared, item.URL)
		r.finish(index, func(a *conversation.ToolActivity) {
			if err != nil {
				a.Error = toolErrorMessage(err)
			} else {
				a.Results = webLinks(results)
			}
		})
		if err != nil {
			out.Result = "Error: " + toolErrorMessage(err)
			return
		}
		out.Result = formatSearchResults(results)
		small := append([]provider.WebResult(nil), results...)
		for i := range small {
			small[i].Snippet = plainExcerpt(small[i].Snippet, 110)
		}
		out.short = "[Older search output compacted; source URLs preserved.]\n" + formatSearchResults(small)
		return
	}
	out.URL = item.URL
	page, err := r.cachedWebPage(ctx, item.URL)
	window, from, to := pageWindow(page.Text, item.Offset)
	r.finish(index, func(a *conversation.ToolActivity) {
		if err != nil {
			a.Error = toolErrorMessage(err)
			return
		}
		a.URL, a.Title, a.Chars = page.FinalURL, page.Title, len([]rune(page.Text))
		a.Excerpt = plainExcerpt(window, 280)
	})
	if err != nil {
		out.Result = "Error: " + toolErrorMessage(err)
		return
	}
	out.Result = formatPage(page, window, from, to)
	out.short = fmt.Sprintf("URL: %s\nTitle: %s\nCached URL: %s\n[Older page output compacted to selected excerpts, not a complete summary. Original window %d-%d. Use web_fetch with the Cached URL and offset=%d to read the full cached text again.]\n%s", page.FinalURL, page.Title, item.URL, from, to, from, relevantExcerpt(window, r.req.Message, 1800))
	return
}

func relevantExcerpt(text, query string, limit int) string {
	if len([]rune(text)) <= limit {
		return text
	}
	terms := strings.FieldsFunc(strings.ToLower(query), func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) })
	paragraphs := strings.FieldsFunc(text, func(c rune) bool { return c == '\n' })
	if len(paragraphs) < 3 {
		paragraphs = strings.Split(text, ". ")
	}
	type passage struct {
		index, score int
		text         string
	}
	var ranked []passage
	for i, p := range paragraphs {
		score := 0
		for _, term := range terms {
			if len([]rune(term)) >= 4 && strings.Contains(strings.ToLower(p), term) {
				score++
			}
		}
		if i == 0 {
			score++
		}
		ranked = append(ranked, passage{i, score, p})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	remaining := limit
	var chosen []passage
	for _, p := range ranked {
		if remaining <= 0 {
			break
		}
		p.text = clipRunes(strings.TrimSpace(p.text), min(600, remaining))
		remaining -= len([]rune(p.text)) + 2
		chosen = append(chosen, p)
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].index < chosen[j].index })
	var parts []string
	for _, p := range chosen {
		parts = append(parts, p.text)
	}
	return strings.Join(parts, "\n\n")
}

func conversationWebStep(search bool, value string) conversation.ToolActivity {
	if search {
		return conversation.ToolActivity{Tool: "web_search", Query: clipRunes(value, 300)}
	}
	return conversation.ToolActivity{Tool: "web_fetch", URL: clipRunes(value, 2000)}
}

// Offset windows of the same URL share a single download, even in parallel
// and in incognito (where pages must not be stored across requests).
type webPageLoad struct {
	once sync.Once
	page pagestore.Page
	err  error
}

func (r *webRun) cachedWebPage(ctx context.Context, url string) (pagestore.Page, error) {
	r.mu.Lock()
	if r.pageLoads == nil {
		r.pageLoads = map[string]*webPageLoad{}
	}
	load := r.pageLoads[url]
	if load == nil {
		load = &webPageLoad{}
		r.pageLoads[url] = load
	}
	r.mu.Unlock()
	load.once.Do(func() {
		defer func() {
			if rec := recover(); rec != nil {
				load.err = fmt.Errorf("the page reader failed")
			}
		}()
		load.page, load.err = r.s.loadPage(ctx, r.req, r.prepared, url)
	})
	return load.page, load.err
}
