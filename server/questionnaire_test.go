package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"neochat/conversation"
)

const askArgs = `{"title":"Лендинг для кофейни","footer":"Ответы помогут сразу попасть в стиль.","questions":[
 {"question":"Какой стиль?","description":"Влияет на шрифты и цвета","options":[{"label":"Минимализм","description":"Много воздуха"},{"label":"Уютный"},{"label":"Другое"}]},
 {"question":"Какие разделы нужны?","multi_select":true,"options":[{"label":"Меню"},{"label":"Доставка"},{"label":"Контакты"},{"label":"меню"}]},
 {"question":"Как называется кофейня?","multi_select":true}
]}`

func TestParseQuestionnaire(t *testing.T) {
	q, err := parseQuestionnaire(askArgs)
	if err != nil {
		t.Fatal(err)
	}
	if q.Title != "Лендинг для кофейни" || q.Footer != "Ответы помогут сразу попасть в стиль." || len(q.Questions) != 3 {
		t.Fatalf("questionnaire = %+v", q)
	}
	style, sections, name := q.Questions[0], q.Questions[1], q.Questions[2]
	if style.ID != "q1" || style.Description != "Влияет на шрифты и цвета" || len(style.Options) != 2 || style.Options[0].ID != "q1o1" || style.Options[0].Description != "Много воздуха" {
		t.Errorf("style = %+v (the model's own Other option must go: the panel always has a custom field)", style)
	}
	if !sections.MultiSelect || len(sections.Options) != 3 {
		t.Errorf("sections = %+v (multi-select kept, duplicate dropped)", sections)
	}
	if len(name.Options) != 0 || name.MultiSelect {
		t.Errorf("free-text question = %+v", name)
	}

	for args, want := range map[string]string{
		`not json`:         "not valid JSON",
		`{"questions":[]}`: "at least one question",
		`{"questions":[{"question":"a","options":[{"label":"only"}]}]}`: "has one option",
		`{"questions":[{"question":""}]}`:                               "is empty",
		`{"questions":[{"question":"1"},{"question":"2"},{"question":"3"},{"question":"4"},{"question":"5"},{"question":"6"},{"question":"7"}]}`:         "at most 6",
		`{"questions":[{"question":"x","options":[{"label":"a"},{"label":"b"},{"label":"c"},{"label":"d"},{"label":"e"},{"label":"f"},{"label":"g"}]}]}`: "at most 6",
	} {
		if _, err := parseQuestionnaire(args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
	if q, _ := parseQuestionnaire(`{"questions":[{"question":"x"}]}`); q.Title != "A few questions" {
		t.Errorf("untitled questionnaire got %q", q.Title)
	}
}

func TestAskUserEndsTheTurnWithAQuestionnaire(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if len(toolMessages(body)) > 0 {
			t.Error("the turn went on after ask_user")
		}
		if hasMessagePrefix(body, "user", "Q: ") {
			return 200, []string{textChunk("Отлично, делаю минималистичный лендинг."), usageChunk}
		}
		return 200, []string{
			textChunk("Пара вопросов перед стартом."),
			toolCallChunk([3]string{"a1", "ask_user", askArgs}, [3]string{"s1", "web_search", `{"query":"кофейни лендинги"}`}),
			usageChunk,
		}
	}}
	s := newWebServer(t, vendor)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "сделай лендинг для кофейни", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "auto"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	if done.ResponseText != "Пара вопросов перед стартом." || done.Questionnaire == nil || len(done.Questionnaire.Questions) != 3 {
		t.Fatalf("done = %q, questionnaire %+v", done.ResponseText, done.Questionnaire)
	}
	if len(done.Activity) != 0 || len(vendor.calls()) != 1 {
		t.Fatalf("the search in the same step ran: activity %+v, calls %d", done.Activity, len(vendor.calls()))
	}
	history, _ := s.Conversations.History(context.Background(), "u1", done.ConversationID, 0)
	if q := history[len(history)-1].Versions[0].Questionnaire; q == nil || q.Title != "Лендинг для кофейни" {
		t.Fatalf("stored version questionnaire = %+v", q)
	}

	// The answers come back as the user's next message; the model sees what
	// it asked right before them.
	answers := "Q: Какой стиль?\nA: Минимализм\n\nQ: Какие разделы нужны?\nA: Меню, Контакты\n\nQ: Как называется кофейня?\nA: Зерно"
	req.Message, req.ConversationID = answers, done.ConversationID
	events, err = collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := events["done"][0].(chatResponse); got.Questionnaire != nil || got.ResponseText != "Отлично, делаю минималистичный лендинг." {
		t.Fatalf("follow-up = %+v", got)
	}
	calls := vendor.calls()
	last := calls[len(calls)-1]["messages"].([]any)
	asked := last[len(last)-2].(map[string]any)["content"].(string)
	if !strings.HasPrefix(asked, "Пара вопросов перед стартом.\n\n[You asked the user with a questionnaire, \"Лендинг для кофейни\":") ||
		!strings.Contains(asked, "2. Какие разделы нужны? (any number of choices: Меню; Доставка; Контакты; or their own answer)") ||
		!strings.Contains(asked, "3. Как называется кофейня? (free answer)") {
		t.Errorf("the model's view of its question turn = %q", asked)
	}
	if user := last[len(last)-1].(map[string]any); user["role"] != "user" || user["content"] != answers {
		t.Errorf("answers message = %v", user)
	}
}

func TestAskUserIsOfferedWithoutWebAndFixedWhenInvalid(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if results := toolMessages(body); len(results) == 1 {
			if text := results[0]["content"].(string); !strings.Contains(text, "has one option") {
				t.Errorf("tool error = %q", text)
			}
			return 200, []string{toolCallChunk([3]string{"a2", "ask_user", `{"title":"T","questions":[{"question":"Имя?"}]}`}), usageChunk}
		}
		return 200, []string{toolCallChunk([3]string{"a1", "ask_user", `{"title":"T","questions":[{"question":"x","options":[{"label":"only"}]}]}`}), usageChunk}
	}}
	s := newWebServer(t, vendor)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "off"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	done := events["done"][0].(chatResponse)
	if done.Questionnaire == nil || done.Questionnaire.Questions[0].Text != "Имя?" || done.ResponseText != "" {
		t.Fatalf("done = %q %+v", done.ResponseText, done.Questionnaire)
	}
	if names := toolNames(vendor.calls()[0]); len(names) != 1 || names[0] != "ask_user" {
		t.Errorf("tools with web off = %v", names)
	}
}

func TestAskUserRefusedRouteAnswersPlainly(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		if body["tools"] != nil {
			return http.StatusBadRequest, []string{`{"error":{"message":"tools are not supported"}}`}
		}
		return 200, []string{textChunk("plain"), usageChunk}
	}}
	s := newWebServer(t, vendor)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", WebSearch: "off"}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if done := events["done"][0].(chatResponse); done.ResponseText != "plain" || len(done.Activity) != 0 {
		t.Fatalf("done = %q %+v (web off: no refusal step)", done.ResponseText, done.Activity)
	}
}

func TestIncognitoHistoryCarriesTheQuestionnaire(t *testing.T) {
	vendor := &scriptedPolza{reply: func(body map[string]any) (int, []string) {
		return 200, []string{textChunk("ok"), usageChunk}
	}}
	s := newWebServer(t, vendor)
	q, _ := parseQuestionnaire(askArgs)
	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "Q: Какой стиль?\nA: Уютный", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", Incognito: true,
		IncognitoHistory: []incognitoMessage{{Role: "user", Content: "лендинг"}, {Role: "assistant", Content: "Вопросы:", Questionnaire: q}}}
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	messages := vendor.calls()[0]["messages"].([]any)
	if asked := messages[len(messages)-2].(map[string]any)["content"].(string); !strings.Contains(asked, "1. Какой стиль? (one choice: Минимализм; Уютный; or their own answer)") {
		t.Fatalf("incognito assistant turn = %q", asked)
	}
}

func hasMessagePrefix(body map[string]any, role, prefix string) bool {
	messages := body["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	text, _ := last["content"].(string)
	return last["role"] == role && strings.HasPrefix(text, prefix)
}

var _ = conversation.Questionnaire{}
