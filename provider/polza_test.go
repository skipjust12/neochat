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

func TestPolzaClient_Generate(t *testing.T) {
	var gotReq polzaChatRequest
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

	c := NewPolzaClient("test-key")
	c.BaseURL = srv.URL

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

func TestPolzaClient_Generate_MaxTokens(t *testing.T) {
	var gotReq polzaChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`))
	}))
	defer srv.Close()

	c := NewPolzaClient("test-key")
	c.BaseURL = srv.URL
	c.MaxTokens = 4096

	if _, err := c.Generate(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotReq.MaxTokens != 4096 {
		t.Errorf("request max_tokens = %d, want 4096", gotReq.MaxTokens)
	}
}

func TestPolzaClient_Generate_MaxTokensOmittedWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "max_tokens") {
			t.Errorf("request body contains max_tokens when PolzaClient.MaxTokens was never set: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`))
	}))
	defer srv.Close()

	c := NewPolzaClient("test-key")
	c.BaseURL = srv.URL

	if _, err := c.Generate(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPolzaClient_UsesPerRequestKeyAndReasoning(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBody = nil
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`))
	}))
	defer srv.Close()

	c := NewPolzaClient("")
	c.BaseURL = srv.URL

	ctx := WithReasoning(WithAPIKey(context.Background(), "pza_user"), Reasoning{Effort: "high"})
	if _, err := c.Generate(ctx, "openai/gpt-6-luna", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer pza_user" {
		t.Errorf("Authorization = %q, want the per-request key", gotAuth)
	}
	reasoning, _ := gotBody["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["type"] != nil {
		t.Errorf("reasoning = %v, want {effort: high}", gotBody["reasoning"])
	}
	if _, ok := gotBody["reasoning_effort"]; ok {
		t.Error("top-level reasoning_effort is ignored by Polza and must not be sent")
	}

	ctx = WithReasoning(WithAPIKey(context.Background(), "pza_user"), Reasoning{Effort: "max", Adaptive: true})
	if _, err := c.Generate(ctx, "anthropic/claude-opus-5.5", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	reasoning, _ = gotBody["reasoning"].(map[string]any)
	if reasoning["type"] != "adaptive" || reasoning["effort_level"] != "max" || reasoning["effort"] != nil {
		t.Errorf("adaptive reasoning = %v, want {type: adaptive, effort_level: max}", gotBody["reasoning"])
	}

	if _, err := c.Generate(WithAPIKey(context.Background(), "pza_user"), "m", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := gotBody["reasoning"]; ok {
		t.Errorf("no reasoning requested, but body carried %v", gotBody["reasoning"])
	}
}

func TestPolzaClient_MissingKey(t *testing.T) {
	c := NewPolzaClient("")
	if _, err := c.Generate(context.Background(), "m", []Message{{Role: "user", Content: "hi"}}); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("Generate err = %v, want ErrMissingAPIKey", err)
	}
	if _, err := c.GenerateStream(context.Background(), "m", []Message{{Role: "user", Content: "hi"}}); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("GenerateStream err = %v, want ErrMissingAPIKey", err)
	}
}

func TestVendorMessage(t *testing.T) {
	body := `{"error":{"code":"UNAUTHORIZED","message":"Неверные учётные данные авторизации","trace_id":"x"}}`
	if got := VendorMessage(body); got != "Неверные учётные данные авторизации" {
		t.Errorf("VendorMessage = %q", got)
	}
	if got := VendorMessage("not json"); got != "" {
		t.Errorf("VendorMessage(non-json) = %q, want empty", got)
	}
}

func TestPolzaClient_Generate_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": {"message": "No endpoints found for test-vendor/test-model", "code": 404}}`))
	}))
	defer srv.Close()

	c := NewPolzaClient("test-key")
	c.BaseURL = srv.URL

	_, err := c.Generate(context.Background(), "test-vendor/test-model", []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected an error for a 404 response, got nil")
	}
}

// TestPolzaClient_Generate_RespectsContextCancellation verifies the
// README pre-launch checklist's "request-level cancellation" item at the
// one place it actually has to be real: the outbound HTTP call. Generate
// already builds its request with http.NewRequestWithContext, which
// should make net/http abort the round trip the moment ctx is done --
// this pins that behavior down with a server that would otherwise hang
// far longer than the test's timeout, so a regression (e.g. someone
// swapping in http.NewRequest by mistake) would make this test time out
// instead of silently passing.
func TestPolzaClient_Generate_RespectsContextCancellation(t *testing.T) {
	block := make(chan struct{}) // never closed -- the handler hangs until the client gives up
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block) // let the handler goroutine exit after the test finishes

	c := NewPolzaClient("test-key")
	c.BaseURL = srv.URL

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

// TestPolzaClient_GenerateStream parses a real OpenAI-compatible SSE
// stream (multiple delta chunks, a trailing usage-only chunk, then
// [DONE]) the same way a real Polza response is shaped, and checks
// the deltas and final GenerateResult come out right.
func TestPolzaClient_GenerateStream(t *testing.T) {
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

	c := NewPolzaClient("test-key")
	c.BaseURL = srv.URL

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

// TestPolzaClient_GenerateStream_APIError checks a mid-stream error
// chunk (Polza reports moderation/vendor failures this way even after
// a 200 has already started streaming) surfaces as a StreamChunk.Err.
// TestPolzaClient_GenerateStream_OutlivesRequestTimeout is the
// regression test for audit.md's stream-timeout finding. A streamed
// generation legitimately runs far longer than a single non-streaming
// call, and the http.Client.Timeout this replaced applied to reading the
// response body -- so it killed any stream still going after 60s
// regardless of how healthy it was. RequestTimeout is set well below the
// time this stream takes: it must not apply here, while StreamTimeout
// (generous) must.
func TestPolzaClient_GenerateStream_OutlivesRequestTimeout(t *testing.T) {
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

	c := NewPolzaClient("test-key")
	c.BaseURL = srv.URL
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

// TestPolzaClient_GenerateStream_StreamTimeoutStillApplies checks the
// replacement bound is real: a stream that never finishes is cut off by
// StreamTimeout rather than hanging forever.
func TestPolzaClient_GenerateStream_StreamTimeoutStillApplies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never sends a terminal chunk
	}))
	defer srv.Close()

	c := NewPolzaClient("test-key")
	c.BaseURL = srv.URL
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

func TestPolzaClient_GenerateStream_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", `{"error":{"message":"rate limited","code":429}}`)
		flusher.Flush()
	}))
	defer srv.Close()

	c := NewPolzaClient("test-key")
	c.BaseURL = srv.URL

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

func TestPolzaClient_EncodesMultimodalParts(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`))
	}))
	defer srv.Close()
	c := NewPolzaClient("k")
	c.BaseURL = srv.URL

	_, err := c.Generate(context.Background(), "m", []Message{
		{Role: "system", Content: "be nice"},
		{Role: "user", Content: "what is in these?", Parts: []Part{
			{Type: PartImage, MIME: "image/png", Name: "a.png", Data: []byte{1, 2}},
			{Type: PartFile, MIME: "application/pdf", Name: "r.pdf", Data: []byte("%PDF")},
			{Type: PartText, Text: "what is in these?"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := gotBody["messages"].([]any)
	if messages[0].(map[string]any)["content"] != "be nice" {
		t.Errorf("plain message must stay a string, got %v", messages[0])
	}
	parts := messages[1].(map[string]any)["content"].([]any)
	image := parts[0].(map[string]any)
	file := parts[1].(map[string]any)
	text := parts[2].(map[string]any)
	if image["type"] != "image_url" || image["image_url"].(map[string]any)["url"] != "data:image/png;base64,AQI=" {
		t.Errorf("image part = %v", image)
	}
	if file["type"] != "file" || file["file"].(map[string]any)["filename"] != "r.pdf" || file["file"].(map[string]any)["file_data"] != "data:application/pdf;base64,JVBERg==" {
		t.Errorf("file part = %v", file)
	}
	if text["type"] != "text" || text["text"] != "what is in these?" {
		t.Errorf("text part = %v", text)
	}
}
