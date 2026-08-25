package router

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadCatalog reads and parses a models.json file at the given path.
func LoadCatalog(path string) (Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, fmt.Errorf("router: read catalog file: %w", err)
	}

	var catalog Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("router: parse catalog file: %w", err)
	}
	if len(catalog.Models) == 0 {
		return Catalog{}, fmt.Errorf("router: catalog file %q contains no models", path)
	}
	for _, model := range catalog.Models {
		if err := validateTaskScores(model.ID, "task_category_scores", model.TaskCategoryScores, knownTaskCategories); err != nil {
			return Catalog{}, err
		}
		if err := validateTaskScores(model.ID, "task_intent_scores", model.TaskIntentScores, knownTaskIntents); err != nil {
			return Catalog{}, err
		}
	}

	return catalog, nil
}

func validateTaskScores(modelID, field string, scores map[string]float64, knownValues map[string]bool) error {
	if len(scores) == 0 {
		return fmt.Errorf("router: model %q has no %s", modelID, field)
	}
	for name, score := range scores {
		if !knownValues[name] {
			return fmt.Errorf("router: model %q has unknown %s key %q", modelID, field, name)
		}
		if score < 0 || score > 1 {
			return fmt.Errorf("router: model %q %s.%s %.2f out of range [0,1]", modelID, field, name, score)
		}
	}
	return nil
}

// FindModel looks up a model by ID in the catalog.
func (c Catalog) FindModel(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// ResolveAPIModelID returns the string a provider.Client should use to
// call this model: APIModelID if the catalog set one explicitly, or ID
// otherwise (the common case -- most catalog IDs already match the
// vendor's own naming).
func (m Model) ResolveAPIModelID() string {
	if m.APIModelID != "" {
		return m.APIModelID
	}
	return m.ID
}
