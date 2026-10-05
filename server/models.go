package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"neochat/auth"
	"neochat/modelcatalog"
	"neochat/router"
)

// The Manual picker's model list. GET /models answers from the live
// catalog (package modelcatalog), refreshing it from Polza first when it's
// older than its TTL -- so opening the app brings in models released since
// -- but waiting at most modelsWait for that: a slow Polza gets the list as
// it was. POST /models/refresh (Settings -> General -> Refresh) fetches
// right away. Polza is asked at most once per cooldown however many people
// open the app or press Refresh.

const (
	modelsWait = 4 * time.Second
	// newModelDays is how long an added model is marked "New".
	newModelDays = 14
)

type modelEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Provider    string   `json:"provider"`
	Description string   `json:"description,omitempty"`
	Tier        string   `json:"tier"`
	Kind        string   `json:"kind,omitempty"`
	Inputs      []string `json:"inputs"`
	Legacy      bool     `json:"legacy,omitempty"`
	New         bool     `json:"new,omitempty"`
	// Video is a video model's choices and price tiers, for the composer;
	// MaxReferences is how many pictures it takes (a video counts as two).
	Video         *router.VideoOptions `json:"video,omitempty"`
	MaxReferences int                  `json:"max_references,omitempty"`
}

type modelsResponse struct {
	Models    []modelEntry `json:"models"`
	UpdatedAt *time.Time   `json:"updated_at,omitempty"`
}

// router is the router over the current catalog: the live one when the
// server keeps it current, else the one it started with.
func (s *Server) router() router.Router {
	if s.Models == nil {
		return s.Router
	}
	return router.Router{Catalog: s.Models.Current().Catalog, Weights: s.Router.Weights}
}

// knowModel lets a request for a model released since the last refresh
// (picked in another tab, or right after a restart) through: an unknown
// manual model refreshes a stale catalog before the request is checked.
func (s *Server) knowModel(ctx context.Context, modelID string) {
	if s.Models == nil || modelID == "" {
		return
	}
	if _, ok := s.Models.Current().Catalog.FindModel(modelID); ok {
		return
	}
	if _, err := s.Models.Refresh(ctx, false); err != nil {
		log.Printf("server: refresh models for %q: %v", modelID, err)
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if s.Models == nil {
		writeModels(w, s.Router.Catalog, nil)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Models.Refresh(r.Context(), false); err != nil {
			log.Printf("server: refresh models: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(modelsWait):
	case <-r.Context().Done():
		return
	}
	writeModelsState(w, s.Models.Current())
}

func (s *Server) handleModelsRefresh(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if s.Models == nil {
		http.Error(w, "The model list on this server is fixed.", http.StatusServiceUnavailable)
		return
	}
	state, err := s.Models.Refresh(r.Context(), true)
	if err != nil {
		log.Printf("server: refresh models for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "Couldn't reach Polza AI for the model list. Try again in a minute.", http.StatusBadGateway)
		return
	}
	writeModelsState(w, state)
}

func writeModelsState(w http.ResponseWriter, state *modelcatalog.State) {
	var updated *time.Time
	if !state.UpdatedAt.IsZero() {
		at := state.UpdatedAt.UTC()
		updated = &at
	}
	response := modelsResponse{Models: modelEntries(state.Catalog, state.Retired, state.Added), UpdatedAt: updated}
	encodeModels(w, response)
}

func writeModels(w http.ResponseWriter, catalog router.Catalog, retired map[string]bool) {
	encodeModels(w, modelsResponse{Models: modelEntries(catalog, retired, nil)})
}

func encodeModels(w http.ResponseWriter, response modelsResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("server: encode models: %v", err)
	}
}

// modelEntries is the picker's view of a catalog, in catalog order:
// models Polza retired are left out, and models added in the last
// newModelDays are marked new.
func modelEntries(catalog router.Catalog, retired map[string]bool, added map[string]time.Time) []modelEntry {
	entries := make([]modelEntry, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		if retired[m.ID] {
			continue
		}
		inputs := m.InputModalities
		if len(inputs) == 0 {
			inputs = []string{"text"}
		}
		entry := modelEntry{ID: m.ID, Name: modelName(m), Provider: m.Provider, Tier: modelTier(m), Kind: m.Kind, Inputs: inputs, Legacy: m.Legacy}
		if m.Kind == router.KindVideo {
			entry.Video, entry.MaxReferences = m.Video, m.MaxReferenceImages
		}
		if !m.Legacy {
			entry.Description = m.Description
		}
		if at, ok := added[m.ID]; ok && time.Since(at) < newModelDays*24*time.Hour {
			entry.New = true
		}
		entries = append(entries, entry)
	}
	return entries
}

// modelTier is the highest tier a model serves.
func modelTier(m router.Model) string {
	tier := "instant"
	for _, mode := range m.Modes {
		switch {
		case mode == "max":
			return "max"
		case mode == "thinking":
			tier = "thinking"
		}
	}
	return tier
}
