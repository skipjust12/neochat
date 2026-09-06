package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"neochat/auth"
	"neochat/conversation"
	"neochat/idempotency"
	"neochat/ratelimit"
)

func TestConversationReadEndpointsAreScopedToAuthenticatedUser(t *testing.T) {
	store := conversation.NewInMemoryStore()
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if err := store.Append(context.Background(), "owner", "conversation-1", conversation.Message{Role: conversation.RoleUser, Content: "Saved title", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), "other", "hidden", conversation.Message{Role: conversation.RoleUser, Content: "Must stay private", CreatedAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Conversations: store}
	identity := auth.Identity{UserID: "owner"}

	listRecorder := httptest.NewRecorder()
	srv.handleConversationList(listRecorder, httptest.NewRequest("GET", "/conversations", nil), identity)
	if listRecorder.Code != 200 {
		t.Fatalf("list status = %d, want 200", listRecorder.Code)
	}
	var list conversationListResponse
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Conversations) != 1 || list.Conversations[0].ID != "conversation-1" {
		t.Fatalf("list response = %#v", list)
	}

	historyRequest := httptest.NewRequest("GET", "/conversations/conversation-1", nil)
	historyRequest.SetPathValue("conversation_id", "conversation-1")
	historyRecorder := httptest.NewRecorder()
	srv.handleConversationHistory(historyRecorder, historyRequest, identity)
	if historyRecorder.Code != 200 {
		t.Fatalf("history status = %d, want 200", historyRecorder.Code)
	}
	var history conversationHistoryResponse
	if err := json.Unmarshal(historyRecorder.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Messages) != 1 || history.Messages[0].Content != "Saved title" {
		t.Fatalf("history response = %#v", history)
	}
}

func TestConversationUpdateAndDeleteEndpoints(t *testing.T) {
	store := conversation.NewInMemoryStore()
	mustAppend := func(user, id string) {
		if err := store.Append(context.Background(), user, id, conversation.Message{Role: conversation.RoleUser, Content: "Original", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	mustAppend("owner", "chat")
	mustAppend("other", "chat")
	srv := &Server{Conversations: store}
	identity := auth.Identity{UserID: "owner"}

	updateRequest := httptest.NewRequest(http.MethodPatch, "/conversations/chat", bytes.NewBufferString(`{"title":"Renamed","pinned":true}`))
	updateRequest.SetPathValue("conversation_id", "chat")
	updateRecorder := httptest.NewRecorder()
	srv.handleConversationUpdate(updateRecorder, updateRequest, identity)
	if updateRecorder.Code != http.StatusNoContent {
		t.Fatalf("update status = %d: %s", updateRecorder.Code, updateRecorder.Body.String())
	}
	list, err := store.List(context.Background(), "owner", 10)
	if err != nil || len(list) != 1 || list[0].Title != "Renamed" || !list[0].Pinned {
		t.Fatalf("updated list = %+v, err = %v", list, err)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/conversations/chat", nil)
	deleteRequest.SetPathValue("conversation_id", "chat")
	deleteRecorder := httptest.NewRecorder()
	srv.handleConversationDelete(deleteRecorder, deleteRequest, identity)
	if deleteRecorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	ownerHistory, _ := store.History(context.Background(), "owner", "chat", 0)
	otherHistory, _ := store.History(context.Background(), "other", "chat", 0)
	if len(ownerHistory) != 0 || len(otherHistory) != 1 {
		t.Fatalf("delete scope owner=%+v other=%+v", ownerHistory, otherHistory)
	}
}

func TestHistoryHTTPPaginationAndUserLimit(t *testing.T) {
	ctx := context.Background()
	store := conversation.NewInMemoryStore()
	credentials := auth.NewInMemoryStore()
	token, err := credentials.IssueKey(ctx, "owner", "pro")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 45; i++ {
		if err := store.Append(ctx, "owner", "long", conversation.Message{Role: conversation.RoleUser, Content: "message"}); err != nil {
			t.Fatal(err)
		}
	}
	srv := &Server{Conversations: store, Auth: credentials, UserRateLimiter: ratelimit.NewInMemoryLimiter(2, time.Hour), Idempotency: idempotency.NewInMemoryStore()}
	mux := srv.Mux()
	request := func(url, ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", url, nil)
		req.RemoteAddr = ip
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		return response
	}
	first := request("/conversations/long", "192.0.2.10:1234")
	if first.Code != 200 {
		t.Fatalf("first status %d", first.Code)
	}
	var page conversationHistoryResponse
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 20 || page.NextCursor == 0 {
		t.Fatalf("unbounded history: %+v", page)
	}
	if first.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private history cacheable")
	}
	second := request("/conversations/long?before="+strconv.FormatInt(page.NextCursor, 10), "192.0.2.11:1234")
	if second.Code != 200 {
		t.Fatalf("second status %d", second.Code)
	}
	if third := request("/conversations/long", "192.0.2.12:1234"); third.Code != 429 {
		t.Fatalf("IP change bypassed user limit: %d", third.Code)
	}
}
