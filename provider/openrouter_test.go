package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestOpenRouterClient_Generate_MaxTokens(t *testing.T) {
	var gotReq openRouterChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`))
	}))
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL
	c.MaxTokens = 4096

	if _, err := c.Generate(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotReq.MaxTokens != 4096 {
		t.Errorf("request max_tokens = %d, want 4096", gotReq.MaxTokens)
	}
}

func TestOpenRouterClient_Generate_MaxTokensOmittedWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "max_tokens") {
			t.Errorf("request body contains max_tokens when OpenRouterClient.MaxTokens was never set: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`))
	}))
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL

	if _, err := c.Generate(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
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

// TestOpenRouterClient_Generate_RespectsContextCancellation verifies the
// README pre-launch checklist's "request-level cancellation" item at the
// one place it actually has to be real: the outbound HTTP call. Generate
// already builds its request with http.NewRequestWithContext, which
// should make net/http abort the round trip the moment ctx is done --
// this pins that behavior down with a server that would otherwise hang
// far longer than the test's timeout, so a regression (e.g. someone
// swapping in http.NewRequest by mistake) would make this test time out
// instead of silently passing.
func TestOpenRouterClient_Generate_RespectsContextCancellation(t *testing.T) {
	block := make(chan struct{}) // never closed -- the handler hangs until the client gives up
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block) // let the handler goroutine exit after the test finishes

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Generate(ctx, "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error once the context deadline is exceeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected the error to wrap context.DeadlineExceeded, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Generate took %s to return after the context deadline passed, want well under the httpClient's 60s timeout", elapsed)
	}
}

// TestOpenRouterClient_GenerateStream parses a real OpenAI-compatible SSE
// stream (multiple delta chunks, a trailing usage-only chunk, then
// [DONE]) the same way a real OpenRouter response is shaped, and checks
// the deltas and final GenerateResult come out right.
func TestOpenRouterClient_GenerateStream(t *testing.T) {
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, line := range []string{
			`data: {"choices":[{"delta":{"content":"hello "}}]}`,
			`data: {"choices":[{"delta":{"content":"there"}}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
			`data: [DONE]`,
		} {
			fmt.Fprintf(w, "%s\n\n", line)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL

	ch, err := c.GenerateStream(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var deltas []string
	var final GenerateResult
	var sawDone bool
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		if chunk.Delta != "" {
			deltas = append(deltas, chunk.Delta)
		}
		if chunk.Done {
			sawDone = true
			final = chunk.Final
		}
	}

	if !sawDone {
		t.Fatal("expected a Done chunk")
	}
	if len(deltas) != 2 || deltas[0] != "hello " || deltas[1] != "there" {
		t.Errorf("deltas = %+v, want [\"hello \", \"there\"]", deltas)
	}
	if final.Text != "hello there" {
		t.Errorf("final.Text = %q, want %q", final.Text, "hello there")
	}
	if final.InputTokens != 7 || final.OutputTokens != 2 {
		t.Errorf("final tokens = %d/%d, want 7/2", final.InputTokens, final.OutputTokens)
	}
	if stream, _ := gotReq["stream"].(bool); !stream {
		t.Error("expected the request to set stream=true")
	}
}

// TestOpenRouterClient_GenerateStream_APIError checks a mid-stream error
// chunk (OpenRouter reports moderation/vendor failures this way even after
// a 200 has already started streaming) surfaces as a StreamChunk.Err.
// TestOpenRouterClient_GenerateStream_OutlivesRequestTimeout is the
// regression test for audit.md's stream-timeout finding. A streamed
// generation legitimately runs far longer than a single non-streaming
// call, and the http.Client.Timeout this replaced applied to reading the
// response body -- so it killed any stream still going after 60s
// regardless of how healthy it was. RequestTimeout is set well below the
// time this stream takes: it must not apply here, while StreamTimeout
// (generous) must.
func TestOpenRouterClient_GenerateStream_OutlivesRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		// Three chunks spread over ~300ms, i.e. 6x the RequestTimeout set
		// below. Under the old shared Client.Timeout this stream would be
		// cut off partway through.
		for _, line := range []string{
			`data: {"choices":[{"delta":{"content":"slow "}}]}`,
			`data: {"choices":[{"delta":{"content":"but "}}]}`,
			`data: {"choices":[{"delta":{"content":"fine"}}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":3}}`,
			`data: [DONE]`,
		} {
			time.Sleep(75 * time.Millisecond)
			fmt.Fprintf(w, "%s\n\n", line)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL
	c.RequestTimeout = 50 * time.Millisecond
	c.StreamTimeout = 30 * time.Second

	ch, err := c.GenerateStream(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var text string
	var final GenerateResult
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatalf("stream errored -- RequestTimeout must not bound a streamed call: %v", chunk.Err)
		}
		text += chunk.Delta
		if chunk.Done {
			final = chunk.Final
		}
	}
	if text != "slow but fine" {
		t.Errorf("streamed text = %q, want %q", text, "slow but fine")
	}
	if final.OutputTokens != 3 {
		t.Errorf("final.OutputTokens = %d, want 3 (the usage chunk must still arrive)", final.OutputTokens)
	}
}

// TestOpenRouterClient_GenerateStream_StreamTimeoutStillApplies checks the
// replacement bound is real: a stream that never finishes is cut off by
// StreamTimeout rather than hanging forever.
func TestOpenRouterClient_GenerateStream_StreamTimeoutStillApplies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never sends a terminal chunk
	}))
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL
	c.StreamTimeout = 150 * time.Millisecond

	ch, err := c.GenerateStream(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never ended -- StreamTimeout did not bound it")
	}
}

func TestOpenRouterClient_GenerateStream_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", `{"error":{"message":"rate limited","code":429}}`)
		flusher.Flush()
	}))
	defer srv.Close()

	c := NewOpenRouterClient("test-key")
	c.baseURL = srv.URL

	ch, err := c.GenerateStream(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error opening the stream: %v", err)
	}

	var streamErr error
	for chunk := range ch {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected the stream to end with an error")
	}
}
