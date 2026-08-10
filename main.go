package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"neochat/limits"
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
	plans, err := limits.LoadPlanLimits("configs/plans.json")
	if err != nil {
		log.Fatal(err)
	}

	r := router.NewRouter(catalog, weights)
	store := limits.NewInMemorySpendStore()
	ctx := context.Background()

	plan := plans["pro"]
	userID := "demo-user"

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

	// Request 1: fresh user, nothing spent yet -- routes normally.
	routeAndRecord(ctx, r, store, plan, userID, input)

	// Simulate this user having already burned through their Thinking+Max
	// cap earlier in the billing cycle (e.g. a prior heavy session), then
	// send the same request again -- this is the section 6.4 downgrade path.
	if err := limits.RecordThinkingMaxSpend(ctx, store, userID, plan.ThinkingMaxCapUSD, time.Now()); err != nil {
		log.Fatal(err)
	}
	fmt.Println("--- simulated: user has now spent their full Thinking+Max cap for this cycle ---")
	routeAndRecord(ctx, r, store, plan, userID, input)
}

// routeAndRecord runs one request end-to-end through the limits check,
// router.Route, and the post-generation spend record -- the same sequence
// a real server would follow (see docs/unit-economics.md section 6 and the
// limits package doc comment).
func routeAndRecord(ctx context.Context, r router.Router, store limits.SpendStore, plan limits.PlanLimits, userID string, input router.ClassifierOutput) {
	locked, err := limits.CheckThinkingMaxLock(ctx, store, plan, userID)
	if err != nil {
		log.Fatal(err)
	}

	result, err := r.Route(input, "auto", "", 8_000, locked, nil)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("thinking_max_locked: %v\n", locked)
	fmt.Printf("selected_model_id:   %s\n", result.SelectedModelID)
	fmt.Printf("selected_mode:       %s\n", result.SelectedMode)
	fmt.Printf("estimated_cost_usd:  %.6f\n", result.EstimatedCostUSD)
	fmt.Printf("reason:              %s\n\n", result.Reason)

	// In a real system this fires after generation completes, with the
	// actual token usage -- EstimatedCostUSD is a pre-flight guess, this
	// demo just reuses it since there is no real generation call yet.
	if result.SelectedMode != "instant" {
		if err := limits.RecordThinkingMaxSpend(ctx, store, userID, result.EstimatedCostUSD, time.Now()); err != nil {
			log.Fatal(err)
		}
	}
}
