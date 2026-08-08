package router

// Known tool identifiers used in ClassifierOutput.RequiredTools and
// Model.SupportsTools. This is not a closed enum: new tool types can be
// added on both sides without touching this Go code or the JSON schema's
// shape, since both fields are plain string lists.
const (
	ToolWebSearch     = "web_search"
	ToolCodeExecution = "code_execution"
)

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

	// RequiredTools lists tool capabilities the request needs (e.g.
	// "web_search", "code_execution"). A model must declare every entry in
	// its own SupportsTools to be a candidate. Generic on purpose: adding a
	// new tool type (retrieval, image_generation, ...) needs no schema or
	// struct change, just a new string value both sides agree on.
	RequiredTools []string `json:"required_tools"`

	ExpectedOutputLength string `json:"expected_output_length"` // "short" | "medium" | "long"

	// EstimatedOutputTokens is the classifier's numeric estimate of the
	// response length. 0 means "not estimated" and skips the
	// max-output-tokens hard filter entirely.
	EstimatedOutputTokens int `json:"estimated_output_tokens"`

	// OutputFormat is the structural format the response is expected to
	// take (e.g. "text", "markdown", "json", "function_call"). Empty means
	// unconstrained and skips the output-format hard filter.
	OutputFormat string `json:"output_format"`

	ComplexityScore   float64 `json:"complexity_score"`
	ContextDependency string  `json:"context_dependency"`
	Confidence        float64 `json:"confidence"`

	// Reserved for a future moderation iteration. Parsed so the schema is
	// stable end-to-end, but nothing in Route reads these fields yet -- no
	// hard filter, no scoring effect, no safety routing. TODO: wire up once
	// moderation is actually implemented.
	ContentFlags    []string `json:"content_flags"`
	SafetyRiskScore float64  `json:"safety_risk_score"`
}

// Model describes a single entry in the model catalog (models.json).
type Model struct {
	ID                string   `json:"id"`
	Provider          string   `json:"provider"`
	Modes             []string `json:"modes"`
	CostInputPerMTok  float64  `json:"cost_input_per_mtok"`
	CostOutputPerMTok float64  `json:"cost_output_per_mtok"`
	ContextWindow     int      `json:"context_window"`
	SupportsModality  []string `json:"supports_modality"`

	// SupportsTools lists the tool identifiers (see ToolWebSearch etc.)
	// this model can actually invoke. Matched against
	// ClassifierOutput.RequiredTools during hard filtering.
	SupportsTools []string `json:"supports_tools"`

	// MaxOutputTokens is the model's response length ceiling. 0 means
	// "unknown/unbounded" and is never used to exclude the model.
	MaxOutputTokens int `json:"max_output_tokens"`

	// SupportsOutputFormats lists structural output formats this model can
	// reliably produce (e.g. "text", "markdown", "json", "function_call").
	// Matched against ClassifierOutput.OutputFormat during hard filtering.
	SupportsOutputFormats []string `json:"supports_output_formats"`
}

// Catalog is the full set of models loaded from models.json.
type Catalog struct {
	Models []Model `json:"models"`
}

// AutoModeWeights configures the auto-mode tier heuristic: how much each
// classifier signal contributes to the "how much model do we need" score,
// and where the instant/thinking/max boundaries sit on that score.
//
// score is reasoning_weight*enumScore(reasoning_depth) plus
// complexity_weight*complexity_score plus
// creativity_weight*enumScore(creativity_level), where enumScore maps
// "low"/"moderate"/"high" to 0/1/2. Ceilings are inclusive: a score landing
// exactly on a boundary buys the cheaper tier.
type AutoModeWeights struct {
	ReasoningWeight  float64 `json:"reasoning_weight"`
	ComplexityWeight float64 `json:"complexity_weight"`
	CreativityWeight float64 `json:"creativity_weight"`
	InstantCeiling   float64 `json:"instant_ceiling"`
	ThinkingCeiling  float64 `json:"thinking_ceiling"`
}

// Weights holds the scoring weights and thresholds loaded from weights.json.
// Field meanings and units are documented alongside the JSON file itself.
//
// Model selection within an already-chosen tier is deliberately not
// weighted by anything here: once a tier is picked, every survivor in it
// has already been judged "good enough" (that's what the tier and the hard
// filters are for), so Route always takes the cheapest one -- see
// scoreAndPick. AutoModeWeights and ConfidenceEscalationThreshold are the
// only places quality/cost tradeoffs are actually made.
type Weights struct {
	ConfidenceEscalationThreshold float64         `json:"confidence_escalation_threshold"`
	AutoMode                      AutoModeWeights `json:"auto_mode"`
}

// RouteResult is the output of the routing decision.
type RouteResult struct {
	SelectedModelID string `json:"selected_model_id"`
	SelectedMode    string `json:"selected_mode"`
	Reason          string `json:"reason"`
}
