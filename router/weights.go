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
	if w.TaskProfile.CategoryWeight < 0 || w.TaskProfile.IntentWeight < 0 ||
		w.TaskProfile.CategoryWeight+w.TaskProfile.IntentWeight == 0 {
		return Weights{}, fmt.Errorf(
			"router: weights file %q task_profile weights must be non-negative and have a positive sum", path,
		)
	}
	for name, value := range map[string]float64{
		"default_score":   w.TaskProfile.DefaultScore,
		"minimum_score":   w.TaskProfile.MinimumScore,
		"max_quality_gap": w.TaskProfile.MaxQualityGap,
	} {
		if value < 0 || value > 1 {
			return Weights{}, fmt.Errorf(
				"router: weights file %q task_profile.%s %.2f out of range [0,1]", path, name, value,
			)
		}
	}

	return w, nil
}
