package classifier

import (
	"context"
	"testing"

	"neochat/provider"
)

const sampleReply = `{
	"schema_version": "1.0",
	"task_type": "code_generation",
	"language": "ru",
	"modality_input": ["text", "code"],
	"modality_output_expected": ["text", "code"],
	"reasoning_depth": "moderate",
	"creativity_level": "low",
	"required_tools": [],
	"expected_output_length": "medium",
	"estimated_output_tokens": 400,
	"output_format": "",
	"complexity_score": 0.5,
	"context_dependency": "light",
	"confidence": 0.9,
	"content_flags": [],
	"safety_risk_score": 0.0
}`

func TestClassify_ParsesReply(t *testing.T) {
	fake := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: sampleReply}}}
	c := New(fake, "gpt-5.6-luna", "system prompt text")

	out, _, err := c.Classify(context.Background(), "write a function that sorts a list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.TaskType != "code_generation" || out.ReasoningDepth != "moderate" {
		t.Errorf("unexpected parsed output: %+v", out)
	}

	if len(fake.Requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(fake.Requests))
	}
	req := fake.Requests[0]
	if req.APIModelID != "gpt-5.6-luna" {
		t.Errorf("APIModelID = %q, want gpt-5.6-luna", req.APIModelID)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != "system prompt text" {
		t.Errorf("expected the system prompt as the first message, got %+v", req.Messages)
	}
}

func TestClassify_StripsCodeFence(t *testing.T) {
	fake := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: "```json\n" + sampleReply + "\n```"}}}
	c := New(fake, "gpt-5.6-luna", "system prompt text")

	out, _, err := c.Classify(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error parsing a fenced reply: %v", err)
	}
	if out.TaskType != "code_generation" {
		t.Errorf("unexpected parsed output: %+v", out)
	}
}

func TestClassify_InvalidJSONReturnsError(t *testing.T) {
	fake := &provider.FakeClient{Responses: []provider.GenerateResult{{Text: "not json at all"}}}
	c := New(fake, "gpt-5.6-luna", "system prompt text")

	if _, _, err := c.Classify(context.Background(), "hi"); err == nil {
		t.Fatal("expected an error for a non-JSON reply, got nil")
	}
}

func TestLoadSystemPrompt(t *testing.T) {
	prompt, err := LoadSystemPrompt("../prompts/classifier_system_prompt.md")
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
