package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"neochat/conversation"
	"neochat/provider"
)

// ask_user lets a model put a short questionnaire to the user instead of
// guessing. The client shows it as a panel (above the composer, or in its
// place on phones); calling it ends the model's turn, and the answers come
// back as the user's next message, one "Q: …" / "A: …" pair per question.
// Every question also takes an answer in the user's own words (the panel's
// last row), and questions can allow several options. The guidance on
// when and how to ask lives in the core prompt (prompts/*.md).

// Questionnaire limits. Text beyond these is cut; counts beyond them are
// sent back to the model as an error so it can trim the questionnaire.
const (
	maxQuestions          = 6
	maxQuestionOptions    = 6
	maxQuestionnaireRunes = 80
	maxFooterRunes        = 300
	maxQuestionRunes      = 300
	maxOptionLabelRunes   = 100
	maxOptionDescRunes    = 200
)

var askUserTool = provider.FunctionTool{
	Name: "ask_user",
	Description: "Show the user a short questionnaire in a panel and end your turn; their answers arrive as the next user message, one \"Q: question\" / \"A: answer\" pair per question. " +
		"Use it when a few choices from the user would change the result a lot and guessing would waste their time. " +
		"Every question automatically ends with a field for the user's own answer, so never add an \"Other\" or \"Custom\" option.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":  map[string]any{"type": "string", "description": "Heading of the questionnaire, 2-6 words naming the topic."},
			"footer": map[string]any{"type": "string", "description": "One short sentence shown under the questions: why you ask or what you'll do with the answers."},
			"questions": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": maxQuestions,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question":     map[string]any{"type": "string", "description": "The question, short and concrete."},
						"description":  map[string]any{"type": "string", "description": "Optional hint under the question: context or what the choice affects."},
						"multi_select": map[string]any{"type": "boolean", "description": "true when several options can apply at once; the user may pick any number."},
						"options": map[string]any{
							"type":        "array",
							"maxItems":    maxQuestionOptions,
							"description": "2-6 distinct options, the one you'd recommend first. Leave empty when the answer can't be listed (a name, a link, a description): the user then just writes it.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"label":       map[string]any{"type": "string", "description": "The option, a few words."},
									"description": map[string]any{"type": "string", "description": "Optional: what it means or its trade-off, in one short line."},
								},
								"required": []string{"label"},
							},
						},
					},
					"required": []string{"question"},
				},
			},
		},
		"required": []string{"title", "questions"},
	},
}

// customOptionLabels are "other"-style options a model may add despite the
// instructions; the panel's own field already covers them.
var customOptionLabels = map[string]bool{
	"other": true, "other…": true, "other...": true, "custom": true, "custom answer": true, "something else": true,
	"другое": true, "другое…": true, "другое...": true, "свой вариант": true, "свой ответ": true, "иное": true, "другой вариант": true,
}

// parseQuestionnaire reads and tidies ask_user's arguments. A returned
// error is meant for the model (it gets it as the tool's result).
func parseQuestionnaire(arguments string) (*conversation.Questionnaire, error) {
	var args struct {
		Title     string `json:"title"`
		Footer    string `json:"footer"`
		Questions []struct {
			Question    string `json:"question"`
			Description string `json:"description"`
			MultiSelect bool   `json:"multi_select"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, fmt.Errorf("the arguments are not valid JSON")
	}
	if len(args.Questions) == 0 {
		return nil, fmt.Errorf("add at least one question")
	}
	if len(args.Questions) > maxQuestions {
		return nil, fmt.Errorf("ask at most %d questions; keep the ones that matter most", maxQuestions)
	}
	q := &conversation.Questionnaire{
		Title:  oneLine(args.Title, maxQuestionnaireRunes),
		Footer: oneLine(args.Footer, maxFooterRunes),
	}
	if q.Title == "" {
		q.Title = "A few questions"
	}
	for i, in := range args.Questions {
		text := oneLine(in.Question, maxQuestionRunes)
		if text == "" {
			return nil, fmt.Errorf("question %d is empty", i+1)
		}
		question := conversation.Question{
			ID:          fmt.Sprintf("q%d", i+1),
			Text:        text,
			Description: oneLine(in.Description, maxQuestionRunes),
			MultiSelect: in.MultiSelect,
		}
		seen := map[string]bool{}
		for _, opt := range in.Options {
			label := oneLine(opt.Label, maxOptionLabelRunes)
			key := strings.ToLower(strings.Trim(label, " .:"))
			if label == "" || seen[key] || customOptionLabels[key] {
				continue
			}
			seen[key] = true
			question.Options = append(question.Options, conversation.QuestionOption{
				ID:          fmt.Sprintf("q%do%d", i+1, len(question.Options)+1),
				Label:       label,
				Description: oneLine(opt.Description, maxOptionDescRunes),
			})
		}
		switch {
		case len(question.Options) > maxQuestionOptions:
			return nil, fmt.Errorf("question %d has %d options; give at most %d (the user can always write their own)", i+1, len(question.Options), maxQuestionOptions)
		case len(question.Options) == 1:
			return nil, fmt.Errorf("question %d has one option; give 2-%d, or none to let the user just write the answer", i+1, maxQuestionOptions)
		}
		if len(question.Options) == 0 {
			question.MultiSelect = false
		}
		q.Questions = append(q.Questions, question)
	}
	return q, nil
}

// boundQuestionnaire applies the same limits to a questionnaire that comes
// back from the client (incognito history), so the model is never sent
// more than parseQuestionnaire would have let through.
func boundQuestionnaire(q *conversation.Questionnaire) *conversation.Questionnaire {
	if q == nil || len(q.Questions) == 0 {
		return nil
	}
	out := &conversation.Questionnaire{Title: oneLine(q.Title, maxQuestionnaireRunes), Footer: oneLine(q.Footer, maxFooterRunes)}
	for i, in := range q.Questions {
		if i == maxQuestions {
			break
		}
		question := conversation.Question{ID: in.ID, Text: oneLine(in.Text, maxQuestionRunes), Description: oneLine(in.Description, maxQuestionRunes), MultiSelect: in.MultiSelect}
		for j, opt := range in.Options {
			if j == maxQuestionOptions {
				break
			}
			question.Options = append(question.Options, conversation.QuestionOption{ID: opt.ID, Label: oneLine(opt.Label, maxOptionLabelRunes), Description: oneLine(opt.Description, maxOptionDescRunes)})
		}
		out.Questions = append(out.Questions, question)
	}
	return out
}

// questionnaireText is how an asked questionnaire appears in the history
// sent to the model, after the text of the answer that asked it -- so in
// later turns it knows what it asked, and the user's "Q:/A:" message reads
// as the reply to it.
func questionnaireText(q *conversation.Questionnaire) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[You asked the user with a questionnaire, %q:\n", q.Title)
	for i, question := range q.Questions {
		fmt.Fprintf(&b, "%d. %s", i+1, question.Text)
		if len(question.Options) == 0 {
			b.WriteString(" (free answer)")
		} else {
			labels := make([]string, len(question.Options))
			for j, opt := range question.Options {
				labels[j] = opt.Label
			}
			kind := "one choice"
			if question.MultiSelect {
				kind = "any number of choices"
			}
			fmt.Fprintf(&b, " (%s: %s; or their own answer)", kind, strings.Join(labels, "; "))
		}
		b.WriteString("\n")
	}
	b.WriteString("Their answers come in the next message.]")
	return b.String()
}

// assistantHistoryContent is an assistant message as the model sees it in
// history: its text, plus the questionnaire its latest version asked.
func assistantHistoryContent(m conversation.Message) string {
	if len(m.Versions) == 0 || m.Versions[len(m.Versions)-1].Questionnaire == nil {
		return m.Content
	}
	return strings.TrimSpace(m.Content + "\n\n" + questionnaireText(m.Versions[len(m.Versions)-1].Questionnaire))
}

func oneLine(s string, n int) string {
	return clipRunes(strings.Join(strings.Fields(s), " "), n)
}
