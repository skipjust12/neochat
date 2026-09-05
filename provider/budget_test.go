package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBudgetOutputLimitReachesVendor(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxTokens int  `json:"max_tokens"`
			Stream    bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.MaxTokens != 123 {
			t.Errorf("max_tokens=%d, want 123", body.MaxTokens)
		}
		if body.Stream {
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}
	}))
	defer upstream.Close()
	client := NewOpenRouterClient("test")
	client.baseURL = upstream.URL
	client.MaxTokens = 16000
	ctx := WithOutputLimit(context.Background(), 123)
	if _, err := client.Generate(ctx, "model", nil); err != nil {
		t.Fatal(err)
	}
	ch, err := client.GenerateStream(ctx, "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
}

func TestAmbiguousServerErrorsAreNotRetried(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		if (&StatusError{StatusCode: status}).Retryable() {
			t.Fatalf("unsafe retry for %d", status)
		}
	}
}
