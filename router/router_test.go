package router

import "testing"

func testCatalog() Catalog {
	return Catalog{Models: []Model{
		{
			ID: "instant-model", Provider: "test", Modes: []string{"instant"},
			CostInputPerMTok: 1, CostOutputPerMTok: 2, ContextWindow: 50_000,
			SupportsModality: []string{"text", "code"},
		},
		{
			ID: "thinking-model", Provider: "test", Modes: []string{"instant", "thinking"},
			CostInputPerMTok: 3, CostOutputPerMTok: 10, ContextWindow: 200_000,
			SupportsModality:  []string{"text", "code", "image"},
			SupportsWebSearch: true, SupportsCodeExecution: true,
		},
		{
			ID: "max-model", Provider: "test", Modes: []string{"thinking", "max"},
			CostInputPerMTok: 15, CostOutputPerMTok: 75, ContextWindow: 500_000,
			SupportsModality:  []string{"text", "code", "image"},
			SupportsWebSearch: true, SupportsCodeExecution: true,
		},
	}}
}

func testWeights() Weights {
	return Weights{
		ReasoningDepthWeight:          map[string]float64{"low": 0.2, "moderate": 0.6, "high": 1.2},
		ComplexityWeight:              1.0,
		CostWeight:                    0.5,
		ConfidenceEscalationThreshold: 0.5,
	}
}

func baseInput() ClassifierOutput {
	return ClassifierOutput{
		SchemaVersion:          "1.0",
		TaskType:               "code_generation",
		Language:               "ru",
		ModalityInput:          []string{"text", "code"},
		ModalityOutputExpected: []string{"text", "code"},
		ReasoningDepth:         "moderate",
		CreativityLevel:        "low",
		NeedsWebSearch:         false,
		NeedsCodeExecution:     false,
		ExpectedOutputLength:   "medium",
		ComplexityScore:        0.6,
		ContextDependency:      "light",
		Confidence:             0.82,
	}
}

func TestRoute_ManualBypassesScoring(t *testing.T) {
	r := NewRouter(testCatalog(), testWeights())
	result, err := r.Route(baseInput(), "manual", "max-model", 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedModelID != "max-model" {
		t.Errorf("expected max-model, got %s", result.SelectedModelID)
	}
	if result.SelectedMode != "manual" {
		t.Errorf("expected mode=manual, got %s", result.SelectedMode)
	}
}

func TestRoute_HardFilterByContextWindow(t *testing.T) {
	r := NewRouter(testCatalog(), testWeights())
	// Only max-model has a large enough context window (500k); requestedMode
	// pins to "thinking", where both thinking-model and max-model qualify by
	// mode, but thinking-model's 200k context window is too small.
	input := baseInput()
	input.Confidence = 0.9 // avoid escalation muddying the assertion
	result, err := r.Route(input, "thinking", "", 300_000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedModelID != "max-model" {
		t.Errorf("expected max-model (only one with enough context), got %s", result.SelectedModelID)
	}
}

func TestRoute_LowConfidenceEscalatesModeTier(t *testing.T) {
	r := NewRouter(testCatalog(), testWeights())
	input := baseInput()
	input.Confidence = 0.1 // well below the 0.5 threshold -> forces escalation
	result, err := r.Route(input, "instant", "", 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedMode != "thinking" {
		t.Errorf("expected escalation from instant to thinking, got mode=%s", result.SelectedMode)
	}
}

func TestRoute_AutoModePicksTierFromComplexity(t *testing.T) {
	r := NewRouter(testCatalog(), testWeights())

	simple := baseInput()
	simple.ReasoningDepth = "low"
	simple.ComplexityScore = 0.1
	simple.CreativityLevel = "low"
	simple.Confidence = 0.95

	result, err := r.Route(simple, "auto", "", 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedMode != "instant" {
		t.Errorf("expected auto mode=instant for a simple low-complexity request, got %s", result.SelectedMode)
	}

	hard := baseInput()
	hard.ReasoningDepth = "high"
	hard.ComplexityScore = 0.95
	hard.CreativityLevel = "high"
	hard.Confidence = 0.95

	result, err = r.Route(hard, "auto", "", 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedMode != "max" {
		t.Errorf("expected auto mode=max for a hard, high-complexity request, got %s", result.SelectedMode)
	}
}
