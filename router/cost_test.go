package router

import (
	"testing"
	"time"
)

func TestComputeCostUSD(t *testing.T) {
	model := Model{CostInputPerMTok: 3, CostOutputPerMTok: 15}

	got := ComputeCostUSD(model, 1_000_000, 1_000_000)
	want := 3.0 + 15.0
	if got != want {
		t.Errorf("ComputeCostUSD(1M in, 1M out) = %.4f, want %.4f", got, want)
	}

	got = ComputeCostUSD(model, 0, 0)
	if got != 0 {
		t.Errorf("ComputeCostUSD(0, 0) = %.4f, want 0", got)
	}
}

func TestNewCostLogEntry(t *testing.T) {
	model := Model{ID: "thinking-model", CostInputPerMTok: 3, CostOutputPerMTok: 10}
	ts := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	entry := NewCostLogEntry("user-1", "req-1", model, "thinking", 500_000, 200_000, ts)

	if entry.UserID != "user-1" || entry.RequestID != "req-1" || entry.ModelID != "thinking-model" || entry.Mode != "thinking" {
		t.Errorf("unexpected identity fields: %+v", entry)
	}
	if entry.InputTokens != 500_000 || entry.OutputTokens != 200_000 {
		t.Errorf("unexpected token fields: %+v", entry)
	}
	wantCost := 500_000.0/1_000_000*3 + 200_000.0/1_000_000*10
	if entry.CostUSD != wantCost {
		t.Errorf("CostUSD = %.4f, want %.4f", entry.CostUSD, wantCost)
	}
	if !entry.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", entry.Timestamp, ts)
	}
}

func TestRoute_EstimatedCostUSDMatchesSelectedModel(t *testing.T) {
	r := NewRouter(testCatalog(), testWeights())

	input := baseInput()
	input.EstimatedOutputTokens = 1000

	result, err := r.Route(input, "thinking", "", 2000, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	model, ok := r.Catalog.FindModel(result.SelectedModelID)
	if !ok {
		t.Fatalf("selected model %q not found in catalog", result.SelectedModelID)
	}
	want := ComputeCostUSD(model, 2000, 1000)
	if result.EstimatedCostUSD != want {
		t.Errorf("EstimatedCostUSD = %.6f, want %.6f", result.EstimatedCostUSD, want)
	}

	manual, err := r.Route(input, "manual", "max-model", 2000, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	maxModel, _ := r.Catalog.FindModel("max-model")
	wantManual := ComputeCostUSD(maxModel, 2000, 1000)
	if manual.EstimatedCostUSD != wantManual {
		t.Errorf("manual EstimatedCostUSD = %.6f, want %.6f", manual.EstimatedCostUSD, wantManual)
	}
}
