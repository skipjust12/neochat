package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"neochat/auth"
	"neochat/modelcatalog"
)

// polzaModelList serves a Polza-style model list: the direct-mode test
// catalog, plus Claude Haiku 5.5 released after it.
func polzaModelList(t *testing.T, calls *atomic.Int32) string {
	t.Helper()
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()
	entry := func(id, name string, created int64) string {
		return fmt.Sprintf(`{"id":%q,"name":%q,"type":"chat","created":%d,"endpoints":["/api/v1/chat/completions"],"context_length":400000,"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"top_provider":{"supported_parameters":["tools","reasoning"],"pricing":{"currency":"RUB","prompt_per_million":"117.068","completion_per_million":"585.34"}}}`, id, name, created)
	}
	body := `{"data":[` + strings.Join([]string{
		entry("openai/gpt-6-luna", "OpenAI: GPT-6 Luna", old),
		entry("deepseek/text", "DeepSeek: DeepSeek Text", old),
		entry("anthropic/claude-opus-5.5", "Anthropic: Claude Opus 5.5", old),
		entry("anthropic/claude-haiku-4.5", "Anthropic: Claude Haiku 4.5", old),
		entry("anthropic/claude-haiku-5.5", "Anthropic: Claude Haiku 5.5", time.Now().Add(-24*time.Hour).Unix()),
	}, ",") + `]}`
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(list.Close)
	return list.URL
}

func TestModels_ListFollowsPolza(t *testing.T) {
	var calls atomic.Int32
	s := newDirectServer(t, &polzaStandIn{reply: "hi"})
	s.Models = modelcatalog.New(s.Router.Catalog, modelcatalog.WithURL(polzaModelList(t, &calls)))
	token, _ := s.Auth.(*auth.InMemoryStore).IssueKey(context.Background(), "u1", "pro")

	// Public: the picker loads it before anything else.
	var list modelsResponse
	if code := getJSON(t, s, "GET", "/models", "", nil, &list); code != 200 || list.UpdatedAt == nil || calls.Load() != 1 {
		t.Fatalf("GET /models: %d, updated %v, %d fetches", code, list.UpdatedAt, calls.Load())
	}
	byID := map[string]modelEntry{}
	for _, m := range list.Models {
		byID[m.ID] = m
	}
	if m := byID["claude-haiku-5.5"]; m.Name != "Claude Haiku 5.5" || m.Legacy || !m.New || m.Provider != "anthropic" || m.Tier != "instant" || strings.Join(m.Inputs, ",") != "text,image" {
		t.Fatalf("Haiku 5.5 = %+v", m)
	}
	if m := byID["claude-haiku-4-5"]; !m.Legacy || m.New {
		t.Fatalf("Haiku 4.5 = %+v, want legacy", m)
	}
	if m := byID["claude-opus-5.5"]; m.Tier != "max" || m.Legacy {
		t.Fatalf("Opus 5.5 = %+v", m)
	}
	getJSON(t, s, "GET", "/models", "", nil, &list)
	if calls.Load() != 1 {
		t.Fatal("a fresh list was fetched again")
	}

	if code := getJSON(t, s, "POST", "/models/refresh", "nope", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("refresh without a key: %d", code)
	}
	if code := getJSON(t, s, "POST", "/models/refresh", token, nil, &list); code != 200 || calls.Load() != 1 {
		t.Fatalf("refresh inside the cooldown: %d, %d fetches", code, calls.Load())
	}
}

func TestModels_RequestForANewModelRefreshesFirst(t *testing.T) {
	var calls atomic.Int32
	standIn := &polzaStandIn{reply: "haiku answer"}
	s := newDirectServer(t, standIn)
	s.Models = modelcatalog.New(s.Router.Catalog, modelcatalog.WithURL(polzaModelList(t, &calls)))
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hello", RequestedMode: "manual", ManualModelID: "claude-haiku-5.5", providerKey: "pza_user"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatalf("a model released since the last refresh: %v", err)
	}
	if done := events["done"][0].(chatResponse); done.SelectedModelID != "claude-haiku-5.5" || standIn.lastBody(t)["model"] != "anthropic/claude-haiku-5.5" {
		t.Fatalf("done = %+v, vendor model %v", done, standIn.lastBody(t)["model"])
	}
	req.ManualModelID = "claude-nonexistent"
	if _, err := collectStream(s, context.Background(), req); err == nil || calls.Load() != 1 {
		t.Fatalf("unknown model: %v, %d fetches (a fresh list isn't fetched again)", err, calls.Load())
	}
}

func TestModels_FixedCatalog(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{reply: "hi"})
	var list modelsResponse
	if code := getJSON(t, s, "GET", "/models", "", nil, &list); code != 200 || len(list.Models) != 4 || list.UpdatedAt != nil {
		t.Fatalf("fixed catalog: %d, %d models", code, len(list.Models))
	}
	raw, _ := json.Marshal(list.Models[0])
	if !strings.Contains(string(raw), `"tier":"instant"`) {
		t.Fatalf("entry = %s", raw)
	}
}
