package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"neochat/conversation"
)

func TestFollowUpReceivesSourceFactsInSavedAndPrivateChat(t *testing.T) {
	steps := []conversation.ToolActivity{
		{Tool: "web_search", Query: "annual report", Results: []conversation.WebLink{{Title: "Report", URL: "https://example.com/report", Snippet: "The verified total is 12345."}}},
		{Tool: "web_fetch", URL: "https://example.com/report", Title: "Report", Excerpt: "Published 2026-11-27; the denominator is 67890."},
	}
	for _, private := range []bool{false, true} {
		t.Run(fmt.Sprint(private), func(t *testing.T) {
			s, standIn := newTitleServer(t, "")
			s.TitleModelID = ""
			req := chatRequest{UserID: "u1", PlanID: "pro", Message: "What were the exact figures?", RequestedMode: "manual", ManualModelID: "deepseek-text", providerKey: "pza", Incognito: private}
			if private {
				req.IncognitoHistory = []incognitoMessage{{Role: "user", Content: "Read the report"}, {Role: "assistant", Content: "I found the report.", Activity: steps}}
			} else {
				req.ConversationID = "source-history"
				for _, m := range []conversation.Message{{Role: conversation.RoleUser, Content: "Read the report"}, {Role: conversation.RoleAssistant, Content: "I found the report.", Versions: []conversation.ResponseVersion{{Content: "I found the report.", Activity: steps}}}} {
					must(t, s.Conversations.Append(context.Background(), req.UserID, req.ConversationID, m))
				}
			}
			if _, err := collectStream(s, context.Background(), req); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(standIn.chat.lastBody(t))
			if err != nil {
				t.Fatal(err)
			}
			for _, fact := range []string{"12345", "67890", "2026-11-27", "https://example.com/report", "partial text"} {
				if !strings.Contains(string(data), fact) {
					t.Fatalf("follow-up lost source evidence %s", fact)
				}
			}
		})
	}
}

func TestResearchHistoryBudgetPrioritizesNewestPageObservations(t *testing.T) {
	var steps []conversation.ToolActivity
	for i := 0; i < 40; i++ {
		steps = append(steps, conversation.ToolActivity{Tool: "web_fetch", URL: fmt.Sprintf("https://example.com/%d", i), Excerpt: fmt.Sprintf("Evidence %02d ", i) + strings.Repeat("x", 250)})
	}
	budget := historyWebEvidenceRunes
	note := historyToolStepsWithEvidence(steps, &budget)
	if budget < 0 || !strings.Contains(note, "page excerpt: Evidence 39") || strings.Contains(note, "page excerpt: Evidence 00") {
		t.Fatal("source budget does not favor recent actual observations")
	}
	bounded := boundActivity(steps)
	if len(bounded) != 16 || bounded[0].URL != "https://example.com/24" || bounded[15].Excerpt == "" {
		t.Fatal("private chat kept the earliest steps and lost the latest page reads")
	}
	// All URLs remain present as a route back to the cached originals, even
	// when that output's excerpt did not fit into the shared context budget.
	if !strings.Contains(note, "https://example.com/0") {
		t.Fatal("lost source address")
	}
}

func TestConversationSummaryReceivesSourceEvidenceWithoutMutatingMessages(t *testing.T) {
	original := []conversation.Message{{Role: conversation.RoleAssistant, Content: "I read it.", Versions: []conversation.ResponseVersion{{Content: "I read it.", Activity: []conversation.ToolActivity{{Tool: "web_search", Query: "report", Results: []conversation.WebLink{{URL: "https://example.com/report", Snippet: "Exact fact: 12345"}}}}}}}}
	input := summaryInput(original)
	if !strings.Contains(input[0].Content, "12345") || !strings.Contains(input[0].Content, "https://example.com/report") {
		t.Fatal("conversation summarization would erase the source facts")
	}
	if original[0].Content != "I read it." {
		t.Fatal("mutated saved assistant message")
	}
}
