package main

import (
	"encoding/json"
	"fmt"
	"log"

	"neochat/router"
)

func main() {
	catalog, err := router.LoadCatalog("configs/models.json")
	if err != nil {
		log.Fatal(err)
	}
	weights, err := router.LoadWeights("configs/weights.json")
	if err != nil {
		log.Fatal(err)
	}

	r := router.NewRouter(catalog, weights)

	rawInput := `{
		"schema_version": "1.0",
		"task_type": "code_generation",
		"language": "ru",
		"modality_input": ["text", "code"],
		"modality_output_expected": ["text", "code"],
		"reasoning_depth": "moderate",
		"creativity_level": "low",
		"required_tools": [],
		"expected_output_length": "medium",
		"estimated_output_tokens": 1200,
		"output_format": "text",
		"complexity_score": 0.6,
		"context_dependency": "light",
		"confidence": 0.82,
		"content_flags": [],
		"safety_risk_score": 0.0
	}`

	var input router.ClassifierOutput
	if err := json.Unmarshal([]byte(rawInput), &input); err != nil {
		log.Fatal(err)
	}

	result, err := r.Route(input, "auto", "", 8_000)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("selected_model_id:  %s\n", result.SelectedModelID)
	fmt.Printf("selected_mode:      %s\n", result.SelectedMode)
	fmt.Printf("estimated_cost_usd: %.6f\n", result.EstimatedCostUSD)
	fmt.Printf("reason:             %s\n", result.Reason)
}
