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
