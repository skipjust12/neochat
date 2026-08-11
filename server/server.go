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
	"neochat/costlog"
	"neochat/idempotency"
	"neochat/limits"
	"neochat/moderation"
	"neochat/provider"
	"neochat/router"
	"neochat/summarizer"
	"neochat/tokenizer"
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

	// CostLog persists one costlog.Store record per completed generation
	// (real token usage, not the pre-flight estimate) -- see README
	// pre-launch checklist item 7 ("per-request cost accounting").
	// Distinct from Store above: Store accumulates spend into rolling
	// windows for the Thinking+Max cap (limits.CheckThinkingMaxLock);
	// CostLog keeps one row per request for billing reconciliation, unit-
	// economics sanity checks, and eval-set cost tracking -- the same
	// underlying numbers, different shape, both derived from the same
	// finalize call. Required, not nil-safe like Idempotency: every
	// successful generation logs a cost record unconditionally, there is
	// no per-request opt-out.
	CostLog costlog.Store

	// Idempotency dedups retries so a client that never saw a response
	// (dropped connection, timeout) can safely resend the same request
	// without triggering a second generate call and a second charge --
	// see README pre-launch checklist item 3. Keyed off chatRequest's
	// IdempotencyKey field; nil (the zero value) disables the guard
	// entirely, so existing callers that don't set this field are
	// unaffected. See reserveIdempotent/finishIdempotent.
	Idempotency idempotency.Store

	// Generators maps a catalog Model.Provider string (e.g. "openai",
	// "anthropic") to the client that can actually call it. A model whose
	// provider has no entry here can be selected by the router but not
	// generated from -- see handleChat's error path.
	Generators map[string]provider.Client

	// SystemPrompts maps a persona name (one of SystemPromptNames -- e.g.
	// "Default", "Expert") to the prompt text prepended as a "system"
	// message ahead of conversation history and the new user message on
	// every generation call (see prepare) -- the prompt for the model
	// actually answering the user, distinct from Classifier.SystemPrompt
	// and Moderator.SystemPrompt, which are their own cheap-model calls.
	// A persona with no entry (prompt not written yet -- see
	// LoadSystemPrompts) sends no system message at all when selected.
	SystemPrompts map[string]string

	// Summarizer folds the older part of a long conversation's history
	// into a rolling text summary -- see maybeSummarize. Closes the
	// "Context window mismatch" gap (README): without it, a conversation
	// that outgrows every catalog model's ContextWindow simply stops
	// routing.
	Summarizer summarizer.Summarizer

	// SummaryTriggerTokens is the tokenizer.EstimateMessages threshold
	// (evaluated against the full stored history) above which prepare
	// summarizes the older part of the conversation instead of sending it
	// in full. Typically set below the smallest catalog model's
	// ContextWindow, so summarization kicks in before applyHardFilters
	// would reject every model -- see cmd/server/main.go for how the
	// default is derived. <= 0 (the zero value) disables summarization
	// entirely -- prepare sends full history exactly as it did before
	// this feature existed.
	SummaryTriggerTokens int

	// SummaryTailMessages is how many of the most recent messages are
	// always sent verbatim, never folded into the summary -- keeps the
	// immediate back-and-forth the model is actively reasoning about
	// intact even once older turns have been compressed.
	SummaryTailMessages int
}

// chatRequest is the wire format for POST /chat.
type chatRequest struct {
	UserID string `json:"user_id"`
	PlanID string `json:"plan_id"`

	// ConversationID threads this request onto an existing conversation's
	// stored history (see conversation/). Empty starts a new one --
	// handle generates an ID and returns it in chatResponse so the client
	// can pass it back on the next turn.
	ConversationID string `json:"conversation_id,omitempty"`
	Message        string `json:"message"`
	RequestedMode  string `json:"requested_mode"` // "auto" | "instant" | "thinking" | "max" | "manual"
	ManualModelID  string `json:"manual_model_id,omitempty"`

	// EstimatedContextTokens is accepted for wire-format backward
	// compatibility but no longer used: server.prepare now computes the
	// real figure server-side via tokenizer.EstimateMessages, from the
	// actual system prompt + stored history + new message about to be
	// sent, instead of trusting whatever number the client supplies here.
	// A client-supplied estimate would either go stale as a conversation
	// grows (README "Context window mismatch") or could be understated on
	// purpose to slip a request under a spend cap (README "Cost-based
	// abuse") -- deriving it from the real request body closes both.
	EstimatedContextTokens int `json:"estimated_context_tokens,omitempty"`

	// IdempotencyKey, if set, makes retrying this exact request safe: a
	// second call with the same (user_id, idempotency_key) replays the
	// first attempt's response instead of generating (and billing) again.
	// The client is responsible for reusing the same key on a retry and
	// picking a new one for a genuinely new message -- typically a UUID
	// generated once per user-initiated send. Empty disables the guard for
	// that request (no dedup, same behavior as before this field existed).
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// Persona picks which of SystemPromptNames' system prompts to send
	// with this request (e.g. "Expert", "Cynical") -- the UI's tone/style
	// picker once it exists. Empty means "Default". A non-empty value not
	// in SystemPromptNames is a 400, same as an unknown plan_id.
	Persona string `json:"persona,omitempty"`
}

// defaultPersona is the persona used when a request doesn't specify one.
const defaultPersona = "Default"

// isValidPersona reports whether name is one of SystemPromptNames.
// Validation is against this fixed list, not against which personas
// currently have loaded prompt text -- a persona whose file is still an
// empty placeholder (see LoadSystemPrompts) is a legitimate selection
// that just sends no system message, not a client error.
func isValidPersona(name string) bool {
	for _, n := range SystemPromptNames {
		if n == name {
			return true
		}
	}
	return false
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
	mux.HandleFunc("POST /chat/stream", s.handleChatStream)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// decodeChatRequest parses and validates the POST /chat and POST
// /chat/stream request bodies -- both endpoints run the exact same
// pipeline (see handle/handleStream) and differ only in how the result is
// delivered, so their input handling is shared here rather than
// duplicated per handler.
func decodeChatRequest(w http.ResponseWriter, r *http.Request, plans map[string]limits.PlanLimits) (chatRequest, limits.PlanLimits, bool) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return chatRequest{}, limits.PlanLimits{}, false
	}
	if req.UserID == "" || req.Message == "" || req.PlanID == "" {
		http.Error(w, "user_id, plan_id, and message are required", http.StatusBadRequest)
		return chatRequest{}, limits.PlanLimits{}, false
	}
	plan, ok := plans[req.PlanID]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown plan_id %q", req.PlanID), http.StatusBadRequest)
		return chatRequest{}, limits.PlanLimits{}, false
	}
	if req.Persona != "" && !isValidPersona(req.Persona) {
		http.Error(w, fmt.Sprintf("unknown persona %q (want one of %v)", req.Persona, SystemPromptNames), http.StatusBadRequest)
		return chatRequest{}, limits.PlanLimits{}, false
	}
	return req, plan, true
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	req, plan, ok := decodeChatRequest(w, r, s.Plans)
	if !ok {
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

// handleChatStream is /chat's streaming counterpart: same request shape
// and the same underlying pipeline (see handleStream), but the response is
// delivered as a Server-Sent Events stream instead of one JSON body --
// this is what lets a client render the reply as it's generated instead
// of waiting for the whole thing. Event types (see writeSSEEvent calls in
// handleStream):
//   - "meta": routing decided a model, generation is about to start --
//     {conversation_id, selected_model_id, selected_mode, estimated_cost_usd}.
//     Can be sent more than once per request if a circuit-open failover
//     re-routes mid-request; the last one sent is the model that actually
//     answered.
//   - "delta": one incremental text chunk -- {text}.
//   - "done": generation finished successfully -- the full chatResponse.
//   - "blocked": Layer 1 moderation flagged the request -- the full
//     chatResponse (Blocked=true), no "meta"/"delta" ever preceded it.
//   - "error": the request failed -- {message}.
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	req, plan, ok := decodeChatRequest(w, r, s.Plans)
	if !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func(event string, payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			log.Printf("server: /chat/stream marshal %s event: %v", event, err)
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}

	if err := s.handleStream(r.Context(), req, plan, send); err != nil {
		log.Printf("server: /chat/stream error for user_id=%s: %v", req.UserID, err)
		send("error", map[string]string{"message": err.Error()})
	}
}

// preparedRequest holds everything shared between handle and handleStream
// once classification, moderation, and history loading have all run --
// only how the actual generation call is made (and its result delivered)
// differs between the two.
type preparedRequest struct {
	conversationID string
	classified     router.ClassifierOutput
	locked         bool
	messages       []provider.Message

	// estimatedContextTokens is tokenizer.EstimateMessages(messages) --
	// computed server-side from what's actually about to be sent, not
	// taken from chatRequest.EstimatedContextTokens (see that field's doc
	// comment for why the client-supplied number is no longer trusted).
	estimatedContextTokens int

	// requestID identifies this one generation attempt for
	// costlog.Store.Record (see finalize) -- distinct from conversationID
	// (spans every turn of a thread) and chatRequest.IdempotencyKey
	// (client-supplied, optional, for retry dedup). Generated fresh per
	// prepare call the same way conversation.NewID mints one: 16 random
	// bytes, hex-encoded.
	requestID string
}

// prepare runs classify + moderate (concurrently) -> check spend lock ->
// load conversation history, the sequence both handle and handleStream
// need before they can route and generate. If Layer 1 moderation flags
// the request, blocked is non-nil and the caller must return it as-is
// without running anything below (no route, no generate).
func (s *Server) prepare(ctx context.Context, req chatRequest, plan limits.PlanLimits) (prepared preparedRequest, blocked *chatResponse, err error) {
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
		return preparedRequest{}, nil, fmt.Errorf("moderate: %w", modErr)
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
		return preparedRequest{}, &chatResponse{ConversationID: conversationID, Blocked: true, ResponseText: tosViolationMessage}, nil
	}
	if classifyErr != nil {
		return preparedRequest{}, nil, fmt.Errorf("classify: %w", classifyErr)
	}

	locked, err := limits.CheckThinkingMaxLock(ctx, s.Store, plan, req.UserID)
	if err != nil {
		return preparedRequest{}, nil, fmt.Errorf("check thinking_max lock: %w", err)
	}

	// Send the model everything stored for this conversation so far, plus
	// the new message -- without this, /chat could never hold more than a
	// single-turn exchange.
	history, err := s.Conversations.History(ctx, req.UserID, conversationID)
	if err != nil {
		return preparedRequest{}, nil, fmt.Errorf("load conversation history: %w", err)
	}

	// Fold the older part of a long conversation into a rolling summary
	// once the full history gets big enough that sending it verbatim
	// risks outgrowing every catalog model's ContextWindow. summaryText
	// is "" when there's nothing to summarize yet (short conversation) or
	// summarization failed -- either way tail equals history unchanged,
	// so the rest of prepare behaves exactly as before this feature
	// existed.
	tail, summaryText := s.maybeSummarize(ctx, req.UserID, conversationID, history)

	persona := req.Persona
	if persona == "" {
		persona = defaultPersona
	}
	systemPrompt := s.SystemPrompts[persona] // "" for a valid-but-not-yet-written persona -- see LoadSystemPrompts

	messages := make([]provider.Message, 0, len(tail)+3)
	if systemPrompt != "" {
		messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	}
	if summaryText != "" {
		messages = append(messages, provider.Message{Role: "system", Content: "Earlier in this conversation:\n" + summaryText})
	}
	for _, m := range tail {
		messages = append(messages, provider.Message{Role: string(m.Role), Content: m.Content})
	}
	messages = append(messages, provider.Message{Role: "user", Content: req.Message})

	// tokenizer.EstimateMessages replaces chatRequest.EstimatedContextTokens
	// as the number Route actually filters/prices against -- computed from
	// the real message list (system prompt + summary/tail history + the
	// new message), so it grows with the conversation instead of staying
	// whatever the client declared on turn 1 (README "Context window
	// mismatch"), and can't be understated by a client trying to slip a
	// request under a spend cap (README "Cost-based abuse"). Once
	// maybeSummarize starts folding old turns into a summary, this stays
	// far smaller than the conversation's full stored history.
	return preparedRequest{
		conversationID:         conversationID,
		classified:             classified,
		locked:                 locked,
		messages:               messages,
		estimatedContextTokens: tokenizer.EstimateMessages(messages),
		requestID:              conversation.NewID(),
	}, nil, nil
}

// maybeSummarize decides whether history is small enough to send as-is,
// or whether its older part should be folded into a rolling summary
// first. It returns the messages that should actually be sent verbatim
// (either the whole of history, or just its most recent
// SummaryTailMessages) and the summary text to inject alongside them
// ("" if no summarization applies).
//
// Failure to summarize (store error or the summarizer model call
// failing) is not fatal: it logs and falls back to sending history
// unchanged, exactly as if SummaryTriggerTokens had not been reached --
// worst case a very large conversation still hits applyHardFilters'
// context-window check, the same outcome as before this feature existed.
func (s *Server) maybeSummarize(ctx context.Context, userID, conversationID string, history []conversation.Message) ([]conversation.Message, string) {
	// SummaryTriggerTokens <= 0 means the feature is unconfigured (the
	// zero-value Server, e.g. in tests that don't set it) rather than
	// "summarize everything" -- a real deployment always sets a positive
	// threshold, see cmd/server/main.go.
	if s.SummaryTriggerTokens <= 0 {
		return history, ""
	}
	if len(history) <= s.SummaryTailMessages {
		return history, ""
	}

	historyAsMessages := make([]provider.Message, len(history))
	for i, m := range history {
		historyAsMessages[i] = provider.Message{Role: string(m.Role), Content: m.Content}
	}
	if tokenizer.EstimateMessages(historyAsMessages) < s.SummaryTriggerTokens {
		return history, ""
	}

	state, err := s.Conversations.GetSummary(ctx, userID, conversationID)
	if err != nil {
		log.Printf("server: get summary for user_id=%s conversation_id=%s: %v", userID, conversationID, err)
		return history, ""
	}

	tailStart := len(history) - s.SummaryTailMessages
	if state.CoversThrough > tailStart {
		// Stored state predates a shorter SummaryTailMessages/history
		// shrink than currently configured -- clamp rather than slice
		// with a negative-length range below.
		state.CoversThrough = tailStart
	}

	toFold := history[state.CoversThrough:tailStart]
	summaryText := state.Text
	if len(toFold) > 0 {
		newSummary, err := s.Summarizer.Summarize(ctx, state.Text, toFold)
		if err != nil {
			log.Printf("server: summarize user_id=%s conversation_id=%s: %v", userID, conversationID, err)
			return history, ""
		}
		summaryText = newSummary
		if err := s.Conversations.SetSummary(ctx, userID, conversationID, conversation.Summary{
			Text:          summaryText,
			CoversThrough: tailStart,
			UpdatedAt:     time.Now(),
		}); err != nil {
			log.Printf("server: set summary for user_id=%s conversation_id=%s: %v", userID, conversationID, err)
		}
	}

	return history[tailStart:], summaryText
}

// generateCaller performs one generation call against gen -- either
// gen.Generate directly (handle) or a wrapper that streams through
// gen.(provider.StreamingClient) and reports each delta as it arrives
// (handleStream). routeAndCall is agnostic to which; it only needs the
// final GenerateResult (or error) back.
type generateCaller func(ctx context.Context, gen provider.Client, apiModelID string, messages []provider.Message) (provider.GenerateResult, error)

// directGenerate is the generateCaller handle uses: gen.Generate, no
// streaming involved.
func directGenerate(ctx context.Context, gen provider.Client, apiModelID string, messages []provider.Message) (provider.GenerateResult, error) {
	return gen.Generate(ctx, apiModelID, messages)
}

// streamingGenerate builds the generateCaller handleStream uses: it
// streams through gen's provider.StreamingClient, invoking onDelta for
// every text chunk as it arrives, and returns the stream's final
// GenerateResult once the vendor signals it's done.
func streamingGenerate(onDelta func(delta string)) generateCaller {
	return func(ctx context.Context, gen provider.Client, apiModelID string, messages []provider.Message) (provider.GenerateResult, error) {
		streamingClient, ok := gen.(provider.StreamingClient)
		if !ok {
			return provider.GenerateResult{}, fmt.Errorf("provider %T does not support streaming", gen)
		}
		ch, err := streamingClient.GenerateStream(ctx, apiModelID, messages)
		if err != nil {
			return provider.GenerateResult{}, err
		}
		for chunk := range ch {
			if chunk.Err != nil {
				return provider.GenerateResult{}, chunk.Err
			}
			if chunk.Delta != "" {
				onDelta(chunk.Delta)
			}
			if chunk.Done {
				return chunk.Final, nil
			}
		}
		return provider.GenerateResult{}, fmt.Errorf("provider: stream closed without a final chunk")
	}
}

// routeAndCall picks a model via s.Router.Route and calls it through call,
// retrying with the failed model excluded whenever call returns a
// *provider.CircuitOpenError (see provider.CircuitBreakerClient), bounded
// by maxCircuitFailoverAttempts so a catalog with every candidate's
// circuit open still fails instead of looping. Shared by handle (call =
// directGenerate) and handleStream (call = streamingGenerate(...)) -- the
// routing/failover policy is identical either way, only how the model is
// actually invoked differs.
//
// onRoute, if non-nil, is invoked with each successful Route result right
// before that attempt's call -- handleStream uses this to emit its "meta"
// event as soon as a model is picked, without waiting for generation to
// finish. It fires again on every failover retry, so a client watching
// "meta" events always sees the model an in-flight attempt is actually
// using.
func (s *Server) routeAndCall(ctx context.Context, req chatRequest, prepared preparedRequest, onRoute func(router.RouteResult), call generateCaller) (router.RouteResult, router.Model, provider.GenerateResult, error) {
	excludedModelIDs := map[string]bool{}
	for attempt := 0; ; attempt++ {
		result, err := s.Router.Route(prepared.classified, req.RequestedMode, req.ManualModelID, prepared.estimatedContextTokens, prepared.locked, excludedModelIDs)
		if err != nil {
			return router.RouteResult{}, router.Model{}, provider.GenerateResult{}, fmt.Errorf("route: %w", err)
		}
		if onRoute != nil {
			onRoute(result)
		}

		model, ok := s.Router.Catalog.FindModel(result.SelectedModelID)
		if !ok {
			return router.RouteResult{}, router.Model{}, provider.GenerateResult{}, fmt.Errorf("selected model %q not found in catalog", result.SelectedModelID)
		}
		gen, ok := s.Generators[model.Provider]
		if !ok {
			return router.RouteResult{}, router.Model{}, provider.GenerateResult{}, fmt.Errorf("no provider.Client configured for provider %q (model %q)", model.Provider, model.ID)
		}

		genResult, err := call(ctx, gen, model.ResolveAPIModelID(), prepared.messages)
		if err == nil {
			return result, model, genResult, nil
		}

		var circuitErr *provider.CircuitOpenError
		if !errors.As(err, &circuitErr) || attempt >= maxCircuitFailoverAttempts-1 {
			return router.RouteResult{}, router.Model{}, provider.GenerateResult{}, fmt.Errorf("generate: %w", err)
		}
		log.Printf("server: circuit open for model_id=%s (api_model_id=%s), rerouting: %v", result.SelectedModelID, model.ResolveAPIModelID(), err)
		excludedModelIDs[result.SelectedModelID] = true
	}
}

// finalize persists the turn, records actual spend, and builds the
// response chatResponse/handleStream's "done" event share. Best-effort on
// persistence: generation already succeeded and the user already has
// their answer, so a persistence failure here shouldn't turn into a
// request failure -- it just means this turn won't be there for History
// on the next one. Same reasoning as prepare's ModerationLog handling.
func (s *Server) finalize(ctx context.Context, req chatRequest, prepared preparedRequest, result router.RouteResult, model router.Model, genResult provider.GenerateResult) (chatResponse, error) {
	now := time.Now()
	if err := s.Conversations.Append(ctx, req.UserID, prepared.conversationID, conversation.Message{Role: conversation.RoleUser, Content: req.Message, CreatedAt: now}); err != nil {
		log.Printf("server: failed to persist user message for conversation_id=%s: %v", prepared.conversationID, err)
	}
	if err := s.Conversations.Append(ctx, req.UserID, prepared.conversationID, conversation.Message{Role: conversation.RoleAssistant, Content: genResult.Text, ModelID: model.ID, CreatedAt: now}); err != nil {
		log.Printf("server: failed to persist assistant message for conversation_id=%s: %v", prepared.conversationID, err)
	}

	actualCost := router.ComputeCostUSD(model, genResult.InputTokens, genResult.OutputTokens)
	var err error
	if result.SelectedMode == "instant" {
		err = limits.RecordInstantSpend(ctx, s.Store, req.UserID, actualCost, now)
	} else {
		err = limits.RecordThinkingMaxSpend(ctx, s.Store, req.UserID, actualCost, now)
	}
	if err != nil {
		return chatResponse{}, fmt.Errorf("record spend: %w", err)
	}

	// Best-effort like Conversations.Append above: the request already
	// succeeded and the user already has their answer, so a logging
	// failure here shouldn't fail the request -- it just means this one
	// request is missing from the per-request cost log (README pre-launch
	// checklist item 7), not from the rolling-window spend Store above,
	// which is what actually gates the Thinking+Max cap.
	entry := router.NewCostLogEntry(req.UserID, prepared.requestID, model, result.SelectedMode, genResult.InputTokens, genResult.OutputTokens, now)
	if err := s.CostLog.Record(ctx, entry); err != nil {
		log.Printf("server: failed to record cost log entry for request_id=%s: %v", prepared.requestID, err)
	}

	return chatResponse{
		ConversationID:   prepared.conversationID,
		SelectedModelID:  result.SelectedModelID,
		SelectedMode:     result.SelectedMode,
		Reason:           result.Reason,
		EstimatedCostUSD: result.EstimatedCostUSD,
		ActualCostUSD:    actualCost,
		ResponseText:     genResult.Text,
	}, nil
}

// reserveIdempotent claims req's idempotency key, if it has one and
// s.Idempotency is configured. It returns (cached response, true, nil) if
// this exact (user_id, idempotency_key) already completed -- the caller
// must return that instead of doing any work (no re-classify, re-moderate,
// re-generate, or re-bill). It returns (chatResponse{}, false, nil) if this
// is a genuinely new attempt that should proceed normally, in which case
// the caller must report its outcome back via finishIdempotent. A request
// with no IdempotencyKey (or a Server with no Idempotency store configured)
// always takes this second path -- the guard is opt-in per request.
func (s *Server) reserveIdempotent(ctx context.Context, req chatRequest) (chatResponse, bool, error) {
	if req.IdempotencyKey == "" || s.Idempotency == nil {
		return chatResponse{}, false, nil
	}

	rec, found, err := s.Idempotency.Reserve(ctx, req.UserID, req.IdempotencyKey)
	if err != nil {
		if errors.Is(err, idempotency.ErrInFlight) {
			return chatResponse{}, false, fmt.Errorf("a request with idempotency_key %q is already in progress for this user", req.IdempotencyKey)
		}
		return chatResponse{}, false, fmt.Errorf("idempotency reserve: %w", err)
	}
	if !found {
		return chatResponse{}, false, nil
	}

	var resp chatResponse
	if err := json.Unmarshal(rec.Response, &resp); err != nil {
		return chatResponse{}, false, fmt.Errorf("idempotency: decode cached response for idempotency_key %q: %w", req.IdempotencyKey, err)
	}
	return resp, true, nil
}

// finishIdempotent reports the outcome of an attempt reserveIdempotent
// claimed. A successful response is cached so a later retry with the same
// key replays it instead of generating (and billing) again. A failed
// attempt instead releases the key, so a legitimate retry after a real
// failure (as opposed to a dropped connection after success) isn't
// permanently stuck behind ErrInFlight. No-op under the same conditions
// reserveIdempotent short-circuits on (no key, or no store configured).
func (s *Server) finishIdempotent(ctx context.Context, req chatRequest, resp chatResponse, handleErr error) {
	if req.IdempotencyKey == "" || s.Idempotency == nil {
		return
	}

	if handleErr != nil {
		if err := s.Idempotency.Release(ctx, req.UserID, req.IdempotencyKey); err != nil {
			log.Printf("server: failed to release idempotency_key=%s for user_id=%s: %v", req.IdempotencyKey, req.UserID, err)
		}
		return
	}

	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("server: failed to encode idempotent response for idempotency_key=%s: %v", req.IdempotencyKey, err)
		if relErr := s.Idempotency.Release(ctx, req.UserID, req.IdempotencyKey); relErr != nil {
			log.Printf("server: failed to release idempotency_key=%s for user_id=%s: %v", req.IdempotencyKey, req.UserID, relErr)
		}
		return
	}
	if err := s.Idempotency.Complete(ctx, req.UserID, req.IdempotencyKey, idempotency.Record{Response: data}); err != nil {
		log.Printf("server: failed to complete idempotency_key=%s for user_id=%s: %v", req.IdempotencyKey, req.UserID, err)
	}
}

// handle runs the full non-streaming sequence: reserve the idempotency key
// (if any) -> prepare (classify + moderate concurrently -> check spend lock
// -> load history) -> route + generate (with circuit-open failover) ->
// finalize (persist + record spend) -> report the outcome back to the
// idempotency store. Split out from handleChat so tests can call it
// directly with a fixed request/plan and inspect the typed result instead
// of parsing HTTP output.
func (s *Server) handle(ctx context.Context, req chatRequest, plan limits.PlanLimits) (chatResponse, error) {
	if cached, found, err := s.reserveIdempotent(ctx, req); err != nil {
		return chatResponse{}, err
	} else if found {
		return cached, nil
	}

	resp, err := s.handleOnce(ctx, req, plan)
	s.finishIdempotent(ctx, req, resp, err)
	return resp, err
}

// handleOnce is handle's actual work, run at most once per idempotency key
// -- see handle's wrapping via reserveIdempotent/finishIdempotent.
func (s *Server) handleOnce(ctx context.Context, req chatRequest, plan limits.PlanLimits) (chatResponse, error) {
	prepared, blocked, err := s.prepare(ctx, req, plan)
	if err != nil {
		return chatResponse{}, err
	}
	if blocked != nil {
		return *blocked, nil
	}

	result, model, genResult, err := s.routeAndCall(ctx, req, prepared, nil, directGenerate)
	if err != nil {
		return chatResponse{}, err
	}

	return s.finalize(ctx, req, prepared, result, model, genResult)
}

// handleStream is handle's streaming counterpart: identical pipeline
// (including the same idempotency guard, see handle), but text deltas are
// pushed to send("delta", ...) as they arrive instead of being buffered
// into one final response. send is also used for the "meta" event (once
// routing picks a model, before generation starts) and the terminal
// "done"/"blocked" event -- see handleChatStream's doc comment for the
// full event contract. A cached idempotent replay skips straight to
// "done"/"blocked" -- no "meta"/"delta" events, since no generation
// actually happens on a replay. Split out from handleChatStream the same
// way handle is split from handleChat, for the same reason: testable
// without parsing SSE wire output.
func (s *Server) handleStream(ctx context.Context, req chatRequest, plan limits.PlanLimits, send func(event string, payload any)) error {
	if cached, found, err := s.reserveIdempotent(ctx, req); err != nil {
		return err
	} else if found {
		if cached.Blocked {
			send("blocked", cached)
		} else {
			send("done", cached)
		}
		return nil
	}

	resp, err := s.handleStreamOnce(ctx, req, plan, send)
	s.finishIdempotent(ctx, req, resp, err)
	return err
}

// handleStreamOnce is handleStream's actual work, run at most once per
// idempotency key -- see handleStream's wrapping via
// reserveIdempotent/finishIdempotent.
func (s *Server) handleStreamOnce(ctx context.Context, req chatRequest, plan limits.PlanLimits, send func(event string, payload any)) (chatResponse, error) {
	prepared, blocked, err := s.prepare(ctx, req, plan)
	if err != nil {
		return chatResponse{}, err
	}
	if blocked != nil {
		send("blocked", *blocked)
		return *blocked, nil
	}

	call := streamingGenerate(func(delta string) {
		send("delta", map[string]string{"text": delta})
	})
	onRoute := func(result router.RouteResult) {
		send("meta", map[string]any{
			"conversation_id":    prepared.conversationID,
			"selected_model_id":  result.SelectedModelID,
			"selected_mode":      result.SelectedMode,
			"estimated_cost_usd": result.EstimatedCostUSD,
		})
	}

	result, model, genResult, err := s.routeAndCall(ctx, req, prepared, onRoute, call)
	if err != nil {
		return chatResponse{}, err
	}

	resp, err := s.finalize(ctx, req, prepared, result, model, genResult)
	if err != nil {
		return chatResponse{}, err
	}
	send("done", resp)
	return resp, nil
}
