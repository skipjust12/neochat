// Package websearch runs web searches for the web_search tool on a
// SearXNG instance (https://docs.searxng.org), in place of Polza's web
// plugin. SearXNG keeps no index of its own: each query goes out live to
// the engines it is set up with (Google, Bing, DuckDuckGo, Brave, ...),
// and their results come back merged.
//
// The instance is the server's own (compose.production.yml), reached on
// the internal network, so no address checks are needed here: the URL
// comes from configuration, never from a model.
package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"neochat/provider"
)

// Defaults for a zero SearXNG field.
const (
	DefaultTimeout  = 15 * time.Second
	DefaultLanguage = "all"
	maxBodyBytes    = 2 << 20
)

// SearXNG queries one SearXNG instance's JSON API. BaseURL is required;
// the instance must list "json" in search.formats.
type SearXNG struct {
	BaseURL string
	// Language is SearXNG's language parameter ("all", "en", "ru-RU", ...).
	Language string
	Timeout  time.Duration

	once   sync.Once
	client *http.Client
}

// Error is a search failure worth showing to the model and the user as is.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

// Search returns up to max results for query, in SearXNG's order (results
// several engines agree on come first).
func (s *SearXNG) Search(ctx context.Context, query string, max int) ([]provider.WebResult, error) {
	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("language", s.language())
	params.Set("safesearch", "0")
	endpoint := strings.TrimRight(s.BaseURL, "/") + "/search?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Message: "the search service is unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Message: fmt.Sprintf("the search service answered HTTP %d", resp.StatusCode)}
	}
	var body struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
		UnresponsiveEngines [][]string `json:"unresponsive_engines"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body); err != nil {
		return nil, &Error{Message: "the search service sent an unreadable answer"}
	}
	if len(body.Results) == 0 && len(body.UnresponsiveEngines) > 0 {
		// Nothing found because every engine failed (blocked, timed out),
		// not because the web has nothing: say so, so the model doesn't
		// rephrase the same query over and over.
		return nil, &Error{Message: "no search engine answered"}
	}
	out := make([]provider.WebResult, 0, min(len(body.Results), max))
	for _, r := range body.Results {
		if len(out) == max {
			break
		}
		out = append(out, provider.WebResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

func (s *SearXNG) language() string {
	if s.Language != "" {
		return s.Language
	}
	return DefaultLanguage
}

func (s *SearXNG) httpClient() *http.Client {
	s.once.Do(func() {
		timeout := s.Timeout
		if timeout == 0 {
			timeout = DefaultTimeout
		}
		s.client = &http.Client{Timeout: timeout}
	})
	return s.client
}
