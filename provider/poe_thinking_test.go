package provider

import (
	"strings"
	"testing"
)

func TestPoeThinkingEnvelopeSplitAtEveryByte(t *testing.T) {
	source := "*Thinking...*\n\n> First thought.\n> Second thought.\n\nПроверю данные.\n*Thinking...*\n\n> Another thought.\n\nГотовый ответ."
	for size := 1; size <= len(source); size++ {
		f := poeThinkingFilter{}
		var answer, reasoning strings.Builder
		for at := 0; at < len(source); at += size {
			a, r := f.push(source[at:min(len(source), at+size)], false)
			answer.WriteString(a)
			reasoning.WriteString(r)
		}
		a, r := f.push("", true)
		answer.WriteString(a)
		reasoning.WriteString(r)
		if strings.Contains(answer.String(), "Thinking") || strings.Contains(answer.String(), "thought") || !strings.Contains(answer.String(), "Проверю данные.") || !strings.Contains(answer.String(), "Готовый ответ.") {
			t.Fatalf("size %d answer %q", size, answer.String())
		}
		if !strings.Contains(reasoning.String(), "Another thought.") {
			t.Fatalf("size %d reasoning %q", size, reasoning.String())
		}
	}
}
func TestPoeThinkingPreservesOrdinaryMarkdown(t *testing.T) {
	for _, source := range []string{"> Normal quote\n\nAnswer", "*Thinking...*\n\nThis is ordinary prose.", "```markdown\n*Thinking...*\n\n> Example\n```\nAnswer", "Thinking about this normally."} {
		f := poeThinkingFilter{}
		answer, reasoning := "", ""
		for _, r := range source {
			a, b := f.push(string(r), false)
			answer += a
			reasoning += b
		}
		a, b := f.push("", true)
		answer += a
		reasoning += b
		if answer != source || reasoning != "" {
			t.Fatalf("source %q answer %q reasoning %q", source, answer, reasoning)
		}
	}
}
