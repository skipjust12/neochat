// Package server wires classifier, moderation, router, limits, and
// provider together behind one HTTP endpoint -- the "actual generation
// pipeline" the rest of the repo has been built to sit in front of (see
// README "Next steps").
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"neochat/classifier"
	"neochat/limits"
	"neochat/moderation"
	"neochat/provider"
	"neochat/router"
)

// tosViolationMessage is the only thing a moderation-flagged request's
// user ever sees -- no detail about which category tripped or why, per
// README's Moderation section ("the user sees nothing but a ToS violation
// message").
const tosViolationMessage = "This message was blocked because it violates our usage policies."

// Server holds everything one /chat request needs. All fields are
// required; use New to build one with validation.
type Server struct {
	Router        router.Router
	Classifier    classifier.Classifier
	Moderator     moderation.Moderator
	ModerationLog moderation.BlockLog
	Store         limits.SpendStore
	Plans         map[string]limits.PlanLimits

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

	// Blocked is true when Layer 1 moderation flagged the request before
	// any generation happened -- ResponseText is then tosViolationMessage,
	// not a model reply, and every other field is zero.
	Blocked bool `json:"blocked,omitempty"`
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

// handle runs the full sequence: classify + moderate (concurrently) ->
// check spend lock -> route -> generate -> record actual spend. Split out
// from handleChat so tests can call it directly with a fixed request/plan
// and inspect the typed result instead of parsing HTTP output.
func (s *Server) handle(ctx context.Context, req chatRequest, plan limits.PlanLimits) (chatResponse, error) {
	// Layer 1 moderation (README "Moderation") runs concurrently with
	// classification -- both are cheap-model calls on the same raw
	// request text, so there's no reason to pay their latency twice. This
	// repo has no streaming response pipeline yet, so it skips the
	// documented "generate concurrently into a buffer, discard on flag"
	// optimization: without streaming, generating before moderation
	// clears would only ever waste money, never save user-visible
	// latency, so generation simply waits for both results below instead.
	//
	// Moderation failing (as opposed to flagging) fails the whole request
	// closed rather than silently letting an unmoderated message through
	// -- an outage of the moderation model becomes an outage of chat,
	// which is the deliberate tradeoff until a circuit breaker (README
	// pre-launch checklist) makes a softer fallback safe to add.
	var (
		classified  router.ClassifierOutput
		classifyErr error
		modResult   moderation.Result
		modErr      error
	)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		classified, classifyErr = s.Classifier.Classify(ctx, req.Message)
	}()
	go func() {
		defer wg.Done()
		modResult, modErr = s.Moderator.Moderate(ctx, req.Message)
	}()
	wg.Wait()

	if modErr != nil {
		return chatResponse{}, fmt.Errorf("moderate: %w", modErr)
	}
	if modResult.Flagged {
		log.Printf("server: /chat blocked by moderation for user_id=%s categories=%v reason=%q", req.UserID, modResult.Categories, modResult.Reason)
		// The block itself already happened (nothing below this point
		// runs) regardless of whether it's successfully recorded --
		// logging failure is a monitoring gap, not a reason to let a
		// flagged message through.
		if err := s.ModerationLog.Record(ctx, moderation.BlockEntry{
			UserID:     req.UserID,
			Categories: modResult.Categories,
			Reason:     modResult.Reason,
			Timestamp:  time.Now(),
		}); err != nil {
			log.Printf("server: failed to record moderation block for user_id=%s: %v", req.UserID, err)
		}
		return chatResponse{Blocked: true, ResponseText: tosViolationMessage}, nil
	}
	if classifyErr != nil {
		return chatResponse{}, fmt.Errorf("classify: %w", classifyErr)
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
