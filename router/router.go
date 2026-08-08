package router

import (
	"fmt"
	"strings"
)

// Mode tiers, ordered from cheapest/fastest to strongest. Escalation moves
// one step to the right; "max" is the ceiling and cannot escalate further.
var modeTiers = []string{"instant", "thinking", "max"}

// TODO: вынести в weights.json позже.
// Пороговые правила для auto-режима: комбинируем reasoning_depth,
// complexity_score и creativity_level в один числовой "запрос на мощность"
// и режем его порогами ниже.
const (
	autoScoreInstantCeiling  = 1.0 // score < this  -> "instant"
	autoScoreThinkingCeiling = 2.5 // score < this  -> "thinking"; else -> "max"
)

// defaultThreeLevelEnum is the fallback used everywhere a "low"/"moderate"/
// "high" classifier field (reasoning_depth, creativity_level) is looked up
// but the classifier sent something outside that enum. Keeping a single
// named fallback -- instead of letting each lookup site default to its own
// zero value -- is what keeps the auto-mode heuristic and the cost-scoring
// formula in agreement about what an unrecognized value means.
const defaultThreeLevelEnum = "moderate"

// enumScore maps the classifier's enum strings ("low"/"moderate"/"high")
// onto a 0..2 scale for the auto-mode heuristic. Used for both
// reasoning_depth and creativity_level.
func enumScore(v string) float64 {
	switch strings.ToLower(v) {
	case "low":
		return 0
	case "moderate":
		return 1
	case "high":
		return 2
	default:
		return enumScore(defaultThreeLevelEnum)
	}
}

// Router bundles a loaded model catalog and scoring weights. Construct it
// once at startup with NewRouter, then call Route per request.
type Router struct {
	Catalog Catalog
	Weights Weights
}

// NewRouter builds a Router from an already-loaded catalog and weights set.
func NewRouter(catalog Catalog, weights Weights) Router {
	return Router{Catalog: catalog, Weights: weights}
}

// Route selects a model and product mode for a single request, given the
// classifier's analysis of it, the mode the user explicitly requested
// ("auto" | "instant" | "thinking" | "max" | "manual"), the manually chosen
// model ID (only used when requestedMode == "manual"), and an estimate of
// how many tokens of conversation context the request carries.
func (r Router) Route(input ClassifierOutput, requestedMode string, manualModelID string, estimatedContextTokens int) (RouteResult, error) {
	if requestedMode == "manual" {
		model, ok := r.Catalog.FindModel(manualModelID)
		if !ok {
			return RouteResult{}, fmt.Errorf("router: manual_model_id %q not found in catalog", manualModelID)
		}
		return RouteResult{
			SelectedModelID: model.ID,
			SelectedMode:    "manual",
			Reason:          fmt.Sprintf("requested_mode=manual: bypassed scoring, used manual_model_id=%q directly", manualModelID),
		}, nil
	}

	if err := validateInput(input); err != nil {
		return RouteResult{}, err
	}

	commonCandidates, filterLog := r.applyHardFilters(input, estimatedContextTokens)
	if len(commonCandidates) == 0 {
		return RouteResult{}, fmt.Errorf("router: no candidate models survive hard filters (%s)", filterLog)
	}

	baseMode := requestedMode
	autoReason := ""
	if requestedMode == "auto" {
		baseMode, autoReason = r.determineAutoMode(input)
	}

	// confidenceBelowThreshold records the actual comparison against the
	// threshold, independent of whether escalation could take effect. Do
	// not conflate this with "escalated": "max" is already the ceiling
	// tier, so low confidence there triggers the condition but never
	// changes effectiveMode.
	confidenceBelowThreshold := input.Confidence < r.Weights.ConfidenceEscalationThreshold
	effectiveMode := baseMode
	escalated := false
	if confidenceBelowThreshold {
		if escalatedMode, ok := escalateMode(baseMode); ok {
			effectiveMode = escalatedMode
			escalated = true
		}
	}

	modeCandidates := filterByMode(commonCandidates, effectiveMode)
	if len(modeCandidates) == 0 {
		return RouteResult{}, fmt.Errorf(
			"router: no candidate models support mode %q after hard filters (%s)", effectiveMode, filterLog,
		)
	}

	winner, scoreLog := r.scoreAndPick(input, modeCandidates)

	var reason strings.Builder
	fmt.Fprintf(&reason, "hard filters passed: %s. ", filterLog)
	if requestedMode == "auto" {
		fmt.Fprintf(&reason, "auto mode resolved base_mode=%s (%s). ", baseMode, autoReason)
	} else {
		fmt.Fprintf(&reason, "requested_mode=%s used as base_mode. ", requestedMode)
	}
	switch {
	case escalated:
		fmt.Fprintf(&reason, "confidence %.2f < threshold %.2f: escalated %s -> %s. ",
			input.Confidence, r.Weights.ConfidenceEscalationThreshold, baseMode, effectiveMode)
	case confidenceBelowThreshold:
		fmt.Fprintf(&reason, "confidence %.2f < threshold %.2f: escalation triggered but base_mode=%s has no higher tier. ",
			input.Confidence, r.Weights.ConfidenceEscalationThreshold, baseMode)
	default:
		fmt.Fprintf(&reason, "confidence %.2f >= threshold %.2f: no escalation. ",
			input.Confidence, r.Weights.ConfidenceEscalationThreshold)
	}
	fmt.Fprintf(&reason, "scored %d candidate(s) for mode=%s: %s", len(modeCandidates), effectiveMode, scoreLog)

	return RouteResult{
		SelectedModelID: winner.ID,
		SelectedMode:    effectiveMode,
		Reason:          reason.String(),
	}, nil
}

// validateInput rejects classifier output with numeric fields outside their
// documented [0,1] range. This runs only on the scored path (requestedMode
// != "manual"): manual routing never reads these fields, so a malformed
// classifier payload should not be able to block an explicit manual choice.
func validateInput(input ClassifierOutput) error {
	if input.ComplexityScore < 0 || input.ComplexityScore > 1 {
		return fmt.Errorf("router: complexity_score %.4f out of range [0,1]", input.ComplexityScore)
	}
	if input.Confidence < 0 || input.Confidence > 1 {
		return fmt.Errorf("router: confidence %.4f out of range [0,1]", input.Confidence)
	}
	return nil
}

// applyHardFilters removes models that cannot serve the request at all,
// independent of which mode tier is ultimately chosen: insufficient context
// window, missing web-search/code-execution support, or missing modality
// support. It returns the surviving models plus a short human-readable log
// of what was checked, for inclusion in RouteResult.Reason.
func (r Router) applyHardFilters(input ClassifierOutput, estimatedContextTokens int) ([]Model, string) {
	requiredModalities := map[string]bool{}
	for _, m := range input.ModalityInput {
		requiredModalities[m] = true
	}
	for _, m := range input.ModalityOutputExpected {
		requiredModalities[m] = true
	}

	var kept []Model
	for _, m := range r.Catalog.Models {
		if m.ContextWindow < estimatedContextTokens {
			continue
		}
		if input.NeedsWebSearch && !m.SupportsWebSearch {
			continue
		}
		if input.NeedsCodeExecution && !m.SupportsCodeExecution {
			continue
		}
		if !supportsAllModalities(m, requiredModalities) {
			continue
		}
		kept = append(kept, m)
	}

	log := fmt.Sprintf(
		"context>=%d, web_search=%v, code_execution=%v, modalities=%v -> %d/%d models",
		estimatedContextTokens, input.NeedsWebSearch, input.NeedsCodeExecution,
		modalitiesList(requiredModalities), len(kept), len(r.Catalog.Models),
	)
	return kept, log
}

func supportsAllModalities(m Model, required map[string]bool) bool {
	supported := map[string]bool{}
	for _, s := range m.SupportsModality {
		supported[s] = true
	}
	for req := range required {
		if !supported[req] {
			return false
		}
	}
	return true
}

func modalitiesList(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// reasoningDepthWeight looks up the configured cost-scoring weight for a
// reasoning_depth value. An unrecognized value falls back to the weight for
// defaultThreeLevelEnum ("moderate"), matching enumScore's fallback used by
// the auto-mode heuristic -- without this, the same unrecognized input would
// silently score as 0 here (weaker than even "low") while being treated as
// "moderate" for auto-mode tier selection.
func (r Router) reasoningDepthWeight(depth string) float64 {
	if w, ok := r.Weights.ReasoningDepthWeight[depth]; ok {
		return w
	}
	return r.Weights.ReasoningDepthWeight[defaultThreeLevelEnum]
}

func filterByMode(models []Model, mode string) []Model {
	var kept []Model
	for _, m := range models {
		for _, mm := range m.Modes {
			if mm == mode {
				kept = append(kept, m)
				break
			}
		}
	}
	return kept
}

// escalateMode moves a mode one tier up ("instant"->"thinking",
// "thinking"->"max"). "max" has no tier above it, so ok is false.
func escalateMode(mode string) (string, bool) {
	for i, m := range modeTiers {
		if m == mode && i+1 < len(modeTiers) {
			return modeTiers[i+1], true
		}
	}
	return mode, false
}

// determineAutoMode picks a base mode tier for requestedMode=="auto" from a
// simple weighted combination of reasoning_depth, complexity_score and
// creativity_level. See the TODO'd constants above.
func (r Router) determineAutoMode(input ClassifierOutput) (string, string) {
	score := enumScore(input.ReasoningDepth) + 2*input.ComplexityScore + 0.5*enumScore(input.CreativityLevel)

	var mode string
	switch {
	case score < autoScoreInstantCeiling:
		mode = "instant"
	case score < autoScoreThinkingCeiling:
		mode = "thinking"
	default:
		mode = "max"
	}

	reasonDetail := fmt.Sprintf(
		"score=%.2f (reasoning_depth=%s, complexity_score=%.2f, creativity_level=%s)",
		score, input.ReasoningDepth, input.ComplexityScore, input.CreativityLevel,
	)
	return mode, reasonDetail
}

// scoreAndPick applies the weighted scoring formula from weights.json to
// pick a winner among mode-eligible candidates:
//
//	qualityPull    = reasoning_depth_weight[depth] + complexity_weight * complexity_score
//	effectiveBias  = qualityPull - cost_weight
//	score(model)   = effectiveBias * (cost_input_per_mtok + cost_output_per_mtok)
//
// A positive effectiveBias means the request's reasoning/complexity demands
// push toward the strongest (priciest) model within the tier; a negative
// effectiveBias means, all else equal within the tier, prefer the cheapest
// model. Ties are broken by lowest total cost.
func (r Router) scoreAndPick(input ClassifierOutput, candidates []Model) (Model, string) {
	qualityPull := r.reasoningDepthWeight(input.ReasoningDepth) + r.Weights.ComplexityWeight*input.ComplexityScore
	effectiveBias := qualityPull - r.Weights.CostWeight

	best := candidates[0]
	bestScore := effectiveBias * (best.CostInputPerMTok + best.CostOutputPerMTok)

	for _, m := range candidates[1:] {
		totalCost := m.CostInputPerMTok + m.CostOutputPerMTok
		score := effectiveBias * totalCost
		bestTotalCost := best.CostInputPerMTok + best.CostOutputPerMTok
		if score > bestScore || (score == bestScore && totalCost < bestTotalCost) {
			best = m
			bestScore = score
		}
	}

	log := fmt.Sprintf(
		"quality_pull=%.2f, effective_bias=%.2f, winner=%s (cost=%.2f, score=%.2f)",
		qualityPull, effectiveBias, best.ID, best.CostInputPerMTok+best.CostOutputPerMTok, bestScore,
	)
	return best, log
}
