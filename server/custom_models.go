package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"neochat/auth"
	"neochat/router"
)

// Only the owner can use these definitions. They are never added to the
// shared catalog or automatic routing, and contain no API credentials.
type customModel struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	URL      string `json:"url"`
	Name     string `json:"name"`
	Handle   string `json:"handle"`
}

var poeHandlePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
var customProviders = map[string]bool{"anthropic": true, "openai": true, "google": true, "spacexai": true, "deepseek": true, "moonshot": true}

func normalizeCustomModel(m customModel) (customModel, error) {
	if !customProviders[m.Provider] {
		return m, fmt.Errorf("Choose a supported provider.")
	}
	handle := strings.TrimSpace(m.URL)
	if strings.Contains(handle, "://") {
		u, err := url.Parse(handle)
		if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "poe.com") || u.User != nil || u.RawPath != "" {
			return m, fmt.Errorf("Use an https://poe.com/bot-name link or a bot name.")
		}
		handle = strings.TrimSuffix(strings.Trim(u.Path, "/"), "/api")
	}
	if !poeHandlePattern.MatchString(handle) {
		return m, fmt.Errorf("Enter a valid Poe bot name or link.")
	}
	m.Handle = handle // Case is part of the Poe API identifier.
	m.URL = "https://poe.com/" + handle
	m.Name = strings.TrimSpace(m.Name)
	if len([]rune(m.Name)) > 80 || strings.ContainsAny(m.Name, "\r\n\x00") {
		return m, fmt.Errorf("Display name must be under 80 characters on one line.")
	}
	if m.Name == "" {
		m.Name = strings.ReplaceAll(strings.ReplaceAll(handle, "-", " "), "_", " ")
		if strings.HasPrefix(handle, "GPT-") {
			m.Name = "GPT-" + strings.TrimPrefix(m.Name, "GPT ")
		}
	}
	hash := sha256.Sum256([]byte(m.Provider + "\x00" + strings.ToLower(handle)))
	m.ID = "custom-" + hex.EncodeToString(hash[:12])
	return m, nil
}
func (s *Server) customSettings(ctx context.Context, userID string) (map[string]json.RawMessage, error) {
	object := map[string]json.RawMessage{}
	if s.Settings == nil {
		return object, nil
	}
	raw, err := s.Settings.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &object); err != nil {
			return nil, err
		}
	}
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	return object, nil
}
func customDefinitions(object map[string]json.RawMessage) ([]customModel, error) {
	var models []customModel
	if raw := object["custom_models"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &models); err != nil {
			return nil, err
		}
	}
	return models, nil
}
func (s *Server) customRouterModel(m customModel) router.Model {
	// Reuse authoritative capabilities/pricing whenever this bot is listed.
	for _, known := range s.router().Catalog.Models {
		if strings.EqualFold(known.ResolveAPIModelID(), m.Handle) {
			known.ID = m.ID
			known.APIModelID = m.Handle
			known.DisplayName = m.Name
			known.Provider = m.Provider
			known.ManualOnly = true
			known.Legacy = false
			return known
		}
	}
	// An unlisted chat bot uses Poe's standard Chat endpoint, including tools.
	// Rates are reservation estimates only; Poe reports actual token usage.
	model := router.Model{ID: m.ID, APIModelID: m.Handle, DisplayName: m.Name, Provider: m.Provider, Poe: true, ManualOnly: true, Modes: []string{"max"}, ContextWindow: 128000, MaxOutputTokens: 8192, InputModalities: []string{"text", "image", "file"}, SupportsModality: []string{"text"}, SupportsOutputFormats: []string{"text", "markdown", "json", "function_call"}, ToolCalling: true, CostInputPerMTok: 5, CostOutputPerMTok: 25}
	for _, known := range s.router().Catalog.Models {
		if known.Provider == m.Provider && known.Kind == "" {
			model.CostInputPerMTok = max(model.CostInputPerMTok, known.CostInputPerMTok)
			model.CostOutputPerMTok = max(model.CostOutputPerMTok, known.CostOutputPerMTok)
		}
	}
	return model
}
func (s *Server) requestRouter(req chatRequest) router.Router {
	result := s.router()
	if req.customModel != nil {
		result.Catalog.Models = append(append([]router.Model(nil), result.Catalog.Models...), *req.customModel)
	}
	return result
}
func (s *Server) loadCustomModel(ctx context.Context, req *chatRequest) error {
	if !strings.HasPrefix(req.ManualModelID, "custom-") {
		return nil
	}
	object, err := s.customSettings(ctx, req.UserID)
	if err != nil {
		return fmt.Errorf("load custom models: %w", err)
	}
	definitions, err := customDefinitions(object)
	if err != nil {
		return err
	}
	for _, definition := range definitions {
		if definition.ID == req.ManualModelID {
			model := s.customRouterModel(definition)
			req.customModel = &model
			return nil
		}
	}
	return nil // validateRequest reports an unknown model, including another user's.
}
func (s *Server) handleCustomModels(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if s.Settings == nil {
		http.Error(w, "Settings sync is unavailable.", http.StatusServiceUnavailable)
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	object, err := s.customSettings(r.Context(), identity.UserID)
	if err != nil {
		http.Error(w, "Could not load models.", 500)
		return
	}
	definitions, err := customDefinitions(object)
	if err != nil {
		http.Error(w, "Could not load models.", 500)
		return
	}
	legacy := map[string]bool{}
	if raw := object["model_legacy"]; len(raw) > 0 {
		if json.Unmarshal(raw, &legacy) != nil {
			http.Error(w, "Could not load model preferences.", 500)
			return
		}
	}
	if legacy == nil {
		legacy = map[string]bool{}
	}
	switch r.Method {
	case http.MethodPatch:
		var input struct {
			Legacy *bool `json:"legacy"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128)).Decode(&input) != nil || input.Legacy == nil {
			http.Error(w, "Specify legacy: true or false.", 400)
			return
		}
		id := r.PathValue("id")
		_, known := s.router().Catalog.FindModel(id)
		for _, m := range definitions {
			if m.ID == id {
				known = true
			}
		}
		if !known {
			http.Error(w, "Unknown model.", 404)
			return
		}
		legacy[id] = *input.Legacy

	case http.MethodPost:
		var input customModel
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&input) != nil {
			http.Error(w, "Invalid model definition.", 400)
			return
		}
		model, err := normalizeCustomModel(input)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		replaced := false
		for i, m := range definitions {
			if m.ID == model.ID {
				definitions[i] = model
				replaced = true
				break
			}
		}
		if !replaced {
			if len(definitions) >= 50 {
				http.Error(w, "You can save up to 50 custom models.", 400)
				return
			}
			definitions = append(definitions, model)
		}
	case http.MethodDelete:
		kept := definitions[:0]
		for _, m := range definitions {
			if m.ID != r.PathValue("id") {
				kept = append(kept, m)
			}
		}
		definitions = kept
		delete(legacy, r.PathValue("id"))
	}
	if r.Method != http.MethodGet {
		object["custom_models"], _ = json.Marshal(definitions)
		object["model_legacy"], _ = json.Marshal(legacy)
		body, _ := json.Marshal(object)
		if len(body) > maxSettingsBytes {
			http.Error(w, "Settings exceed 32 KB.", 400)
			return
		}
		if err = s.Settings.Put(r.Context(), identity.UserID, body); err != nil {
			http.Error(w, "Could not save models.", 500)
			return
		}
	}
	models := router.Catalog{}
	for _, definition := range definitions {
		models.Models = append(models.Models, s.customRouterModel(definition))
	}
	encodeModels(w, modelsResponse{Models: modelEntries(models, nil, nil), LegacyOverrides: legacy})
}
