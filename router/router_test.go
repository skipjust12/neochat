package router

import "testing"

func testCatalog() Catalog {
	return Catalog{Models: []Model{
		{
			ID: "instant-model", Provider: "test", Modes: []string{"instant"},
			CostInputPerMTok: 1, CostOutputPerMTok: 2, ContextWindow: 50_000,
			SupportsModality: []string{"text", "code"},
			MaxOutputTokens:  4096, SupportsOutputFormats: []string{"text", "markdown"},
		},
		{
			ID: "thinking-model", Provider: "test", Modes: []string{"instant", "thinking"},
			CostInputPerMTok: 3, CostOutputPerMTok: 10, ContextWindow: 200_000,
			SupportsModality: []string{"text", "code", "image"},
			SupportsTools:    []string{ToolWebSearch, ToolCodeExecution},
			MaxOutputTokens:  16384, SupportsOutputFormats: []string{"text", "markdown", "json"},
		},
		{
			ID: "max-model", Provider: "test", Modes: []string{"thinking", "max"},
			CostInputPerMTok: 15, CostOutputPerMTok: 75, ContextWindow: 500_000,
			SupportsModality: []string{"text", "code", "image"},
			SupportsTools:    []string{ToolWebSearch, ToolCodeExecution},
			MaxOutputTokens:  32768, SupportsOutputFormats: []string{"text", "markdown", "json", "function_call"},
		},
	}}
}

func testWeights() Weights {
	return Weights{
		ConfidenceEscalationThreshold: 0.5,
		AutoMode: AutoModeWeights{
			ReasoningWeight:  1.0,
			ComplexityWeight: 2.0,
			CreativityWeight: 0.2,
			InstantCeiling:   1.0,
			ThinkingCeiling:  2.5,
		},
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
		RequiredTools:          nil,
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

func TestRoute_HardFilterByToolsOutputTokensAndFormat(t *testing.T) {
	r := NewRouter(testCatalog(), testWeights())

	// instant-model declares no tools and only "text"/"markdown" output
	// formats, so a request needing code_execution and JSON output must
	// skip it even though instant is otherwise the cheapest tier.
	withTools := baseInput()
	withTools.Confidence = 0.9
	withTools.RequiredTools = []string{ToolCodeExecution}
	withTools.OutputFormat = "json"
	result, err := r.Route(withTools, "instant", "", 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedModelID != "thinking-model" {
		t.Errorf("expected thinking-model (only instant-tier model with code_execution+json), got %s", result.SelectedModelID)
	}

	// instant-model caps at 4096 output tokens; asking for far more should
	// exclude it even with no tool/format requirements at all.
	longOutput := baseInput()
	longOutput.Confidence = 0.9
	longOutput.EstimatedOutputTokens = 10_000
	result, err = r.Route(longOutput, "instant", "", 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedModelID != "thinking-model" {
		t.Errorf("expected thinking-model (only instant-tier model with enough max_output_tokens), got %s", result.SelectedModelID)
	}
}

func TestRoute_AutoModeSnapsToAvailableTierWhenNaturalTierHasNoCandidates(t *testing.T) {
	// A single-model catalog that can only ever serve "instant", modeling an
	// image-only generator (like nano-banana-2-lite in the real catalog).
	catalog := Catalog{Models: []Model{
		{
			ID: "image-only-model", Provider: "test", Modes: []string{"instant"},
			CostInputPerMTok: 1, CostOutputPerMTok: 1, ContextWindow: 100_000,
			SupportsModality:      []string{"text", "image"},
			SupportsOutputFormats: []string{"image"},
		},
	}}
	r := NewRouter(catalog, testWeights())

	// high/high/0.9 pushes the auto heuristic's "natural" tier to "max", but
	// the only hard-filter survivor is instant-only. Before the fix this
	// returned "no candidate models support mode \"max\"" even though a
	// perfectly capable model existed, just not at that tier.
	input := ClassifierOutput{
		ReasoningDepth: "high", CreativityLevel: "high",
		ModalityInput: []string{"text"}, ModalityOutputExpected: []string{"image"},
		OutputFormat: "image", ComplexityScore: 0.9, Confidence: 0.9,
	}
	result, err := r.Route(input, "auto", "", 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedModelID != "image-only-model" {
		t.Errorf("expected image-only-model, got %s", result.SelectedModelID)
	}
	if result.SelectedMode != "instant" {
		t.Errorf("expected auto mode to snap down to instant (the only available tier), got %s", result.SelectedMode)
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
