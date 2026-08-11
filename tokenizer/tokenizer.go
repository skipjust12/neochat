// Package tokenizer estimates how many tokens a piece of text, or a full
// chat message list, will cost a model to process. It exists to replace
// trusting chatRequest.EstimatedContextTokens -- a client-supplied,
// unverifiable number -- with a value computed server-side from what's
// actually being sent, closing two gaps documented in README.md:
//
//   - "Context window mismatch": once a conversation has real history, the
//     router's context-window hard filter needs to know the true size of
//     what's about to be sent, not a number the client made up on turn 1
//     and never updated.
//   - "Cost-based abuse": estimated cost must come from real input size
//     before a request goes out, not a number the client can freely
//     understate to slip under a spend cap.
package tokenizer

import (
	"math"
	"strings"

	"neochat/provider"
)

// charsPerToken is the rough average English-text ratio widely cited for
// this kind of approximation (~4 characters/token). Used alone it
// undercounts token-dense text -- short words, heavy punctuation/
// whitespace, non-English scripts, code -- so EstimateText combines it
// with a word-count-based estimate and takes the larger of the two. This
// package intentionally biases toward overestimating: it feeds a hard
// context-window filter and a cost pre-estimate, and undercounting either
// is exactly the failure mode it exists to prevent.
const charsPerToken = 4.0

// tokensPerWord approximates tokens-per-word for common BPE vocabularies,
// which average under 1 token per word for English (~0.75). Estimating
// 1 token/word therefore already overcounts relative to a real tokenizer
// on typical English text -- the same deliberate-overestimate bias as
// charsPerToken, from the other direction.
const tokensPerWord = 1.0

// perMessageOverhead approximates the fixed per-message framing cost a
// chat completion format adds beyond the raw text -- role/name delimiters
// and message boundaries the underlying wire format needs, independent of
// content length. ~4 tokens/message is the commonly cited ballpark for
// this framing overhead. Applied per message in EstimateMessages; a bare
// string passed to EstimateText has no message framing of its own, so it
// doesn't get this added.
const perMessageOverhead = 4

// EstimateText returns an approximate token count for a single piece of
// text. Not a real BPE tokenizer -- the model catalog spans multiple
// vendors (Anthropic, OpenAI, Google, Moonshot, DeepSeek), each with its
// own vocabulary, so no single exact tokenizer would even be correct for
// every model in it. See the package doc for why this leans toward
// overestimating rather than trying to be exact.
func EstimateText(text string) int {
	if text == "" {
		return 0
	}
	byChars := int(math.Ceil(float64(len([]rune(text))) / charsPerToken))
	byWords := int(math.Ceil(float64(len(strings.Fields(text))) * tokensPerWord))
	if byWords > byChars {
		return byWords
	}
	return byChars
}

// EstimateMessages returns an approximate total token count for a full
// chat message list -- system prompt, conversation history, and the new
// user message all included, matching exactly what server.prepare sends
// to provider.Client.Generate/GenerateStream. This is the value that
// replaces trusting chatRequest.EstimatedContextTokens.
func EstimateMessages(messages []provider.Message) int {
	total := 0
	for _, m := range messages {
		total += EstimateText(m.Content) + perMessageOverhead
	}
	return total
}
