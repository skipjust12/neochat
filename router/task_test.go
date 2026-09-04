package router

import (
	"strings"
	"testing"
)

func TestRoute_TaskProfilePrefersSpecialistBeforeMinimizingCost(t *testing.T) {
	cheapGeneralist := Model{
		ID: "cheap-generalist", Provider: "test", Modes: []string{"thinking"},
		CostInputPerMTok: 1, CostOutputPerMTok: 2, ContextWindow: 100_000,
		SupportsModality: []string{"text"}, SupportsOutputFormats: []string{"text"},
		TaskCategoryScores: map[string]float64{TaskCategoryGeneral: 0.80, TaskCategoryWriting: 0.70},
		TaskIntentScores:   map[string]float64{TaskIntentAnswer: 0.80, TaskIntentEdit: 0.78},
	}
	writingSpecialist := Model{
		ID: "writing-specialist", Provider: "test", Modes: []string{"thinking"},
		CostInputPerMTok: 3, CostOutputPerMTok: 8, ContextWindow: 100_000,
		SupportsModality: []string{"text"}, SupportsOutputFormats: []string{"text"},
		TaskCategoryScores: map[string]float64{TaskCategoryGeneral: 0.82, TaskCategoryWriting: 0.96},
		TaskIntentScores:   map[string]float64{TaskIntentAnswer: 0.82, TaskIntentEdit: 0.97},
	}
	r := NewRouter(Catalog{Models: []Model{cheapGeneralist, writingSpecialist}}, testWeights())

	input := baseInput()
	input.TaskCategory = TaskCategoryWriting
	input.TaskIntent = TaskIntentEdit
	input.ModalityInput = []string{"text"}
	input.ModalityOutputExpected = []string{"text"}
	input.OutputFormat = "text"
	input.Confidence = 0.9

	result, err := r.Route(input, "thinking", "", 1000, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedModelID != writingSpecialist.ID {
		t.Fatalf("selected %q, want task specialist %q", result.SelectedModelID, writingSpecialist.ID)
	}
	if !strings.Contains(result.Reason, "task_category=writing") || !strings.Contains(result.Reason, "task_intent=edit") {
		t.Fatalf("reason does not explain task-aware routing: %s", result.Reason)
	}
}

func TestRoute_TaskProfileChoosesCheapestWithinQualityBand(t *testing.T) {
	models := []Model{
		{
			ID: "cheap-close-fit", Provider: "test", Modes: []string{"instant"},
			CostInputPerMTok: 1, CostOutputPerMTok: 1, ContextWindow: 100_000,
			SupportsModality:   []string{"text"},
			TaskCategoryScores: map[string]float64{TaskCategoryGeneral: 0.90},
			TaskIntentScores:   map[string]float64{TaskIntentAnswer: 0.90},
		},
		{
			ID: "expensive-best-fit", Provider: "test", Modes: []string{"instant"},
			CostInputPerMTok: 5, CostOutputPerMTok: 5, ContextWindow: 100_000,
			SupportsModality:   []string{"text"},
			TaskCategoryScores: map[string]float64{TaskCategoryGeneral: 0.94},
			TaskIntentScores:   map[string]float64{TaskIntentAnswer: 0.94},
		},
	}
	r := NewRouter(Catalog{Models: models}, testWeights())
	input := baseInput()
	input.TaskCategory = TaskCategoryGeneral
	input.TaskIntent = TaskIntentAnswer
	input.ModalityInput = []string{"text"}
	input.ModalityOutputExpected = []string{"text"}
	input.Confidence = 0.9

	result, err := r.Route(input, "instant", "", 1000, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SelectedModelID != "cheap-close-fit" {
		t.Fatalf("selected %q, want cheapest model inside quality band", result.SelectedModelID)
	}
}

func TestRoute_UnknownTaskClassificationFallsBackSafely(t *testing.T) {
	r := NewRouter(testCatalog(), testWeights())
	input := baseInput()
	input.TaskCategory = "invented_category"
	input.TaskIntent = "invented_intent"
	input.Confidence = 0.9

	result, err := r.Route(input, "instant", "", 1000, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Reason, "fell back to general") || !strings.Contains(result.Reason, "fell back to answer") {
		t.Fatalf("reason does not report classification fallback: %s", result.Reason)
	}
}

func TestTaskProfileScoreUsesIntent(t *testing.T) {
	model := Model{
		TaskCategoryScores: map[string]float64{TaskCategoryWriting: 0.8},
		TaskIntentScores: map[string]float64{
			TaskIntentAnswer: 0.5,
			TaskIntentEdit:   1.0,
		},
	}
	weights := TaskProfileWeights{CategoryWeight: 0.5, IntentWeight: 0.5}
	answerScore := taskProfileScore(model, TaskCategoryWriting, TaskIntentAnswer, weights)
	editScore := taskProfileScore(model, TaskCategoryWriting, TaskIntentEdit, weights)
	if editScore <= answerScore {
		t.Fatalf("edit score %.2f should exceed answer score %.2f", editScore, answerScore)
	}
}

func TestRoute_RealCatalogChangesWinnerByTaskProfile(t *testing.T) {
	catalog, err := LoadCatalog("../configs/models.json")
	if err != nil {
		t.Fatal(err)
	}
	weights, err := LoadWeights("../configs/weights.json")
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(catalog, weights)
	input := baseInput()
	input.ModalityInput = []string{"text"}
	input.ModalityOutputExpected = []string{"text"}
	input.OutputFormat = "text"
	input.Confidence = 0.9

	input.TaskCategory = TaskCategoryWriting
	input.TaskIntent = TaskIntentEdit
	writingResult, err := r.Route(input, "thinking", "", 1000, false, nil)
	if err != nil {
		t.Fatal(err)
	}

	input.TaskCategory = TaskCategorySoftwareEngineering
	input.TaskIntent = TaskIntentDebug
	softwareResult, err := r.Route(input, "thinking", "", 1000, false, nil)
	if err != nil {
		t.Fatal(err)
	}

	if writingResult.SelectedModelID != "claude-sonnet-5" {
		t.Fatalf("writing route selected %q, want claude-sonnet-5", writingResult.SelectedModelID)
	}
	if softwareResult.SelectedModelID != "gpt-5.6-terra" {
		t.Fatalf("software route selected %q, want gpt-5.6-terra", softwareResult.SelectedModelID)
	}
}

func TestRoute_RealCatalogAvoidsSoftwareGeneratePriceCliff(t *testing.T) {
	catalog, err := LoadCatalog("../configs/models.json")
	if err != nil {
		t.Fatal(err)
	}
	weights, err := LoadWeights("../configs/weights.json")
	if err != nil {
		t.Fatal(err)
	}

	input := baseInput()
	input.TaskCategory = TaskCategorySoftwareEngineering
	input.TaskIntent = TaskIntentGenerate
	input.ModalityInput = []string{"text"}
	input.ModalityOutputExpected = []string{"code"}
	input.OutputFormat = "json"
	input.Confidence = 0.9

	result, err := NewRouter(catalog, weights).Route(input, "thinking", "", 1000, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.SelectedModelID != "gpt-5.6-terra" {
		t.Fatalf("selected %q, want gpt-5.6-terra inside the quality band", result.SelectedModelID)
	}
}
