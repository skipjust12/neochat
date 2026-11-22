package server

import (
	"neochat/conversation"
	"neochat/provider"
	"strings"
)

func cleanPoeThinkingHistory(messages []conversation.Message) {
	for i := range messages {
		m := &messages[i]
		if m.Role != conversation.RoleAssistant {
			continue
		}
		answer, thought := provider.SplitPoeThinking(m.Content)
		m.Content = answer
		m.Versions = append([]conversation.ResponseVersion(nil), m.Versions...)
		if len(m.Versions) == 0 && thought != "" {
			m.Versions = []conversation.ResponseVersion{{Content: answer, ModelID: m.ModelID, CreatedAt: m.CreatedAt}}
		}
		for j := range m.Versions {
			v := &m.Versions[j]
			text, reasoning := provider.SplitPoeThinking(v.Content)
			v.Content = text
			if reasoning != "" && !strings.Contains(v.Reasoning, reasoning) {
				v.Reasoning += reasoning
			}
		}
		if thought != "" && len(m.Versions) > 0 {
			v := &m.Versions[len(m.Versions)-1]
			if !strings.Contains(v.Reasoning, thought) {
				v.Reasoning += thought
			}
		}
	}
}
