package router

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Mode tiers, ordered from cheapest/fastest to strongest. Escalation moves
// one step to the right; "max" is the ceiling and cannot escalate further.
var modeTiers = []string{"instant", "thinking", "max"}

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
// model ID (only used when requestedMode == "manual"), an estimate of how
// many tokens of conversation context the request carries, and whether
// this user's Thinking+Max spend has hit their plan's 30-day rolling cap
// (see the limits package and docs/unit-economics.md section 6).
// thinkingMaxLocked comes from outside -- Route has no notion of "plans"
// or spend history, only this one boolean.
//
// excludedModelIDs lets the caller keep Route away from models it already
// knows are currently unhealthy -- e.g. server.handle retrying after a
// provider.CircuitOpenError -- without Route itself knowing anything about
// circuit breakers or providers. A nil or empty map excludes nothing. In
// manual mode, an excluded manualModelID is still an error (there is no
// other candidate to fall back to: the user asked for this exact model).
func (r Router) Route(input ClassifierOutput, requestedMode string, manualModelID string, estimatedContextTokens int, thinkingMaxLocked bool, excludedModelIDs map[string]bool) (RouteResult, error) {
	if estimatedContextTokens < 0 {
		return RouteResult{}, fmt.Errorf("router: estimated context tokens %d must be non-negative", estimatedContextTokens)
	}
	if input.EstimatedOutputTokens < 0 {
		return RouteResult{}, fmt.Errorf("router: estimated_output_tokens %d must be non-negative", input.EstimatedOutputTokens)
	}
	if requestedMode != "auto" && requestedMode != "instant" && requestedMode != "thinking" && requestedMode != "max" && requestedMode != "manual" {
		return RouteResult{}, fmt.Errorf("router: requested_mode %q must be auto, instant, thinking, max, or manual", requestedMode)
	}

	if requestedMode == "manual" {
		if excludedModelIDs[manualModelID] {
			return RouteResult{}, fmt.Errorf("router: manual_model_id %q is currently unavailable (circuit open)", manualModelID)
		}
		model, ok := r.Catalog.FindModel(manualModelID)
		if !ok {
			return RouteResult{}, fmt.Errorf("router: manual_model_id %q not found in catalog", manualModelID)
		}
		// Manual bypasses scoring, but not the spend lock -- docs/unit-economics.md
		// section 6.1 calls out manual mode by name as the loophole that would
		// otherwise let a locked-out user keep hitting an expensive model
		// directly. Unlike the scored path below, there is no cheaper
		// candidate to silently fall back to (the user asked for this exact
		// model), so this returns an error instead of substituting a
		// different model the caller didn't ask for.
		if thinkingMaxLocked && !modelSupportsMode(model, "instant") {
			return RouteResult{}, fmt.Errorf(
				"router: manual_model_id %q is thinking/max-tier and thinking_max is locked for this user this billing cycle", manualModelID,
			)
		}
		return RouteResult{
			SelectedModelID:  model.ID,
			SelectedMode:     "manual",
			Reason:           fmt.Sprintf("requested_mode=manual: bypassed scoring, used manual_model_id=%q directly", manualModelID),
			EstimatedCostUSD: ComputeCostUSD(model, estimatedContextTokens, input.EstimatedOutputTokens),
		}, nil
	}

	if err := validateInput(input); err != nil {
		return RouteResult{}, err
	}

	commonCandidates, filterLog := r.applyHardFilters(input, estimatedContextTokens, excludedModelIDs)
	if len(commonCandidates) == 0 {
		return RouteResult{}, fmt.Errorf("router: no candidate models survive hard filters (%s)", filterLog)
	}

	// availableModes are the mode tiers actually reachable given the models
	// that survived the hard filters -- e.g. a request that (via
	// output_format/required_tools/modality) only an image-only,
	// instant-only model can serve has availableModes == {"instant"}, even
	// though the catalog as a whole spans all three tiers.
	availableModes := modeSet(commonCandidates)

	baseMode := requestedMode
	autoReason := ""
	if requestedMode == "auto" {
		baseMode, autoReason = r.determineAutoMode(input, availableModes)
	}

	// confidenceBelowThreshold records the actual comparison against the
	// threshold, independent of whether escalation could take effect. Do
	// not conflate this with "escalated": "max" is already the ceiling
	// tier, so low confidence there triggers the condition but never
	// changes effectiveMode.
	confidenceBelowThreshold := input.Confidence < r.Weights.ConfidenceEscalationThreshold
	effectiveMode := baseMode
	escalated := false
	escalationTarget := "" // the tier escalation aimed for, before any snap; used only in the reason text
	escalationSnapNote := ""
	confidenceEscalationEnabled := requestedMode == "auto" && r.Weights.ConfidenceEscalationEnabled
	if confidenceBelowThreshold && confidenceEscalationEnabled {
		if escalatedMode, ok := escalateMode(baseMode); ok {
			escalationTarget = escalatedMode
			effectiveMode = escalatedMode
			escalated = true
			// The escalation target itself might not be servable by any
			// hard-filter survivor (same class of gap as the auto-mode
			// snap above). Since escalation is already a system decision
			// overriding the nominal mode, snapping it to the nearest
			// tier that is actually available keeps the same "when
			// unsure, prefer capable over absent" philosophy instead of
			// erroring out on a tier nothing can serve.
			if !availableModes[effectiveMode] {
				if snapped, ok := nearestAvailableMode(effectiveMode, availableModes); ok {
					escalationSnapNote = fmt.Sprintf(" (no hard-filter survivors at %s; snapped to %s)", escalationTarget, snapped)
					effectiveMode = snapped
				}
			}
		}
	}

	// thinkingMaxLocked forces Thinking/Max down to Instant regardless of
	// what auto-mode/escalation decided -- it is a spend-guarantee override,
	// not a quality signal, so it applies after those are resolved rather
	// than participating in the tier math itself. Unlike escalation, this
	// only ever moves down: nearestAvailableMode's "prefer stronger" bias
	// would be exactly backwards here.
	lockedDowngradeNote := ""
	if thinkingMaxLocked && effectiveMode != "instant" {
		if availableModes["instant"] {
			lockedDowngradeNote = fmt.Sprintf(" thinking_max locked for this billing cycle: forced %s -> instant.", effectiveMode)
			effectiveMode = "instant"
		} else {
			lockedDowngradeNote = fmt.Sprintf(" thinking_max locked, but no instant-tier candidate survives hard filters: kept %s.", effectiveMode)
		}
	}

	modeCandidates := filterByMode(commonCandidates, effectiveMode)
	if len(modeCandidates) == 0 {
		return RouteResult{}, fmt.Errorf(
			"router: no candidate models support mode %q after hard filters (%s)", effectiveMode, filterLog,
		)
	}

	taskCategory, taskIntent, taskFallbackNote := normalizeTaskClassification(input.TaskCategory, input.TaskIntent)
	winner, scoreLog := r.scoreAndPick(modeCandidates, taskCategory, taskIntent)

	var reason strings.Builder
	fmt.Fprintf(&reason, "hard filters passed: %s. ", filterLog)
	if requestedMode == "auto" {
		fmt.Fprintf(&reason, "auto mode resolved base_mode=%s (%s). ", baseMode, autoReason)
	} else {
		fmt.Fprintf(&reason, "requested_mode=%s used as base_mode. ", requestedMode)
	}
	switch {
	case escalated:
		fmt.Fprintf(&reason, "confidence %.2f < threshold %.2f: escalated %s -> %s%s. ",
			input.Confidence, r.Weights.ConfidenceEscalationThreshold, baseMode, escalationTarget, escalationSnapNote)
	case confidenceBelowThreshold && confidenceEscalationEnabled:
		fmt.Fprintf(&reason, "confidence %.2f < threshold %.2f: escalation triggered but base_mode=%s has no higher tier. ",
			input.Confidence, r.Weights.ConfidenceEscalationThreshold, baseMode)
	case confidenceBelowThreshold:
		fmt.Fprintf(&reason, "confidence %.2f < threshold %.2f: uncertainty recorded, mode unchanged. ",
			input.Confidence, r.Weights.ConfidenceEscalationThreshold)
	default:
		fmt.Fprintf(&reason, "confidence %.2f >= threshold %.2f: no escalation. ",
			input.Confidence, r.Weights.ConfidenceEscalationThreshold)
	}
	fmt.Fprintf(&reason, "scored %d candidate(s) for mode=%s: %s.", len(modeCandidates), effectiveMode, scoreLog)
	if taskFallbackNote != "" {
		fmt.Fprintf(&reason, " Task classification fallback: %s.", taskFallbackNote)
	}
	reason.WriteString(lockedDowngradeNote)

	return RouteResult{
		SelectedModelID:  winner.ID,
		SelectedMode:     effectiveMode,
		Reason:           reason.String(),
		EstimatedCostUSD: ComputeCostUSD(winner, estimatedContextTokens, input.EstimatedOutputTokens),
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
// window, a missing required tool, missing modality support, an output
// budget below the estimated response length, an unsupported output
// format, or membership in excludedModelIDs (e.g. a model whose circuit is
// currently open). It returns the surviving models plus a short
// human-readable log of what was checked, for inclusion in RouteResult.Reason.
func (r Router) applyHardFilters(input ClassifierOutput, estimatedContextTokens int, excludedModelIDs map[string]bool) ([]Model, string) {
	requiredModalities := map[string]bool{}
	for _, m := range input.ModalityInput {
		requiredModalities[m] = true
	}
	for _, m := range input.ModalityOutputExpected {
		requiredModalities[m] = true
	}

	var kept []Model
	for _, m := range r.Catalog.Models {
		if excludedModelIDs[m.ID] {
			continue
		}
		if m.ContextWindow < estimatedContextTokens {
			continue
		}
		if !supportsAllTools(m, input.RequiredTools) {
			continue
		}
		if !supportsAllModalities(m, requiredModalities) {
			continue
		}
		if input.EstimatedOutputTokens > 0 && m.MaxOutputTokens > 0 && m.MaxOutputTokens < input.EstimatedOutputTokens {
			continue
		}
		if input.OutputFormat != "" && !supportsOutputFormat(m, input.OutputFormat) {
			continue
		}
		kept = append(kept, m)
	}

	log := fmt.Sprintf(
		"context>=%d, tools=%v, modalities=%v, estimated_output_tokens=%d, output_format=%q -> %d/%d models",
		estimatedContextTokens, input.RequiredTools, modalitiesList(requiredModalities),
		input.EstimatedOutputTokens, input.OutputFormat, len(kept), len(r.Catalog.Models),
	)
	return kept, log
}

func supportsAllTools(m Model, required []string) bool {
	supported := map[string]bool{}
	for _, t := range m.SupportsTools {
		supported[t] = true
	}
	for _, t := range required {
		if !supported[t] {
			return false
		}
	}
	return true
}

func supportsOutputFormat(m Model, format string) bool {
	for _, f := range m.SupportsOutputFormats {
		if f == format {
			return true
		}
	}
	return false
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
	sort.Strings(out)
	return out
}

// modelSupportsMode reports whether m lists mode among its Modes.
func modelSupportsMode(m Model, mode string) bool {
	for _, mm := range m.Modes {
		if mm == mode {
			return true
		}
	}
	return false
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

// modeSet collects every mode tier reachable by at least one of the given
// models, e.g. an image-only model that only lists "instant" in its Modes
// contributes just {"instant"}.
func modeSet(models []Model) map[string]bool {
	set := map[string]bool{}
	for _, m := range models {
		for _, mode := range m.Modes {
			set[mode] = true
		}
	}
	return set
}

// nearestAvailableMode finds the closest tier to mode that is present in
// available, preferring to move up the tiers first (a stronger tier being
// the fallback direction matches the same "when unsure, prefer capable over
// absent" reasoning behind confidence-based escalation) and only falling
// back downward if nothing stronger is servable. Returns ok=false only if
// mode isn't a recognized tier at all.
func nearestAvailableMode(mode string, available map[string]bool) (string, bool) {
	idx := -1
	for i, m := range modeTiers {
		if m == mode {
			idx = i
			break
		}
	}
	if idx == -1 {
		return "", false
	}
	for i := idx + 1; i < len(modeTiers); i++ {
		if available[modeTiers[i]] {
			return modeTiers[i], true
		}
	}
	for i := idx - 1; i >= 0; i-- {
		if available[modeTiers[i]] {
			return modeTiers[i], true
		}
	}
	return "", false
}

// determineAutoMode picks a base mode tier for requestedMode=="auto" from a
// simple weighted combination of reasoning_depth, complexity_score and
// creativity_level (see the TODO'd constants above), then snaps that pick to
// the nearest tier actually reachable given availableModes -- the models
// that survived this request's hard filters. Without this snap, a request
// that (via required_tools/output_format/modality) can only be served by a
// model restricted to a single tier -- e.g. an image generator that only
// lists "instant" -- would compute a "natural" tier like "thinking" and then
// fail with "no candidate models support mode" even though a capable model
// exists, just not at that tier.
func (r Router) determineAutoMode(input ClassifierOutput, availableModes map[string]bool) (string, string) {
	w := r.Weights.AutoMode
	score := w.ReasoningWeight*enumScore(input.ReasoningDepth) +
		w.ComplexityWeight*input.ComplexityScore +
		w.CreativityWeight*enumScore(input.CreativityLevel)

	// Ceilings are inclusive: a score landing exactly on a boundary buys the
	// cheaper tier, not the pricier one. This matters for neutral or
	// unrecognized enum inputs (which fall back to "moderate" -- see
	// defaultThreeLevelEnum) landing exactly on the thinking ceiling; they
	// should not silently escalate all the way to "max".
	var naturalMode string
	switch {
	case score <= w.InstantCeiling:
		naturalMode = "instant"
	case score <= w.ThinkingCeiling:
		naturalMode = "thinking"
	default:
		naturalMode = "max"
	}

	mode := naturalMode
	snapNote := ""
	if !availableModes[mode] {
		if snapped, ok := nearestAvailableMode(mode, availableModes); ok {
			snapNote = fmt.Sprintf("; natural tier %s has no hard-filter survivors, adjusted to %s", naturalMode, snapped)
			mode = snapped
		}
	}

	reasonDetail := fmt.Sprintf(
		"score=%.2f (reasoning_depth=%s, complexity_score=%.2f, creativity_level=%s)%s",
		score, input.ReasoningDepth, input.ComplexityScore, input.CreativityLevel, snapNote,
	)
	return mode, reasonDetail
}

// scoreAndPick first keeps models whose category+intent fit is close enough
// to the best fit in this tier, then minimizes cost inside that quality
// band. This implements the routing rule "the cheapest model that preserves
// quality" without folding dollars and quality into one unitless score.
func (r Router) scoreAndPick(candidates []Model, category, intent string) (Model, string) {
	type scoredModel struct {
		model Model
		fit   float64
	}

	scored := make([]scoredModel, 0, len(candidates))
	bestFit := 0.0
	for _, model := range candidates {
		fit := taskProfileScore(model, category, intent, r.Weights.TaskProfile)
		scored = append(scored, scoredModel{model: model, fit: fit})
		bestFit = math.Max(bestFit, fit)
	}

	qualityFloor := math.Max(r.Weights.TaskProfile.MinimumScore, bestFit-r.Weights.TaskProfile.MaxQualityGap)
	qualified := make([]scoredModel, 0, len(scored))
	for _, candidate := range scored {
		if candidate.fit >= qualityFloor {
			qualified = append(qualified, candidate)
		}
	}
	// MinimumScore can deliberately be higher than every catalog score while
	// operators tune config. Keep the request servable by falling back to the
	// best-fit model(s), rather than turning a soft preference into an outage.
	if len(qualified) == 0 {
		qualityFloor = bestFit
		for _, candidate := range scored {
			if candidate.fit == bestFit {
				qualified = append(qualified, candidate)
			}
		}
	}

	best := qualified[0]
	bestModel := best.model
	bestCost := bestModel.CostInputPerMTok + bestModel.CostOutputPerMTok
	bestCapability := capabilityScore(bestModel)

	for _, candidate := range qualified[1:] {
		m := candidate.model
		cost := m.CostInputPerMTok + m.CostOutputPerMTok
		capability := capabilityScore(m)

		switch {
		case cost < bestCost:
			bestModel, bestCost, bestCapability = m, cost, capability
		case cost == bestCost && capability > bestCapability:
			bestModel, bestCost, bestCapability = m, cost, capability
		case cost == bestCost && capability == bestCapability && m.ID < bestModel.ID:
			bestModel, bestCost, bestCapability = m, cost, capability
		}
	}

	winnerFit := taskProfileScore(bestModel, category, intent, r.Weights.TaskProfile)
	log := fmt.Sprintf(
		"task_category=%s, task_intent=%s, best_fit=%.2f, quality_floor=%.2f, qualified=%d/%d; cheapest qualified: winner=%s (fit=%.2f, cost=%.2f, capability_score=%d)",
		category, intent, bestFit, qualityFloor, len(qualified), len(candidates), bestModel.ID, winnerFit, bestCost, bestCapability,
	)
	return bestModel, log
}

// capabilityScore is the tie-break metric for scoreAndPick: the total count
// of tools, modalities, and output formats a model declares support for.
// It only ever runs as a tiebreaker between models already at the same
// price, so a coarse count is enough to prefer the strictly more capable
// side of a tie (e.g. a model with web_search support over an otherwise
// identical one without it) without needing a weighted quality model.
func capabilityScore(m Model) int {
	return len(m.SupportsTools) + len(m.SupportsModality) + len(m.SupportsOutputFormats)
}
