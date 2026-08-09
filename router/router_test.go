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
	result, err := r.Route(baseInput(), "manual", "max-model", 1000, false)
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
	result, err := r.Route(input, "thinking", "", 300_000, false)
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
	result, err := r.Route(input, "instant", "", 1000, false)
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
	result, err := r.Route(withTools, "instant", "", 1000, false)
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
	result, err = r.Route(longOutput, "instant", "", 1000, false)
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
	result, err := r.Route(input, "auto", "", 1000, false)
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

	result, err := r.Route(simple, "auto", "", 1000, false)
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

	result, err = r.Route(hard, "auto", "", 1000, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedMode != "max" {
		t.Errorf("expected auto mode=max for a hard, high-complexity request, got %s", result.SelectedMode)
	}
}

// TestRoute_CostTieBreaksByCapabilityThenID pins down scoreAndPick's tie
// rule for equal-cost candidates in the same tier (e.g. claude-sonnet-5 vs
// kimi-k3, both $18/Mtok blended): the more capable model wins, and if
// capability is also tied, the lexicographically smaller ID wins. Catalog
// order must not decide the outcome.
func TestRoute_CostTieBreaksByCapabilityThenID(t *testing.T) {
	lessCapable := Model{
		ID: "z-model", Provider: "test", Modes: []string{"thinking"},
		CostInputPerMTok: 3, CostOutputPerMTok: 15, ContextWindow: 200_000,
		SupportsModality: []string{"text", "code"},
		MaxOutputTokens:  16384, SupportsOutputFormats: []string{"text", "markdown"},
	}
	moreCapable := Model{
		ID: "a-model", Provider: "test", Modes: []string{"thinking"},
		CostInputPerMTok: 3, CostOutputPerMTok: 15, ContextWindow: 200_000,
		SupportsModality: []string{"text", "code"},
		SupportsTools:    []string{ToolWebSearch},
		MaxOutputTokens:  16384, SupportsOutputFormats: []string{"text", "markdown"},
	}
	r := NewRouter(Catalog{Models: []Model{lessCapable, moreCapable}}, testWeights())

	input := baseInput()
	input.ReasoningDepth = "high"
	input.ComplexityScore = 0.6
	input.CreativityLevel = "moderate"

	result, err := r.Route(input, "thinking", "", 1000, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedModelID != "a-model" {
		t.Errorf("expected the more capable equal-cost model (a-model) to win the tie, got %s", result.SelectedModelID)
	}

	// Now make both models equally capable too -- the tie must fall through
	// to ID order, not catalog order.
	moreCapable.SupportsTools = nil
	r = NewRouter(Catalog{Models: []Model{lessCapable, moreCapable}}, testWeights())
	result, err = r.Route(input, "thinking", "", 1000, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedModelID != "a-model" {
		t.Errorf("expected fully-tied models to break by ID order (a-model < z-model), got %s", result.SelectedModelID)
	}
}

// TestRoute_ThinkingMaxLockedForcesInstant pins down the limits-package
// integration point: when thinkingMaxLocked is true, Thinking/Max requests
// (whatever auto-mode or the user's explicit requestedMode picked) must be
// forced down to instant, never blocked outright -- see
// docs/unit-economics.md section 6.4 ("never a full app block, only a
// downgrade").
func TestRoute_ThinkingMaxLockedForcesInstant(t *testing.T) {
	r := NewRouter(testCatalog(), testWeights())
	input := baseInput()
	input.Confidence = 0.9 // avoid escalation muddying the assertion

	result, err := r.Route(input, "thinking", "", 1000, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedMode != "instant" {
		t.Errorf("expected thinkingMaxLocked to force mode=instant, got %s", result.SelectedMode)
	}
	if result.SelectedModelID != "instant-model" {
		t.Errorf("expected the instant-tier model to win once forced down, got %s", result.SelectedModelID)
	}

	// Unlocked, the same request should route to thinking as normal --
	// confirms the lock is the thing making the difference above, not some
	// other side effect.
	unlocked, err := r.Route(input, "thinking", "", 1000, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if unlocked.SelectedMode != "thinking" {
		t.Errorf("expected unlocked request to stay at thinking, got %s", unlocked.SelectedMode)
	}
}

// TestRoute_ThinkingMaxLockedRejectsManualThinkingModel confirms the lock
// closes the manual-mode loophole called out in
// docs/unit-economics.md section 6.1: manual bypasses scoring, but not the
// spend lock. Unlike the scored path, there is no cheaper candidate to
// silently substitute for what the user explicitly asked for, so this must
// error rather than pick a different model on the caller's behalf.
func TestRoute_ThinkingMaxLockedRejectsManualThinkingModel(t *testing.T) {
	r := NewRouter(testCatalog(), testWeights())
	input := baseInput()

	_, err := r.Route(input, "manual", "max-model", 1000, true)
	if err == nil {
		t.Fatal("expected an error routing to a thinking/max-tier model while thinking_max is locked, got nil")
	}

	// An instant-tier manual target must still work even when locked --
	// the lock only concerns Thinking/Max spend.
	result, err := r.Route(input, "manual", "instant-model", 1000, true)
	if err != nil {
		t.Fatalf("unexpected error routing to an instant-tier manual target while locked: %v", err)
	}
	if result.SelectedModelID != "instant-model" {
		t.Errorf("expected instant-model, got %s", result.SelectedModelID)
	}
}
