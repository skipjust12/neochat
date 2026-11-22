package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"neochat/modelcatalog"
	"neochat/provider"
	"neochat/router"
)

func TestPoeUnlistedBotIsSelectableAndCalledByName(t *testing.T) {
	for _, status := range []int{200, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if r.URL.Path != "/chat/completions" || body["model"] != "GPT-6-Luna" || r.Header.Get("Authorization") != "Bearer poe-test" {
					t.Errorf("bot request=%s %v", r.URL.Path, body)
				}
				if status != 200 {
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":{"message":"Bot not available for this API key","code":404}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Bot answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n")
			}))
			defer api.Close()
			s := newDirectServer(t, &polzaStandIn{})
			base, err := router.LoadCatalog("../configs/models.json")
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := modelcatalog.LoadPoe(base, "../configs/poe-models.json")
			if err != nil {
				t.Fatal(err)
			}
			s.Router.Catalog = catalog
			client := provider.NewPoeClient("")
			client.BaseURL = api.URL
			s.Generators = map[string]provider.Client{"openai": client}
			found := false
			for _, m := range modelEntries(catalog, nil, nil) {
				if m.ID == "gpt-6-luna" {
					found = true
					if !m.Unstable || m.Unavailable {
						t.Fatalf("bot row blocked: %+v", m)
					}
				}
			}
			if !found {
				t.Fatal("bot omitted")
			}
			events, err := collectStream(s, context.Background(), chatRequest{UserID: "u1", PlanID: "pro", RequestedMode: "manual", ManualModelID: "gpt-6-luna", Message: "Hi", providerKey: "poe-test", WebSearch: "off"})
			if status == 200 {
				if err != nil {
					t.Fatal(err)
				}
				done := events["done"][0].(chatResponse)
				if done.ResponseText != "Bot answer" || done.SelectedModelID != "gpt-6-luna" {
					t.Fatalf("done=%+v", done)
				}
			} else if err == nil || !strings.Contains(clientErrorMessage(err), "Poe") {
				t.Fatalf("bot refusal not surfaced: %v", err)
			}
			expected := int32(1)
			if calls.Load() != expected {
				t.Errorf("unexpected fallback/retry: %d calls", calls.Load())
			}
		})
	}
}
func TestPoeCachePricesAreUsedForSettlementAndAggregatedUsage(t *testing.T) {
	read, write := 0.1, 1.25
	result := provider.GenerateResult{Poe: true, InputTokens: 1000, OutputTokens: 10, CachedTokens: 800, CacheWriteTokens: 100, CacheReadPrice: &read, CacheWritePrice: &write}
	want := float64(100+80+125+20) / 1e6
	if got := callCostUSD(1, 2, result); math.Abs(got-want) > 1e-12 {
		t.Fatalf("cache cost=%g want=%g", got, want)
	}
	var aggregate webAnswer
	aggregate.add(result)
	aggregate.add(result)
	if got := callCostUSD(1, 2, aggregate.GenerateResult); math.Abs(got-2*want) > 1e-12 {
		t.Fatalf("tool-loop cache prices lost: cost=%g want=%g", got, 2*want)
	}
	// Missing cache prices are unknown, not a made-up discount or write premium.
	result.CacheReadPrice = nil
	result.CacheWritePrice = nil
	if got := callCostUSD(1, 2, result); math.Abs(got-0.00102) > 1e-12 {
		t.Fatalf("unknown cache pricing=%g", got)
	}
}

func TestPoeSelectedOpusUsesOwnersPublicBot(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "Claude-Opus-5.5" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"Model not found"}}`)
			return
		}
		if body["model"] != "neobotclaude55" {
			t.Errorf("Opus sent to %v", body["model"])
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	}))
	defer api.Close()
	client := provider.NewPoeClient("")
	client.BaseURL = api.URL
	config, err := os.ReadFile("../configs/poe-bot-fallbacks.json")
	if err != nil {
		t.Fatal(err)
	}
	client.BotFallbacks, err = provider.ParsePoeBotAliases(string(config))
	if err != nil {
		t.Fatal(err)
	}
	s := newDirectServer(t, &polzaStandIn{})
	s.Generators = map[string]provider.Client{"anthropic": client}
	base, err := router.LoadCatalog("../configs/models.json")
	if err != nil {
		t.Fatal(err)
	}
	s.Router.Catalog, err = modelcatalog.LoadPoe(base, "../configs/poe-models.json")
	if err != nil {
		t.Fatal(err)
	}
	events, err := collectStream(s, context.Background(), chatRequest{UserID: "u1", PlanID: "pro", RequestedMode: "manual", ManualModelID: "claude-opus-5.5", Message: "Hi", providerKey: "test-poe", WebSearch: "off"})
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	if done.ResponseText != "OK" || done.SelectedModelID != "claude-opus-5.5" {
		t.Fatalf("wrong Opus response %+v", done)
	}
}
