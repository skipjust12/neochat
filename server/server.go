// Package server wires classifier, router, limits, and provider together
// behind one HTTP endpoint -- the "actual generation pipeline" the rest of
// the repo has been built to sit in front of (see README "Next steps").
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"neochat/classifier"
	"neochat/limits"
	"neochat/provider"
	"neochat/router"
)

// Server holds everything one /chat request needs. All fields are
// required; use New to build one with validation.
type Server struct {
	Router     router.Router
	Classifier classifier.Classifier
	Store      limits.SpendStore
	Plans      map[string]limits.PlanLimits

	// Generators maps a catalog Model.Provider string (e.g. "openai",
	// "anthropic") to the client that can actually call it. A model whose
	// provider has no entry here can be selected by the router but not
	// generated from -- see handleChat's error path.
	Generators map[string]provider.Client
}

// chatRequest is the wire format for POST /chat.
type chatRequest struct {
	UserID                 string `json:"user_id"`
	PlanID                 string `json:"plan_id"`
	Message                string `json:"message"`
	RequestedMode          string `json:"requested_mode"` // "auto" | "instant" | "thinking" | "max" | "manual"
	ManualModelID          string `json:"manual_model_id,omitempty"`
	EstimatedContextTokens int    `json:"estimated_context_tokens"`
}

type chatResponse struct {
	SelectedModelID  string  `json:"selected_model_id"`
	SelectedMode     string  `json:"selected_mode"`
	Reason           string  `json:"reason"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
	ActualCostUSD    float64 `json:"actual_cost_usd"`
	ResponseText     string  `json:"response_text"`
}

// Mux returns an http.ServeMux with routes registered.
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat", s.handleChat)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if req.UserID == "" || req.Message == "" || req.PlanID == "" {
		http.Error(w, "user_id, plan_id, and message are required", http.StatusBadRequest)
		return
	}
	plan, ok := s.Plans[req.PlanID]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown plan_id %q", req.PlanID), http.StatusBadRequest)
		return
	}

	resp, err := s.handle(r.Context(), req, plan)
	if err != nil {
		log.Printf("server: /chat error for user_id=%s: %v", req.UserID, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("server: encode response: %v", err)
	}
}

// handle runs the full sequence: classify -> check spend lock -> route ->
// generate -> record actual spend. Split out from handleChat so tests can
// call it directly with a fixed request/plan and inspect the typed result
// instead of parsing HTTP output.
func (s *Server) handle(ctx context.Context, req chatRequest, plan limits.PlanLimits) (chatResponse, error) {
	classified, err := s.Classifier.Classify(ctx, req.Message)
	if err != nil {
		return chatResponse{}, fmt.Errorf("classify: %w", err)
	}

	locked, err := limits.CheckThinkingMaxLock(ctx, s.Store, plan, req.UserID)
	if err != nil {
		return chatResponse{}, fmt.Errorf("check thinking_max lock: %w", err)
	}

	result, err := s.Router.Route(classified, req.RequestedMode, req.ManualModelID, req.EstimatedContextTokens, locked)
	if err != nil {
		return chatResponse{}, fmt.Errorf("route: %w", err)
	}

	model, ok := s.Router.Catalog.FindModel(result.SelectedModelID)
	if !ok {
		return chatResponse{}, fmt.Errorf("selected model %q not found in catalog", result.SelectedModelID)
	}
	gen, ok := s.Generators[model.Provider]
	if !ok {
		return chatResponse{}, fmt.Errorf("no provider.Client configured for provider %q (model %q)", model.Provider, model.ID)
	}

	genResult, err := gen.Generate(ctx, model.ResolveAPIModelID(), []provider.Message{
		{Role: "user", Content: req.Message},
	})
	if err != nil {
		return chatResponse{}, fmt.Errorf("generate: %w", err)
	}

	actualCost := router.ComputeCostUSD(model, genResult.InputTokens, genResult.OutputTokens)
	now := time.Now()
	if result.SelectedMode == "instant" {
		err = limits.RecordInstantSpend(ctx, s.Store, req.UserID, actualCost, now)
	} else {
		err = limits.RecordThinkingMaxSpend(ctx, s.Store, req.UserID, actualCost, now)
	}
	if err != nil {
		return chatResponse{}, fmt.Errorf("record spend: %w", err)
	}

	return chatResponse{
		SelectedModelID:  result.SelectedModelID,
		SelectedMode:     result.SelectedMode,
		Reason:           result.Reason,
		EstimatedCostUSD: result.EstimatedCostUSD,
		ActualCostUSD:    actualCost,
		ResponseText:     genResult.Text,
	}, nil
}
