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
	"regexp"
	"strings"

	"neochat/attachment"
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
		for _, p := range m.Parts {
			total += EstimateAttachment(p)
		}
	}
	return total
}

// Attachment estimates. Vendors bill images by resolution after their own
// downscaling, which tops out around 1,600 tokens for Claude and a little
// less for GPT; documents by extracted text plus, for PDFs, a rendered
// image of every page -- roughly 1,500-3,000 tokens a page.
const (
	tokensPerImage  = 1600
	tokensPerPage   = 2500
	bytesPerPDFPage = 50 << 10 // fallback when pages can't be counted

	// Gemini reads a video as a frame a second (258 tokens) plus its sound
	// (32 tokens a second). Without a duration in the file, it is taken
	// from the size at a low bitrate, which overestimates.
	tokensPerVideoSecond = 300
	videoBytesPerSecond  = 64 << 10
)

var pdfPageMarker = regexp.MustCompile(`/Type\s*/Page[^s]`)

// EstimateAttachment returns the extra tokens an image or document part
// costs beyond its text. Text parts return 0: their text is already part
// of Message.Content, which EstimateMessages counts.
func EstimateAttachment(p provider.Part) int {
	switch p.Type {
	case provider.PartImage:
		return tokensPerImage
	case provider.PartFile:
		if p.MIME == "application/pdf" {
			pages := len(pdfPageMarker.FindAllIndex(p.Data, -1))
			// Compressed object streams hide page objects; fall back to size.
			if bySize := len(p.Data) / bytesPerPDFPage; pages == 0 || bySize > pages*4 {
				pages = max(pages, bySize, 1)
			}
			return pages * tokensPerPage
		}
		// DOCX is zipped XML: compressed size is a fair proxy for text.
		return max(len(p.Data)/4, 500)
	case provider.PartVideo:
		seconds, ok := attachment.VideoDuration(p.Data)
		if !ok {
			seconds = float64(len(p.Data)) / videoBytesPerSecond
		}
		return int(math.Ceil(seconds))*tokensPerVideoSecond + 1000
	}
	return 0
}
