package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"neochat/auth"
	"neochat/provider"
	"neochat/settings"
)

func TestCustomModelsPersistRouteAndIsolateUsers(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/chat/completions" || body["model"] != "Claude-Fable-5.2" || r.Header.Get("Authorization") != "Bearer poe-test" {
			t.Errorf("request: %s %v", r.URL.Path, body)
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Fable answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10}}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"Fable answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
		}
	}))
	defer api.Close()
	s := newDirectServer(t, &polzaStandIn{})
	s.Settings = settings.NewInMemoryStore()
	client := provider.NewPoeClient("")
	client.BaseURL = api.URL
	s.Generators["anthropic"] = client
	token, _ := s.Auth.(*auth.InMemoryStore).IssueKey(context.Background(), "u1", "pro")
	other, _ := s.Auth.(*auth.InMemoryStore).IssueKey(context.Background(), "u2", "pro")
	body := `{"provider":"anthropic","url":"https://poe.com/Claude-Fable-5.2?share=1"}`
	got := sendJSON(t, s, "POST", "/account/models", token, body, nil)
	if got.Code != 200 {
		t.Fatalf("add: %d %s", got.Code, got.Body)
	}
	var list modelsResponse
	json.Unmarshal(got.Body.Bytes(), &list)
	if len(list.Models) != 1 || list.Models[0].Name != "Claude Fable 5.2" {
		t.Fatalf("list: %+v", list)
	}
	id := list.Models[0].ID
	if got := sendJSON(t, s, "PATCH", "/account/models/gpt-6-luna", token, `{"legacy":true}`, nil); got.Code != 200 {
		t.Fatalf("move to Legacy: %d %s", got.Code, got.Body)
	}
	if got := sendJSON(t, s, "PATCH", "/account/models/"+id, token, `{"legacy":true}`, nil); got.Code != 200 {
		t.Fatalf("custom Legacy: %d", got.Code)
	}
	if got := sendJSON(t, s, "PATCH", "/account/models/unknown", token, `{"legacy":true}`, nil); got.Code != 404 {
		t.Fatalf("unknown model: %d", got.Code)
	}

	if got := sendJSON(t, s, "GET", "/account/models", other, "", nil); strings.Contains(got.Body.String(), id) {
		t.Fatal("model leaked to another user")
	}
	if got := sendJSON(t, s, "PUT", "/account/settings", token, `{"theme":"dark","custom_models":[],"model_legacy":{}}`, nil); got.Code != 204 {
		t.Fatalf("preferences save: %d %s", got.Code, got.Body)
	}
	// A fresh Server uses persisted definitions, without an in-memory catalog mutation.
	fresh := newDirectServer(t, &polzaStandIn{})
	fresh.Settings = s.Settings
	fresh.Generators["anthropic"] = client
	var persisted modelsResponse
	getJSON(t, s, "GET", "/account/models", token, nil, &persisted)
	if !persisted.LegacyOverrides["gpt-6-luna"] || !persisted.LegacyOverrides[id] {
		t.Fatal("preferences save lost Legacy choices")
	}
	if m, _ := s.router().Catalog.FindModel("gpt-6-luna"); m.Legacy {
		t.Fatal("private Legacy choice changed shared catalog")
	}
	var isolated modelsResponse
	getJSON(t, s, "GET", "/account/models", other, nil, &isolated)
	if len(isolated.LegacyOverrides) > 0 {
		t.Fatal("Legacy choice leaked to another user")
	}
	if got := sendJSON(t, s, "PATCH", "/account/models/gpt-6-luna", token, `{"legacy":false}`, nil); got.Code != 200 {
		t.Fatal("restore failed")
	}
	getJSON(t, s, "GET", "/account/models", token, nil, &persisted)
	if value, ok := persisted.LegacyOverrides["gpt-6-luna"]; !ok || value {
		t.Fatal("explicit restore was not saved")
	}

	req := chatRequest{UserID: "u1", PlanID: "pro", RequestedMode: "manual", ManualModelID: id, Message: "Hi", providerKey: "poe-test", WebSearch: "off"}
	response, err := fresh.handle(context.Background(), req, fresh.Plans["pro"])
	if err != nil || response.ResponseText != "Fable answer" {
		t.Fatalf("non-stream: %+v %v", response, err)
	}
	events, err := collectStream(fresh, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if done := events["done"][0].(chatResponse); done.SelectedModelID != id || done.ResponseText != "Fable answer" {
		t.Fatalf("stream: %+v", done)
	}
	req.UserID = "u2"
	if _, err := collectStream(fresh, context.Background(), req); err == nil {
		t.Fatal("another user could call private model")
	}
	if _, ok := fresh.router().Catalog.FindModel(id); ok {
		t.Fatal("custom model entered shared catalog")
	}
	if got := sendJSON(t, s, "DELETE", "/account/models/"+id, token, "", nil); got.Code != 200 {
		t.Fatalf("delete: %d", got.Code)
	}
	req.UserID = "u1"
	if _, err := collectStream(fresh, context.Background(), req); err == nil {
		t.Fatal("deleted model still routable")
	}
	saved, _ := s.Settings.Get(context.Background(), "u1")
	if !strings.Contains(string(saved), `"theme":"dark"`) {
		t.Fatal("model deletion lost preferences")
	}
}

func TestCustomModelValidation(t *testing.T) {
	for _, input := range []string{"https://evil.example/Bot", "https://poe.com@evil.example/Bot", "http://poe.com/Bot", "https://poe.com/Bot/more", "../Bot", "https://poe.com/%2fBot", ""} {
		if _, err := normalizeCustomModel(customModel{Provider: "openai", URL: input}); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	for _, input := range []string{"GPT-6-Luna", "https://poe.com/GPT-6-Luna", "https://poe.com/GPT-6-Luna/api"} {
		m, err := normalizeCustomModel(customModel{Provider: "openai", URL: input})
		if err != nil || m.Handle != "GPT-6-Luna" || m.Name != "GPT-6 Luna" {
			t.Errorf("normalize %q: %+v %v", input, m, err)
		}
	}
}
