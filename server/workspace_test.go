package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"neochat/auth"
	"neochat/conversation"
)

func getJSON(t *testing.T, srv *Server, method, path, token string, headers map[string]string, out any) int {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response := httptest.NewRecorder()
	srv.Mux().ServeHTTP(response, request)
	if out != nil && response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v (%s)", method, path, err, response.Body)
		}
	}
	return response.Code
}

func TestConversationListAndSearch(t *testing.T) {
	store := conversation.NewInMemoryStore()
	authStore := auth.NewInMemoryStore()
	token, _ := authStore.IssueKey(context.Background(), "u1", "pro")
	srv := &Server{Conversations: store, Auth: authStore}
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 130; i++ {
		store.Append(ctx, "u1", fmt.Sprintf("c%03d", i), conversation.Message{Role: conversation.RoleUser, Content: fmt.Sprintf("chat number %d", i), CreatedAt: at.Add(time.Duration(i) * time.Minute)})
	}
	store.Append(ctx, "u1", "c050", conversation.Message{Role: conversation.RoleAssistant, Content: "Горутины утекают, когда канал никто не читает и они блокируются навсегда.", CreatedAt: at.Add(5 * time.Hour)})
	store.Append(ctx, "other", "x", conversation.Message{Role: conversation.RoleUser, Content: "горутины тоже", CreatedAt: at})
	store.CreateProject(ctx, "u1", conversation.Project{ID: "p1", Name: "Work", CreatedAt: at})
	project := "p1"
	store.UpdateMetadata(ctx, "u1", "c010", conversation.MetadataUpdate{ProjectID: &project})
	store.Append(ctx, "u1", "c010", conversation.Message{Role: conversation.RoleUser, Content: "the quarterly plan draft", CreatedAt: at.Add(10 * time.Minute)})

	var list conversationListResponse
	if code := getJSON(t, srv, "GET", "/conversations", token, nil, &list); code != 200 || len(list.Conversations) != 129 {
		t.Fatalf("list: %d, %d chats (want all 129 outside the project)", code, len(list.Conversations))
	}

	var found searchResponse
	if code := getJSON(t, srv, "GET", "/conversations/search?q=%D0%93%D0%9E%D0%A0%D0%A3%D0%A2%D0%98%D0%9D%D0%AB", token, nil, &found); code != 200 || len(found.Results) != 1 {
		t.Fatalf("search by content: %d %+v", code, found)
	}
	if r := found.Results[0]; r.ID != "c050" || r.Title != "chat number 50" || !strings.Contains(r.Snippet, "Горутины утекают") {
		t.Fatalf("result = %+v", r)
	}
	// Chats inside projects are found too.
	getJSON(t, srv, "GET", "/conversations/search?q=quarterly+plan", token, nil, &found)
	if len(found.Results) != 1 || found.Results[0].ID != "c010" || found.Results[0].ProjectID != "p1" {
		t.Fatalf("project chat search = %+v", found.Results)
	}
	if code := getJSON(t, srv, "GET", "/conversations/search?q=x", token, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("one-letter search: %d", code)
	}

	// Deleting a project keeps its chats, back in the general list.
	if code := getJSON(t, srv, "DELETE", "/projects/p1", token, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete project: %d", code)
	}
	if code := getJSON(t, srv, "DELETE", "/projects/p1", token, nil, nil); code != http.StatusNotFound {
		t.Fatalf("delete missing project: %d", code)
	}
	getJSON(t, srv, "GET", "/conversations", token, nil, &list)
	if len(list.Conversations) != 130 {
		t.Fatalf("after deleting the project the list has %d chats, want 130", len(list.Conversations))
	}
	if projects, _ := store.ListProjects(ctx, "u1"); len(projects) != 0 {
		t.Fatalf("project survived: %+v", projects)
	}
}

func TestBalanceComesFromPolza(t *testing.T) {
	polza := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer pza_good":
			fmt.Fprint(w, `{"amount":"1234.56000000","available":"1200.06000000","reservedAmount":"12.5"}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":"UNAUTHORIZED","message":"bad key"}}`)
		}
	}))
	defer polza.Close()
	authStore := auth.NewInMemoryStore()
	token, _ := authStore.IssueKey(context.Background(), "u1", "pro")
	srv := &Server{Auth: authStore, BalanceURL: polza.URL}

	var balance balanceResponse
	if code := getJSON(t, srv, "GET", "/account/balance", token, map[string]string{providerKeyHeader: "pza_good"}, &balance); code != 200 || balance.AmountRUB != 1234.56 || balance.AvailableRUB != 1200.06 {
		t.Fatalf("balance: %d %+v", code, balance)
	}
	if code := getJSON(t, srv, "GET", "/account/balance", token, map[string]string{providerKeyHeader: "pza_bad"}, nil); code != http.StatusBadGateway {
		t.Fatalf("rejected key: %d", code)
	}
	if code := getJSON(t, srv, "GET", "/account/balance", token, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("no key: %d", code)
	}
}

func TestArtifactFrameIsFramableOnlyHereAndCantCallOut(t *testing.T) {
	response := httptest.NewRecorder()
	(&Server{}).Mux().ServeHTTP(response, httptest.NewRequest("GET", "/artifact-frame", nil))
	csp := response.Header().Get("Content-Security-Policy")
	if response.Code != 200 || response.Header().Get("X-Frame-Options") != "" || !strings.Contains(csp, "frame-ancestors 'self'") || !strings.Contains(csp, "connect-src 'none'") || !strings.Contains(response.Body.String(), "neochat-artifact-ready") {
		t.Fatalf("artifact frame: %d, XFO %q, CSP %q", response.Code, response.Header().Get("X-Frame-Options"), csp)
	}
}
