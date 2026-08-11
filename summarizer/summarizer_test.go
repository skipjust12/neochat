package summarizer

import (
	"context"
	"strings"
	"testing"
	"time"

	"neochat/conversation"
	"neochat/provider"
)

func TestSummarize_SendsSystemPromptAndFoldsPreviousSummary(t *testing.T) {
	fake := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: "updated summary"}}}
	s := New(fake, "gpt-5.6-luna", "system prompt text")

	msgs := []conversation.Message{
		{Role: conversation.RoleUser, Content: "hi", CreatedAt: time.Now()},
		{Role: conversation.RoleAssistant, Content: "hello", CreatedAt: time.Now()},
	}

	got, err := s.Summarize(context.Background(), "previous summary text", msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "updated summary" {
		t.Errorf("got %q, want %q", got, "updated summary")
	}

	if len(fake.Requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(fake.Requests))
	}
	req := fake.Requests[0]
	if req.APIModelID != "gpt-5.6-luna" {
		t.Errorf("APIModelID = %q, want gpt-5.6-luna", req.APIModelID)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != "system prompt text" {
		t.Fatalf("expected the system prompt as the first message, got %+v", req.Messages)
	}
	if !strings.Contains(req.Messages[1].Content, "previous summary text") || !strings.Contains(req.Messages[1].Content, "hi") || !strings.Contains(req.Messages[1].Content, "hello") {
		t.Errorf("expected the user message to include the previous summary and new messages, got %q", req.Messages[1].Content)
	}
}

func TestSummarize_EmptyNewMessagesReturnsError(t *testing.T) {
	fake := &provider.FakeClient{}
	s := New(fake, "gpt-5.6-luna", "system prompt text")

	if _, err := s.Summarize(context.Background(), "", nil); err == nil {
		t.Fatal("expected an error for empty newMessages")
	}
	if len(fake.Requests) != 0 {
		t.Errorf("expected no requests to be sent, got %d", len(fake.Requests))
	}
}

func TestSummarize_GenerateErrorPropagates(t *testing.T) {
	fake := &provider.FakeClient{Err: context.DeadlineExceeded}
	s := New(fake, "gpt-5.6-luna", "system prompt text")

	if _, err := s.Summarize(context.Background(), "", []conversation.Message{{Role: conversation.RoleUser, Content: "hi"}}); err == nil {
		t.Fatal("expected an error when the underlying client fails")
	}
}

func TestSummarize_EmptyReplyReturnsError(t *testing.T) {
	fake := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: "   "}}}
	s := New(fake, "gpt-5.6-luna", "system prompt text")

	if _, err := s.Summarize(context.Background(), "", []conversation.Message{{Role: conversation.RoleUser, Content: "hi"}}); err == nil {
		t.Fatal("expected an error for a blank model reply")
	}
}

func TestLoadSystemPrompt_MissingFile(t *testing.T) {
	if _, err := LoadSystemPrompt("../prompts/does-not-exist.md"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// TestLoadSystemPrompt_PlaceholderFileIsEmptyForNow documents the current
// state of prompts/summarizer_system_prompt.md: it's an intentional empty
// placeholder (real prompt text to be written later), which LoadSystemPrompt
// correctly rejects the same way it would reject any other empty prompt
// file -- cmd/server/main.go will fail to start until that file has real
// content, same as it already does for classifier/moderation prompts.
func TestLoadSystemPrompt_PlaceholderFileIsEmptyForNow(t *testing.T) {
	if _, err := LoadSystemPrompt("../prompts/summarizer_system_prompt.md"); err == nil {
		t.Fatal("expected an error for the still-empty placeholder prompt file -- update this test once it has real content")
	}
}
