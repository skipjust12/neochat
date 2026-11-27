package provider

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestPoeExplicitBotAliasUsesExactHandle(t *testing.T) {
	c := poeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		b := readPoeBody(t, r)
		if b["model"] != "My-Public-Opus" {
			t.Errorf("model=%v", b["model"])
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
	})
	var err error
	c.BotAliases, err = ParsePoeBotAliases(`{"claude-opus-5.5":"My-Public-Opus"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Generate(context.Background(), "Claude-Opus-5.5", []Message{{Role: "user", Content: "Hi"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`[]`, `{"a":"https://example.com"}`, `{"a":"bot/path"}`, `{"a":12}`} {
		if _, err := ParsePoeBotAliases(raw); err == nil {
			t.Fatalf("accepted invalid aliases %s", raw)
		}
	}
}

func TestPoeBotAliasAndFallbackKeepRequestedReasoningMetadata(t *testing.T) {
	for _, mode := range []string{"alias", "fallback"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				calls := 0
				c := nativeFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.URL.Path == "/Official" && mode == "fallback" {
						w.WriteHeader(404)
						fmt.Fprint(w, "not found")
						return
					}
					if r.URL.Path != "/MyPublicBot" {
						t.Errorf("wrong bot %s", r.URL.Path)
					}
					body := readPoeBody(t, r)
					params := body["query"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
					if params["reasoning_effort"] != "high" {
						t.Errorf("selected effort lost at bot remapping: %v", params)
					}
					fmt.Fprint(w, "event: text\ndata: {\"text\":\"OK\"}\n\nevent: done\ndata: {}\n\n")
				})
				c.ModelInfo = func(model string) PoeModelInfo {
					if model == "Official" {
						return PoeModelInfo{ReasoningParameter: "reasoning_effort", ReasoningValues: []string{"low", "high"}}
					}
					return PoeModelInfo{}
				}
				if mode == "alias" {
					c.BotAliases = map[string]string{"official": "MyPublicBot"}
				} else {
					c.BotFallbacks = map[string]string{"official": "MyPublicBot"}
				}
				ctx := WithReasoning(context.Background(), Reasoning{Effort: "high"})
				messages := []Message{{Role: "user", Content: "Solve exactly"}}
				if stream {
					ch, err := c.GenerateStream(ctx, "Official", messages)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range ch {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else {
					if _, err := c.Generate(ctx, "Official", messages); err != nil {
						t.Fatal(err)
					}
				}
				want := 1
				if mode == "fallback" {
					want = 2
				}
				if calls != want {
					t.Fatalf("calls=%d want=%d", calls, want)
				}
			})
		}
	}
}

func TestPoeHelperDoesNotInheritMainModelMetadata(t *testing.T) {
	c := NewPoeClient("key")
	c.ModelInfo = func(model string) PoeModelInfo {
		if model == "main" {
			return PoeModelInfo{ReasoningParameter: "output_effort", ReasoningValues: []string{"high"}}
		}
		return PoeModelInfo{}
	}
	ctx := WithReasoning(context.Background(), Reasoning{Effort: "high"})
	mainCtx := c.requestModelContext(ctx, "main")
	helperCtx := c.requestModelContext(mainCtx, "helper")
	body := c.chatRequest(helperCtx, "helper", nil, false)
	if body["output_effort"] != nil {
		t.Fatal("helper inherited another model's parameter controls")
	}
}
