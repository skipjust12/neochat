package router

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadWeights reads and parses a weights.json file at the given path.
func LoadWeights(path string) (Weights, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Weights{}, fmt.Errorf("router: read weights file: %w", err)
	}

	var w Weights
	if err := json.Unmarshal(data, &w); err != nil {
		return Weights{}, fmt.Errorf("router: parse weights file: %w", err)
	}
	if w.AutoMode.ThinkingCeiling <= w.AutoMode.InstantCeiling {
		return Weights{}, fmt.Errorf(
			"router: weights file %q has auto_mode.thinking_ceiling (%.2f) <= auto_mode.instant_ceiling (%.2f)",
			path, w.AutoMode.ThinkingCeiling, w.AutoMode.InstantCeiling,
		)
	}
	if w.ConfidenceEscalationThreshold < 0 || w.ConfidenceEscalationThreshold > 1 {
		return Weights{}, fmt.Errorf(
			"router: weights file %q has confidence_escalation_threshold %.2f out of range [0,1]",
			path, w.ConfidenceEscalationThreshold,
		)
	}

	return w, nil
}
