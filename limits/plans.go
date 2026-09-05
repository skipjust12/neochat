package limits

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadPlanLimits reads a plans.json file (see configs/plans.json) into a
// map keyed by plan_id.
func LoadPlanLimits(path string) (map[string]PlanLimits, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("limits: read plans file: %w", err)
	}

	var raw struct {
		Plans []PlanLimits `json:"plans"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("limits: parse plans file: %w", err)
	}
	if len(raw.Plans) == 0 {
		return nil, fmt.Errorf("limits: plans file %q contains no plans", path)
	}

	byID := make(map[string]PlanLimits, len(raw.Plans))
	for _, p := range raw.Plans {
		if !validMoney(p.ThinkingMaxCapUSD) || !validMoney(p.InstantExtraCapUSD) {
			return nil, fmt.Errorf("limits: invalid plan caps")
		}
		if p.PlanID == "" {
			return nil, fmt.Errorf("limits: plan entry missing plan_id")
		}
		byID[p.PlanID] = p
	}
	return byID, nil
}
