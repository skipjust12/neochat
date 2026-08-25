package router

import "fmt"

const (
	TaskCategoryGeneral             = "general"
	TaskCategoryWriting             = "writing"
	TaskCategoryFrontend            = "frontend"
	TaskCategorySoftwareEngineering = "software_engineering"
	TaskCategoryDataAnalysis        = "data_analysis"
	TaskCategoryMath                = "math"
	TaskCategoryResearch            = "research"
	TaskCategoryReasoning           = "reasoning"
	TaskCategoryKnowledge           = "knowledge"
	TaskCategoryTranslation         = "translation"
	TaskCategoryVision              = "vision"
	TaskCategoryImageGeneration     = "image_generation"
)

const (
	TaskIntentAnswer    = "answer"
	TaskIntentGenerate  = "generate"
	TaskIntentEdit      = "edit"
	TaskIntentDebug     = "debug"
	TaskIntentReview    = "review"
	TaskIntentExplain   = "explain"
	TaskIntentSummarize = "summarize"
	TaskIntentCompare   = "compare"
	TaskIntentPlan      = "plan"
	TaskIntentExtract   = "extract"
	TaskIntentClassify  = "classify"
	TaskIntentTransform = "transform"
)

var knownTaskCategories = map[string]bool{
	TaskCategoryGeneral: true, TaskCategoryWriting: true,
	TaskCategoryFrontend: true, TaskCategorySoftwareEngineering: true,
	TaskCategoryDataAnalysis: true, TaskCategoryMath: true,
	TaskCategoryResearch: true, TaskCategoryReasoning: true,
	TaskCategoryKnowledge: true, TaskCategoryTranslation: true,
	TaskCategoryVision: true, TaskCategoryImageGeneration: true,
}

var knownTaskIntents = map[string]bool{
	TaskIntentAnswer: true, TaskIntentGenerate: true, TaskIntentEdit: true,
	TaskIntentDebug: true, TaskIntentReview: true, TaskIntentExplain: true,
	TaskIntentSummarize: true, TaskIntentCompare: true, TaskIntentPlan: true,
	TaskIntentExtract: true, TaskIntentClassify: true, TaskIntentTransform: true,
}

func normalizeTaskClassification(category, intent string) (string, string, string) {
	normalizedCategory := category
	normalizedIntent := intent
	var note string
	if !knownTaskCategories[normalizedCategory] {
		normalizedCategory = TaskCategoryGeneral
		note = fmt.Sprintf("unknown task_category=%q fell back to %s", category, normalizedCategory)
	}
	if !knownTaskIntents[normalizedIntent] {
		normalizedIntent = TaskIntentAnswer
		if note != "" {
			note += "; "
		}
		note += fmt.Sprintf("unknown task_intent=%q fell back to %s", intent, normalizedIntent)
	}
	return normalizedCategory, normalizedIntent, note
}

func taskProfileScore(model Model, category, intent string, weights TaskProfileWeights) float64 {
	categoryScore := lookupTaskScore(model.TaskCategoryScores, category, TaskCategoryGeneral, weights.DefaultScore)
	intentScore := lookupTaskScore(model.TaskIntentScores, intent, TaskIntentAnswer, weights.DefaultScore)
	totalWeight := weights.CategoryWeight + weights.IntentWeight
	if totalWeight <= 0 {
		return weights.DefaultScore
	}
	return (weights.CategoryWeight*categoryScore + weights.IntentWeight*intentScore) / totalWeight
}

func lookupTaskScore(scores map[string]float64, key, fallbackKey string, defaultScore float64) float64 {
	if score, ok := scores[key]; ok {
		return score
	}
	if score, ok := scores[fallbackKey]; ok {
		return score
	}
	return defaultScore
}
