package server

import (
	"neochat/conversation"
	"strings"
	"testing"
)

func TestPoeHistoricalThinkingMovesIntoVersion(t *testing.T) {
	raw := "*Thinking...*\n\n> Private thought.\n\nVisible answer."
	original := []conversation.ResponseVersion{{Content: raw}}
	messages := []conversation.Message{{Role: conversation.RoleAssistant, Content: raw, Versions: original}, {Role: conversation.RoleUser, Content: raw}}
	cleanPoeThinkingHistory(messages)
	if strings.Contains(messages[0].Content, "Private") || !strings.Contains(messages[0].Versions[0].Reasoning, "Private thought.") || messages[1].Content != raw {
		t.Fatalf("messages %+v", messages)
	}
	if original[0].Content != raw {
		t.Fatal("mutated stored version")
	}
	cleanPoeThinkingHistory(messages)
	if strings.Count(messages[0].Versions[0].Reasoning, "Private thought.") != 1 {
		t.Fatal("duplicated reasoning")
	}
}
