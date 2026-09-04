package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"neochat/auth"
	"neochat/conversation"
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
