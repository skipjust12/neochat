package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenRouterClient_Generate(t *testing.T) {
	var gotReq openRouterChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer test-key")
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hello back"}}],
			"usage": {"prompt_tokens": 12, "completion_tokens": 4}
		}`))
	}))
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL

	result, err := c.Generate(context.Background(), "test-vendor/test-model", []Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotReq.Model != "test-vendor/test-model" {
		t.Errorf("request model = %q, want test-vendor/test-model", gotReq.Model)
	}
	if len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" || gotReq.Messages[1].Role != "user" {
		t.Errorf("request messages = %+v, want [system, user]", gotReq.Messages)
	}

	if result.Text != "hello back" {
		t.Errorf("Text = %q, want %q", result.Text, "hello back")
	}
	if result.InputTokens != 12 || result.OutputTokens != 4 {
		t.Errorf("tokens = %d/%d, want 12/4", result.InputTokens, result.OutputTokens)
	}
}

func TestOpenRouterClient_Generate_OptionalHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("HTTP-Referer"); got != "https://example.com" {
			t.Errorf("HTTP-Referer = %q, want https://example.com", got)
		}
		if got := r.Header.Get("X-Title"); got != "NeoChat" {
			t.Errorf("X-Title = %q, want NeoChat", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`))
	}))
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL
	c.Referer = "https://example.com"
	c.Title = "NeoChat"

	if _, err := c.Generate(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenRouterClient_Generate_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": {"message": "No endpoints found for test-vendor/test-model", "code": 404}}`))
	}))
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL

	_, err := c.Generate(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected an error for a 404 response, got nil")
	}
}
