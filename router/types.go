package router

// ClassifierOutput is the JSON payload produced by the (cheap) classifier
// model that inspects the incoming user request. It is parsed as-is.
type ClassifierOutput struct {
	SchemaVersion          string   `json:"schema_version"`
	TaskType               string   `json:"task_type"`
	Language               string   `json:"language"`
	ModalityInput          []string `json:"modality_input"`
	ModalityOutputExpected []string `json:"modality_output_expected"`
	ReasoningDepth         string   `json:"reasoning_depth"` // "low" | "moderate" | "high"
	CreativityLevel        string   `json:"creativity_level"`
	NeedsWebSearch         bool     `json:"needs_web_search"`
	NeedsCodeExecution     bool     `json:"needs_code_execution"`
	ExpectedOutputLength   string   `json:"expected_output_length"`
	ComplexityScore        float64  `json:"complexity_score"`
	ContextDependency      string   `json:"context_dependency"`
	Confidence             float64  `json:"confidence"`
}

// Model describes a single entry in the model catalog (models.json).
type Model struct {
	ID                    string   `json:"id"`
	Provider              string   `json:"provider"`
	Modes                 []string `json:"modes"`
	CostInputPerMTok      float64  `json:"cost_input_per_mtok"`
	CostOutputPerMTok     float64  `json:"cost_output_per_mtok"`
	ContextWindow         int      `json:"context_window"`
	SupportsModality      []string `json:"supports_modality"`
	SupportsWebSearch     bool     `json:"supports_web_search"`
	SupportsCodeExecution bool     `json:"supports_code_execution"`
}

// Catalog is the full set of models loaded from models.json.
type Catalog struct {
	Models []Model `json:"models"`
}

// Weights holds the scoring weights and thresholds loaded from weights.json.
// Field meanings and units are documented alongside the JSON file itself.
type Weights struct {
	ReasoningDepthWeight          map[string]float64 `json:"reasoning_depth_weight"`
	ComplexityWeight              float64            `json:"complexity_weight"`
	CostWeight                    float64            `json:"cost_weight"`
	ConfidenceEscalationThreshold float64            `json:"confidence_escalation_threshold"`
}

// RouteResult is the output of the routing decision.
type RouteResult struct {
	SelectedModelID string `json:"selected_model_id"`
	SelectedMode    string `json:"selected_mode"`
	Reason          string `json:"reason"`
}
