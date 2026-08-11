package tokenizer

import (
	"strings"
	"testing"

	"neochat/provider"
)

func TestEstimateText_Empty(t *testing.T) {
	if got := EstimateText(""); got != 0 {
		t.Errorf("EstimateText(\"\") = %d, want 0", got)
	}
}

func TestEstimateText_ScalesWithLength(t *testing.T) {
	short := EstimateText("hello there")
	long := EstimateText(strings.Repeat("hello there ", 100))
	if long <= short*50 {
		t.Errorf("EstimateText of 100x-longer text = %d, want it to scale roughly with length (short=%d)", long, short)
	}
}

// TestEstimateText_OverestimatesTokenDenseText checks the deliberate bias:
// short, punctuation-heavy "words" (a token-dense worst case for the
// chars-per-token heuristic alone) still get counted at least one token
// per word, not undercounted by the chars/4 average.
func TestEstimateText_OverestimatesTokenDenseText(t *testing.T) {
	text := "a b c d e f g h i j" // 10 single-char words, 19 chars total
	got := EstimateText(text)
	if got < 10 {
		t.Errorf("EstimateText(%q) = %d, want >= 10 (one token per word, not undercounted by the chars/4 average)", text, got)
	}
}

func TestEstimateMessages_SumsContentPlusPerMessageOverhead(t *testing.T) {
	messages := []provider.Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hi"},
	}
	want := EstimateText("you are helpful") + perMessageOverhead + EstimateText("hi") + perMessageOverhead
	if got := EstimateMessages(messages); got != want {
		t.Errorf("EstimateMessages() = %d, want %d", got, want)
	}
}

func TestEstimateMessages_Empty(t *testing.T) {
	if got := EstimateMessages(nil); got != 0 {
		t.Errorf("EstimateMessages(nil) = %d, want 0", got)
	}
}

// TestEstimateMessages_GrowsWithHistory is the actual bug this package
// closes: a longer conversation must produce a larger estimate, unlike a
// client-supplied estimated_context_tokens that never changes turn to
// turn (README "Context window mismatch").
func TestEstimateMessages_GrowsWithHistory(t *testing.T) {
	turn1 := []provider.Message{{Role: "user", Content: "what is recursion?"}}
	turn5 := []provider.Message{
		{Role: "user", Content: "what is recursion?"},
		{Role: "assistant", Content: "a function that calls itself, with a base case to stop"},
		{Role: "user", Content: "give an example"},
		{Role: "assistant", Content: "factorial(n) = n * factorial(n-1), with factorial(0) = 1 as the base case"},
		{Role: "user", Content: "what about mutual recursion?"},
	}
	if EstimateMessages(turn5) <= EstimateMessages(turn1) {
		t.Errorf("a 5-turn conversation's estimate (%d) should exceed a 1-turn one's (%d)", EstimateMessages(turn5), EstimateMessages(turn1))
	}
}
