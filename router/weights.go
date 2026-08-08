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
	if len(w.ReasoningDepthWeight) == 0 {
		return Weights{}, fmt.Errorf("router: weights file %q missing reasoning_depth_weight", path)
	}

	return w, nil
}
