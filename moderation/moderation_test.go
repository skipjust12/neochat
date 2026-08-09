package moderation

import (
	"context"
	"testing"

	"neochat/provider"
)

const notFlaggedReply = `{"flagged": false, "categories": [], "reason": "ordinary request"}`
const flaggedReply = `{"flagged": true, "categories": ["illegal_activity"], "reason": "asks how to commit a specific crime"}`

func TestModerate_ParsesNotFlaggedReply(t *testing.T) {
	fake := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: notFlaggedReply}}}
	m := New(fake, "gpt-oss-120b", "system prompt text")

	out, err := m.Moderate(context.Background(), "how do I bake bread?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Flagged {
		t.Errorf("Flagged = true, want false")
	}
	if len(out.Categories) != 0 {
		t.Errorf("Categories = %v, want empty", out.Categories)
	}

	if len(fake.Requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(fake.Requests))
	}
	req := fake.Requests[0]
	if req.APIModelID != "gpt-oss-120b" {
		t.Errorf("APIModelID = %q, want gpt-oss-120b", req.APIModelID)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != "system prompt text" {
		t.Errorf("expected the system prompt as the first message, got %+v", req.Messages)
	}
}

func TestModerate_ParsesFlaggedReply(t *testing.T) {
	fake := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: flaggedReply}}}
	m := New(fake, "gpt-oss-120b", "system prompt text")

	out, err := m.Moderate(context.Background(), "bad request")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Flagged {
		t.Errorf("Flagged = false, want true")
	}
	if len(out.Categories) != 1 || out.Categories[0] != "illegal_activity" {
		t.Errorf("Categories = %v, want [illegal_activity]", out.Categories)
	}
}

func TestModerate_StripsCodeFence(t *testing.T) {
	fake := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: "```json\n" + notFlaggedReply + "\n```"}}}
	m := New(fake, "gpt-oss-120b", "system prompt text")

	out, err := m.Moderate(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error parsing a fenced reply: %v", err)
	}
	if out.Flagged {
		t.Errorf("Flagged = true, want false")
	}
}

func TestModerate_InvalidJSONReturnsError(t *testing.T) {
	fake := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: "not json at all"}}}
	m := New(fake, "gpt-oss-120b", "system prompt text")

	if _, err := m.Moderate(context.Background(), "hi"); err == nil {
		t.Fatal("expected an error for a non-JSON reply, got nil")
	}
}

func TestLoadSystemPrompt(t *testing.T) {
	prompt, err := LoadSystemPrompt("../prompts/moderation_system_prompt.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prompt == "" {
		t.Error("expected a non-empty prompt")
	}
}

func TestLoadSystemPrompt_MissingFile(t *testing.T) {
	if _, err := LoadSystemPrompt("../prompts/does-not-exist.md"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
