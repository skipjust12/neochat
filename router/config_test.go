package router

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCatalog_LoadsTaskProfiles(t *testing.T) {
	catalog, err := LoadCatalog("../configs/models.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, model := range catalog.Models {
		if len(model.TaskCategoryScores) == 0 || len(model.TaskIntentScores) == 0 {
			t.Errorf("model %q has an incomplete task profile", model.ID)
		}
	}
}

func TestLoadCatalog_RejectsUnknownTaskProfileKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	data := `{"models":[{"id":"bad","task_category_scores":{"not_real":0.8},"task_intent_scores":{"answer":0.8}}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalog(path); err == nil {
		t.Fatal("expected an error for an unknown task category")
	}
}

func TestLoadWeights_LoadsTaskProfilePolicy(t *testing.T) {
	weights, err := LoadWeights("../configs/weights.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if weights.TaskProfile.CategoryWeight <= weights.TaskProfile.IntentWeight {
		t.Fatalf("category should be the primary task-profile signal: %+v", weights.TaskProfile)
	}
	if weights.TaskProfile.MaxQualityGap <= 0 {
		t.Fatalf("expected a positive quality band: %+v", weights.TaskProfile)
	}
}

func TestLoadWeights_RejectsInvalidTaskProfilePolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weights.json")
	data := `{"confidence_escalation_threshold":0.5,"auto_mode":{"instant_ceiling":1,"thinking_ceiling":2},"task_profile":{"category_weight":0,"intent_weight":0,"default_score":0.5,"minimum_score":0.6,"max_quality_gap":0.1}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWeights(path); err == nil {
		t.Fatal("expected an error for zero task-profile weights")
	}
}
