package server

import (
	"context"
	"strings"
	"testing"

	"neochat/provider"
	"neochat/router"
)

// withoutIdentityNote is what a call sent apart from identityNote, for tests
// about the rest of the messages.
func withoutIdentityNote(messages []provider.Message) []provider.Message {
	var out []provider.Message
	for _, m := range messages {
		if m.Role == "system" && strings.Contains(m.Content, "one of the AI models available in NeoChat") {
			continue
		}
		out = append(out, m)
	}
	return out
}

func TestIdentityNote(t *testing.T) {
	note := identityNote(router.Model{ID: "m1", DisplayName: "Model One"})
	for _, want := range []string{"You are Model One,", "NeoChat", "through an API"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q: %s", want, note)
		}
	}
	if note := identityNote(router.Model{ID: "m1"}); !strings.HasPrefix(note, "You are m1,") {
		t.Errorf("no display name: %s", note)
	}
}

func TestAnswerIsToldWhichModelItIs(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		return 200, []string{textChunk("Hi."), usageChunk}
	}}
	s := newWebServer(t, vendor)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "which model are you?", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user"}
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if main := vendor.mainCalls(); !hasSystemPrefix(main[0], "You are ") {
		t.Fatal("the model wasn't told which model it is")
	}
}
