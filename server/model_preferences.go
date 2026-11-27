package server

import (
	"encoding/json"
	"fmt"
	"neochat/router"
	"strings"
)

const maxPersonalSettingsBytes = 256 << 10

// Account-owned presentation and manual-routing overrides. Shared catalog
// scoring and other users are never changed by model management.
type modelPreference struct {
	Name          string  `json:"name,omitempty"`
	Description   *string `json:"description,omitempty"`
	URL           string  `json:"url,omitempty"`
	Provider      string  `json:"provider,omitempty"`
	Tier          string  `json:"tier,omitempty"`
	ContextWindow int     `json:"context_window,omitempty"`
	Hidden        bool    `json:"hidden,omitempty"`
}

func personalModelSettings(object map[string]json.RawMessage) (map[string]modelPreference, []string, error) {
	preferences := map[string]modelPreference{}
	order := []string{}
	if raw := object["model_preferences"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &preferences); err != nil {
			return nil, nil, err
		}
	}
	if raw := object["model_order"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &order); err != nil {
			return nil, nil, err
		}
	}
	if preferences == nil {
		preferences = map[string]modelPreference{}
	}
	return preferences, order, nil
}
func validateModelPreference(p modelPreference) error {
	provider := p.Provider
	if provider == "" {
		provider = "openai"
	}
	link := p.URL
	if link == "" {
		link = "placeholder"
	}
	_, err := normalizeCustomModel(customModel{Provider: provider, URL: link, Name: p.Name, Description: func() string {
		if p.Description != nil {
			return *p.Description
		}
		return ""
	}(), Tier: p.Tier, ContextWindow: p.ContextWindow})
	return err
}
func applyModelPreference(m router.Model, p modelPreference) router.Model {
	if p.Name != "" {
		m.DisplayName = p.Name
	}
	if p.Description != nil {
		m.Description = *p.Description
	}
	if p.Provider != "" {
		m.Provider = p.Provider
	}
	if p.URL != "" {
		definition, err := normalizeCustomModel(customModel{Provider: m.Provider, URL: p.URL})
		if err == nil {
			m.APIModelID = definition.Handle
			m.Unavailable = false
			m.Poe = true
		}
	}
	if p.ContextWindow > 0 {
		m.ContextWindow = p.ContextWindow
	}
	if p.Tier != "" {
		m.Modes = []string{p.Tier}
	}
	return m
}
func poeModelURL(m router.Model) string {
	handle := m.ResolveAPIModelID()
	if _, tail, ok := strings.Cut(handle, "/"); ok {
		handle = tail
	}
	if !poeHandlePattern.MatchString(handle) {
		return ""
	}
	return fmt.Sprintf("https://poe.com/%s", handle)
}
