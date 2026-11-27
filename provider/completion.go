package provider

import "strings"

// Truncated reports a vendor-declared incomplete generation. Transport-level
// SSE "done" means the stream ended; it does not mean all content was produced.
func (r GenerateResult) Truncated() bool {
	switch strings.ToLower(strings.TrimSpace(r.FinishReason)) {
	case "length", "max_tokens", "max_output_tokens", "incomplete":
		return true
	default:
		return false
	}
}
