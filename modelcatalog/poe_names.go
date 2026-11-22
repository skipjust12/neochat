package modelcatalog

import "strings"

// Poe's bot handle is an API identifier; the picker uses a separate label.
func poeBotHandle(id, name string) string {
	if name == "" {
		return id
	}
	name = strings.NewReplacer("(", "", ")", "").Replace(name)
	return strings.Join(strings.Fields(name), "-")
}
func poeDisplayName(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	if len(parts) == 0 {
		return name
	}
	if strings.EqualFold(parts[0], "gpt") && len(parts) > 1 {
		parts = append([]string{"GPT-" + parts[1]}, parts[2:]...)
	}
	return strings.Join(parts, " ")
}
