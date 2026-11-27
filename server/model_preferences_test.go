package server

import (
	"context"
	"encoding/json"
	"neochat/auth"
	"neochat/settings"
	"strings"
	"testing"
)

func TestModelLibraryOverridesArePersistentPrivateAndRoutable(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{})
	s.Settings = settings.NewInMemoryStore()
	token, _ := s.Auth.(*auth.InMemoryStore).IssueKey(context.Background(), "u1", "pro")
	other, _ := s.Auth.(*auth.InMemoryStore).IssueKey(context.Background(), "u2", "pro")
	patch := `{"name":"My Luna","description":"For quick work","url":"https://poe.com/My-Public-Luna","provider":"openai","tier":"thinking","context_window":1048576,"legacy":true}`
	if got := sendJSON(t, s, "PATCH", "/account/models/gpt-6-luna", token, patch, nil); got.Code != 200 {
		t.Fatalf("patch %d %s", got.Code, got.Body)
	}
	req := chatRequest{UserID: "u1", RequestedMode: "manual", ManualModelID: "gpt-6-luna"}
	if err := s.loadCustomModel(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	m, ok := s.requestRouter(req).Catalog.FindModel(req.ManualModelID)
	if !ok || m.DisplayName != "My Luna" || m.ResolveAPIModelID() != "My-Public-Luna" || m.ContextWindow != 1048576 || modelTier(m) != "thinking" {
		t.Fatalf("manual route %+v", m)
	}
	original, _ := s.router().Catalog.FindModel("gpt-6-luna")
	if original.DisplayName == "My Luna" {
		t.Fatal("changed shared catalog")
	}
	var data modelsResponse
	getJSON(t, s, "GET", "/account/models", other, nil, &data)
	if len(data.Preferences) != 0 || len(data.LegacyOverrides) != 0 {
		t.Fatal("leaked account overrides")
	}
	if got := sendJSON(t, s, "PUT", "/account/settings", token, `{"theme":"light","model_preferences":{},"model_order":[]}`, nil); got.Code != 204 {
		t.Fatal(got.Body)
	}
	getJSON(t, s, "GET", "/account/models", token, nil, &data)
	if data.Preferences["gpt-6-luna"].Name != "My Luna" {
		t.Fatal("ordinary settings save lost model preferences")
	}
	if got := sendJSON(t, s, "PATCH", "/account/models/gpt-6-luna", token, `{"reset":true}`, nil); got.Code != 200 {
		t.Fatal(got.Body)
	}
	var reset modelsResponse
	getJSON(t, s, "GET", "/account/models", token, nil, &reset)
	if len(reset.Preferences) > 0 || len(reset.LegacyOverrides) > 0 {
		t.Fatal("reset did not restore catalog")
	}
}
func TestModelLibraryOrderAndDuplicateRemoval(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{})
	s.Settings = settings.NewInMemoryStore()
	token, _ := s.Auth.(*auth.InMemoryStore).IssueKey(context.Background(), "u1", "pro")
	got := sendJSON(t, s, "POST", "/account/models", token, `{"provider":"openai","url":"GPT-6-Luna","name":"Personal Luna","tier":"instant","description":"Fast","context_window":1048576}`, nil)
	var added modelsResponse
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &added) != nil || len(added.Models) != 1 {
		t.Fatalf("add %d %s", got.Code, got.Body)
	}
	id := added.Models[0].ID
	if added.Models[0].Description != "Fast" || added.Models[0].Tier != "instant" || !added.Models[0].Custom {
		t.Fatalf("custom entry %+v", added.Models[0])
	}
	order := `{"ids":["` + id + `","gpt-6-luna","deepseek-text"]}`
	if got := sendJSON(t, s, "PUT", "/account/models/order", token, order, nil); got.Code != 200 {
		t.Fatal(got.Body)
	}
	for _, bad := range []string{`{"ids":["gpt-6-luna","gpt-6-luna"]}`, `{"ids":["unknown"]}`} {
		if got := sendJSON(t, s, "PUT", "/account/models/order", token, bad, nil); got.Code != 400 {
			t.Fatalf("invalid order accepted %d", got.Code)
		}
	}
	// A failed bulk action must not partly remove the first valid entry.
	if got := sendJSON(t, s, "POST", "/account/models/actions", token, `{"action":"remove","ids":["`+id+`","unknown"]}`, nil); got.Code != 404 {
		t.Fatal(got.Body)
	}
	var before modelsResponse
	getJSON(t, s, "GET", "/account/models", token, nil, &before)
	if len(before.Models) != 1 || len(before.Order) != 3 || before.Order[0] != id {
		t.Fatalf("state after failed action %+v", before)
	}
	got = sendJSON(t, s, "POST", "/account/models/actions", token, `{"action":"remove","ids":["`+id+`","gpt-6-luna"]}`, nil)
	var after modelsResponse
	json.Unmarshal(got.Body.Bytes(), &after)
	if got.Code != 200 || len(after.Models) != 0 || !after.Preferences["gpt-6-luna"].Hidden || len(after.Order) != 2 {
		t.Fatalf("cleanup %d %+v", got.Code, after)
	}
	if got := sendJSON(t, s, "PATCH", "/account/models/gpt-6-luna", token, `{"hidden":false}`, nil); got.Code != 200 {
		t.Fatal(got.Body)
	}
}
func TestModelLibraryRejectsUnsafeOverrides(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{})
	s.Settings = settings.NewInMemoryStore()
	token, _ := s.Auth.(*auth.InMemoryStore).IssueKey(context.Background(), "u1", "pro")
	for _, body := range []string{`{"url":"javascript:alert(1)"}`, `{"url":"https://evil.example/Bot"}`, `{"tier":"free"}`, `{"provider":"unknown"}`, `{"context_window":-1}`, `{"description":"` + strings.Repeat("a", 161) + `"}`, `{"name":"bad\nname"}`} {
		if got := sendJSON(t, s, "PATCH", "/account/models/gpt-6-luna", token, body, nil); got.Code != 400 {
			t.Errorf("accepted %s: %d", body, got.Code)
		}
	}
}
