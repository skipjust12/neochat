package server

import (
	"regexp"
	"strings"
)

// Quotes: the user selects words in an answer and asks about them ("Ask"
// in the UI). The selection travels as chatRequest.Quote, is stored with
// the user's message (conversation.Message.Quote), and reaches the model
// as a Markdown blockquote in front of the question -- the form every
// model reads as "this part, specifically".

// maxQuoteRunes caps a quote; the client cuts selections to the same
// length, so this only trims what a hand-made request sends.
const maxQuoteRunes = 4000

var blankLineRuns = regexp.MustCompile(`\n{3,}`)

// cleanQuote tidies a quote from the client: line endings normalized,
// trailing spaces and runs of blank lines dropped, trimmed and capped.
func cleanQuote(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t ")
	}
	s = blankLineRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return clipRunes(strings.TrimSpace(s), maxQuoteRunes)
}

// withQuote is a user message's text as the model sees it: the quote as a
// blockquote, then what the user wrote about it.
func withQuote(quote, text string) string {
	if quote == "" {
		return text
	}
	lines := strings.Split(quote, "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + line
		}
	}
	quoted := strings.Join(lines, "\n")
	if strings.TrimSpace(text) == "" {
		return quoted
	}
	return quoted + "\n\n" + text
}
