package router

import "time"

// ComputeCostUSD prices a token count against a model's per-Mtok rates.
// Pure pricing math, shared by both the pre-flight estimate that Route
// attaches to RouteResult and the actual post-generation cost recorded via
// NewCostLogEntry -- the two must agree on the formula, since abuse-defense
// estimates and billed cost are compared against each other downstream.
func ComputeCostUSD(model Model, inputTokens, outputTokens int) float64 {
	return ComputeCostUSDRates(model.CostInputPerMTok, model.CostOutputPerMTok, inputTokens, outputTokens)
}

// ComputeCostUSDRates is ComputeCostUSD's formula against raw per-Mtok
// rates instead of a catalog Model -- for pricing a call that has no
// catalog entry at all, e.g. classifier.Classifier/moderation.Moderator's
// cost logging: their model is picked by CLASSIFIER_API_MODEL_ID/
// MODERATION_API_MODEL_ID, an arbitrary OpenRouter slug that (see
// moderation's default, gpt-oss-120b) may not be a chat-generation
// catalog entry at all.
func ComputeCostUSDRates(costInputPerMTok, costOutputPerMTok float64, inputTokens, outputTokens int) float64 {
	return float64(inputTokens)/1_000_000*costInputPerMTok +
		float64(outputTokens)/1_000_000*costOutputPerMTok
}

// CostLogEntry is a single per-request cost record, meant to be persisted
// verbatim (e.g. one row per API call) once the actual token usage for a
// completed generation is known. The router package only builds the value;
// writing it to a store is the caller's job -- see the "per-request cost
// accounting" item in the pre-launch checklist in README.md.
type CostLogEntry struct {
	UserID       string    `json:"user_id"`
	RequestID    string    `json:"request_id"`
	ModelID      string    `json:"model_id"`
	Mode         string    `json:"mode"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	CostUSD      float64   `json:"cost_usd"`
	Timestamp    time.Time `json:"timestamp"`
}

// NewCostLogEntry builds a CostLogEntry from a model and the actual token
// usage a completed generation reported. Call this after the upstream
// provider call returns -- RouteResult.EstimatedCostUSD is a pre-flight
// guess for abuse defense, not a substitute for billing off real usage,
// since actual output length is never known until generation finishes.
func NewCostLogEntry(userID, requestID string, model Model, mode string, inputTokens, outputTokens int, timestamp time.Time) CostLogEntry {
	return CostLogEntry{
		UserID:       userID,
		RequestID:    requestID,
		ModelID:      model.ID,
		Mode:         mode,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CostUSD:      ComputeCostUSD(model, inputTokens, outputTokens),
		Timestamp:    timestamp,
	}
}
