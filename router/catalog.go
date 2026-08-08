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

	return catalog, nil
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
