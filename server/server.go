// Package server wires classifier, moderation, router, limits, and
// provider together behind one HTTP endpoint -- the "actual generation
// pipeline" the rest of the repo has been built to sit in front of (see
// README "Next steps").
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"neochat/classifier"
	"neochat/conversation"
	"neochat/limits"
	"neochat/moderation"
	"neochat/provider"
	"neochat/router"
)

// maxCircuitFailoverAttempts bounds how many times handle will re-route
// around a model whose circuit just opened before giving up. Each retry
// excludes one more model, so this is also the max number of distinct
// models a single request will try.
const maxCircuitFailoverAttempts = 3

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
	Conversations conversation.Store
	Store         limits.SpendStore
	Plans         map[string]limits.PlanLimits

	// Generators maps a catalog Model.Provider string (e.g. "openai",
	// "anthropic") to the client that can actually call it. A model whose
	// provider has no entry here can be selected by the router but not
	// generated from -- see handleChat's error path.
	Generators map[string]provider.Client

	// SystemPrompt is prepended as a "system" message ahead of
	// conversation history and the new user message on every generation
	// call (see handle) -- the prompt for the model actually answering
	// the user, distinct from Classifier.SystemPrompt and
	// Moderator.SystemPrompt, which are their own cheap-model calls.
	// Empty means no system message is sent at all, so this field can be
	// left unset until a real prompt exists (see LoadSystemPrompt).
	SystemPrompt string
}

// chatRequest is the wire format for POST /chat.
type chatRequest struct {
	UserID string `json:"user_id"`
	PlanID string `json:"plan_id"`

	// ConversationID threads this request onto an existing conversation's
	// stored history (see conversation/). Empty starts a new one --
	// handle generates an ID and returns it in chatResponse so the client
	// can pass it back on the next turn.
	ConversationID         string `json:"conversation_id,omitempty"`
	Message                string `json:"message"`
	RequestedMode          string `json:"requested_mode"` // "auto" | "instant" | "thinking" | "max" | "manual"
	ManualModelID          string `json:"manual_model_id,omitempty"`
	EstimatedContextTokens int    `json:"estimated_context_tokens"`
}

type chatResponse struct {
	// ConversationID is always the conversation this turn belongs to --
	// either the one the request supplied, or a freshly generated one if
	// it didn't. Present even on a Blocked response, so a client can
	// retry in the same thread after rephrasing.
	ConversationID   string  `json:"conversation_id"`
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
	conversationID := req.ConversationID
	if conversationID == "" {
		conversationID = conversation.NewID()
	}

	// Layer 1 moderation (README "Moderation") runs concurrently with
	// classification -- both are cheap-model calls on the same raw
	// request text, so there's no reason to pay their latency twice. This
	// repo has no streaming response pipeline yet, so it skips the
	// documented "generate concurrently into a buffer, discard on flag"
	// optimization: without streaming, generating before moderation
	// clears would only ever waste money, never save user-visible
	// latency, so generation simply waits for both results below instead.
	//
	// Both only ever look at req.Message, never at conversation history --
	// a deliberate limitation, not an oversight: moderating/classifying
	// full history on every turn multiplies their cost by conversation
	// length for comparatively little benefit on the common case (a
	// violation is usually in the newest message, not buried in an
	// otherwise-fine history). Revisit if that assumption turns out
	// wrong in practice.
	//
	// Moderation failing (as opposed to flagging) fails the whole request
	// closed rather than silently letting an unmoderated message through
	// -- an outage of the moderation model becomes an outage of chat.
	// provider.CircuitBreakerClient (README pre-launch checklist) exists
	// but currently only wraps generation calls (see cmd/server/main.go),
	// not this one -- extending it here would make a softer fallback
	// safe to add.
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
		return chatResponse{ConversationID: conversationID, Blocked: true, ResponseText: tosViolationMessage}, nil
	}
	if classifyErr != nil {
		return chatResponse{}, fmt.Errorf("classify: %w", classifyErr)
	}

	locked, err := limits.CheckThinkingMaxLock(ctx, s.Store, plan, req.UserID)
	if err != nil {
		return chatResponse{}, fmt.Errorf("check thinking_max lock: %w", err)
	}

	// Send the model everything stored for this conversation so far, plus
	// the new message -- without this, /chat could never hold more than a
	// single-turn exchange. estimated_context_tokens is still whatever
	// the client supplied, though: this repo has no tokenizer to
	// recompute it from real history length, so the router's context-
	// window hard filter and cost estimate can undercount once a
	// conversation has grown a real history (README "Context window
	// mismatch" risk) -- unchanged by this, just now actually exercised.
	history, err := s.Conversations.History(ctx, req.UserID, conversationID)
	if err != nil {
		return chatResponse{}, fmt.Errorf("load conversation history: %w", err)
	}
	messages := make([]provider.Message, 0, len(history)+2)
	if s.SystemPrompt != "" {
		messages = append(messages, provider.Message{Role: "system", Content: s.SystemPrompt})
	}
	for _, m := range history {
		messages = append(messages, provider.Message{Role: string(m.Role), Content: m.Content})
	}
	messages = append(messages, provider.Message{Role: "user", Content: req.Message})

	// Route, then generate; on a CircuitOpenError (the model's circuit just
	// tripped -- see provider.CircuitBreakerClient) retry with that model
	// excluded so Route picks a different, healthy candidate instead of
	// failing the whole request. Bounded by maxCircuitFailoverAttempts so a
	// catalog with every candidate's circuit open still fails instead of
	// looping.
	var (
		result    router.RouteResult
		model     router.Model
		genResult provider.GenerateResult
	)
	excludedModelIDs := map[string]bool{}
	for attempt := 0; ; attempt++ {
		result, err = s.Router.Route(classified, req.RequestedMode, req.ManualModelID, req.EstimatedContextTokens, locked, excludedModelIDs)
		if err != nil {
			return chatResponse{}, fmt.Errorf("route: %w", err)
		}

		var ok bool
		model, ok = s.Router.Catalog.FindModel(result.SelectedModelID)
		if !ok {
			return chatResponse{}, fmt.Errorf("selected model %q not found in catalog", result.SelectedModelID)
		}
		gen, ok := s.Generators[model.Provider]
		if !ok {
			return chatResponse{}, fmt.Errorf("no provider.Client configured for provider %q (model %q)", model.Provider, model.ID)
		}

		genResult, err = gen.Generate(ctx, model.ResolveAPIModelID(), messages)
		if err == nil {
			break
		}

		var circuitErr *provider.CircuitOpenError
		if !errors.As(err, &circuitErr) || attempt >= maxCircuitFailoverAttempts-1 {
			return chatResponse{}, fmt.Errorf("generate: %w", err)
		}
		log.Printf("server: /chat circuit open for model_id=%s (api_model_id=%s), rerouting: %v", result.SelectedModelID, model.ResolveAPIModelID(), err)
		excludedModelIDs[result.SelectedModelID] = true
	}

	// Best-effort: generation already succeeded and the user already has
	// their answer, so a persistence failure here shouldn't turn into a
	// request failure -- it just means this turn won't be there for
	// History on the next one. Same reasoning as ModerationLog above.
	now := time.Now()
	if err := s.Conversations.Append(ctx, req.UserID, conversationID, conversation.Message{Role: conversation.RoleUser, Content: req.Message, CreatedAt: now}); err != nil {
		log.Printf("server: failed to persist user message for conversation_id=%s: %v", conversationID, err)
	}
	if err := s.Conversations.Append(ctx, req.UserID, conversationID, conversation.Message{Role: conversation.RoleAssistant, Content: genResult.Text, ModelID: model.ID, CreatedAt: now}); err != nil {
		log.Printf("server: failed to persist assistant message for conversation_id=%s: %v", conversationID, err)
	}

	actualCost := router.ComputeCostUSD(model, genResult.InputTokens, genResult.OutputTokens)
	if result.SelectedMode == "instant" {
		err = limits.RecordInstantSpend(ctx, s.Store, req.UserID, actualCost, now)
	} else {
		err = limits.RecordThinkingMaxSpend(ctx, s.Store, req.UserID, actualCost, now)
	}
	if err != nil {
		return chatResponse{}, fmt.Errorf("record spend: %w", err)
	}

	return chatResponse{
		ConversationID:   conversationID,
		SelectedModelID:  result.SelectedModelID,
		SelectedMode:     result.SelectedMode,
		Reason:           result.Reason,
		EstimatedCostUSD: result.EstimatedCostUSD,
		ActualCostUSD:    actualCost,
		ResponseText:     genResult.Text,
	}, nil
}
