package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearXNGSearch(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"query":"x","results":[
			{"title":"A","url":"https://a.example","content":"about a","engine":"google"},
			{"title":"B","url":"https://b.example","content":"about b","engine":"bing"},
			{"title":"C","url":"https://c.example","content":"about c","engine":"brave"}]}`)
	}))
	defer srv.Close()

	s := &SearXNG{BaseURL: srv.URL + "/"}
	results, err := s.Search(context.Background(), "новости & more", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Title != "A" || results[0].URL != "https://a.example" || results[1].Snippet != "about b" {
		t.Fatalf("results = %+v", results)
	}
	q := got.URL.Query()
	if got.URL.Path != "/search" || q.Get("q") != "новости & more" || q.Get("format") != "json" || q.Get("language") != "all" {
		t.Errorf("request = %s", got.URL)
	}
}

func TestSearXNGErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"json off":       {http.StatusForbidden, `Forbidden`, "the search service answered HTTP 403"},
		"engines failed": {http.StatusOK, `{"results":[],"unresponsive_engines":[["google","CAPTCHA"]]}`, "no search engine answered"},
		"not json":       {http.StatusOK, `<html>`, "the search service sent an unreadable answer"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		}))
		_, err := (&SearXNG{BaseURL: srv.URL}).Search(context.Background(), "q", 10)
		srv.Close()
		var searchErr *Error
		if !errors.As(err, &searchErr) || searchErr.Message != tc.want {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}

	// No results, every engine fine: an empty list, not an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[],"unresponsive_engines":[]}`)
	}))
	defer srv.Close()
	if results, err := (&SearXNG{BaseURL: srv.URL}).Search(context.Background(), "q", 10); err != nil || len(results) != 0 {
		t.Errorf("empty search = %v, %v", results, err)
	}
}
