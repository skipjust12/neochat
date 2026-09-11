// Package server wires classifier, moderation, router, limits, and
// provider together behind one HTTP endpoint -- the "actual generation
// pipeline" the rest of the repo has been built to sit in front of (see
// README "Next steps").
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"neochat/auth"
	"neochat/classifier"
	"neochat/conversation"
	"neochat/costlog"
	"neochat/idempotency"
	"neochat/limits"
	"neochat/moderation"
	"neochat/provider"
	"neochat/ratelimit"
	"neochat/router"
	"neochat/summarizer"
	"neochat/tokenizer"
)

//go:embed frontend.html
var frontendHTML []byte

//go:embed assets
var frontendAssets embed.FS

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

// maxRequestBodyBytes bounds how large a POST /chat or /chat/stream body
// decodeChatRequest will read before giving up -- without this,
// json.Decode reads an attacker-supplied body of any size straight into
// memory (see audit.md finding #3). 64KB is comfortably above any real
// chat message (a message that long would blow every catalog model's
// context window anyway, see router.applyHardFilters) while still
// bounding worst-case memory use per in-flight request.
const maxRequestBodyBytes = 64 * 1024

// ErrInstantCapExceeded is returned by prepare (and surfaces from
// handle/handleStream) when limits.CheckInstantOverCap reports this
// user_id has crossed their plan's 30-day Instant-tier anti-bot ceiling
// (limits.PlanLimits.InstantExtraCapUSD) -- see docs/unit-economics.md
// section 6.5. Checked, and the whole request rejected, before classify/
// moderate run at all: unlike CheckThinkingMaxLock (which only changes
// which mode a request downgrades to), there is no cheaper tier to fall
// back to here, so an over-cap request has nothing useful left to do.
// handleChat/handleChatStream map this to HTTP 429.
var ErrInstantCapExceeded = errors.New("server: instant-tier spend cap exceeded for this billing cycle, try again later")

// Server holds everything one /chat request needs. All fields are
// required; use New to build one with validation.
type Server struct {
	TrustedProxies []netip.Prefix
	RequireHTTPS   bool
	Router         router.Router
	Classifier     classifier.Classifier
	Moderator      moderation.Moderator
	ModerationLog  moderation.BlockLog
	Conversations  conversation.Store
	Store          limits.SpendStore
	Plans          map[string]limits.PlanLimits

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

	// IPRateLimiter throttles POST /chat and /chat/stream by client IP,
	// checked in Mux's wrapping before authentication even runs -- see
	// audit.md finding #2. It is the outermost of the two limiters because
	// it's the one that costs nothing to evaluate: an unauthenticated
	// flood is rejected on a counter, without an Authenticate call (a
	// database lookup) per request.
	//
	// Being IP-keyed is what makes it useful against callers with no valid
	// credential at all -- the case UserRateLimiter structurally cannot
	// see, since it only exists downstream of a successful Authenticate.
	// The two cover opposite halves of the same problem; see
	// UserRateLimiter for the half this one misses.
	//
	// Nil (the zero value) disables the guard entirely, same nil-disables
	// convention as Idempotency -- a Server built without one (e.g. most
	// tests, which call handle/handleStream directly and never go through
	// Mux anyway) behaves exactly as it did before this field existed.
	IPRateLimiter ratelimit.Limiter

	// UserRateLimiter throttles POST /chat and /chat/stream by
	// authenticated user_id, checked after authenticated resolves the
	// caller's identity and before decodeChatRequest runs.
	//
	// This is the per-user_id half of audit.md finding #2's recommendation,
	// which was impossible until finding #1 was fixed: while user_id came
	// from the request body, a limiter keyed on it just moved with
	// whatever user_id an abusive client claimed next request, so IP was
	// the only key worth counting. Now that user_id comes from a bearer
	// token this counts what it means to count -- and it catches exactly
	// what IPRateLimiter cannot, namely one credential (leaked, shared, or
	// scripted against) driving traffic from many source addresses, where
	// no single IP's counter ever climbs high enough to trip.
	//
	// Nil (the zero value) disables it, same convention as IPRateLimiter.
	UserRateLimiter ratelimit.Limiter

	// Auth authenticates every POST /chat and POST /chat/stream call and
	// resolves it to the real (user_id, plan_id) -- see the authenticated
	// middleware and auth.Authenticator's doc comment. Required, unlike
	// IPRateLimiter/Idempotency's nil-disables convention: an
	// unauthenticated request pipeline is exactly audit.md finding #1
	// (spend-cap bypass by claiming a fresh user_id per request, plan_id
	// spoofing, IDOR on stored conversations), not an optional hardening
	// layer to be skippable by a nil field.
	Auth auth.Authenticator
}

// chatRequest is the wire format for POST /chat.
type chatRequest struct {
	idempotencyOwner        string
	regenerateMessageID     int64
	regenerationHistoryDrop int
	// UserID and PlanID are never read from the client, even though a
	// caller can still send "user_id"/"plan_id" in the JSON body -- json:"-"
	// means json.Decode silently ignores both. decodeChatRequest fills
	// them in itself right after decoding, from the auth.Identity the
	// authenticated middleware resolved for this request's bearer token
	// (see Server.Auth). This is the fix for audit.md finding #1: before
	// this, these two fields came straight from client-supplied JSON, so
	// any caller could claim to be any user_id on any plan_id. Still plain
	// exported fields rather than a separate parameter threaded alongside
	// chatRequest, because every step below (prepare, finalize,
	// recordAbortedSpend, reserveIdempotent, ...) already reads
	// req.UserID/req.PlanID -- only decodeChatRequest's source for them
	// changed.
	UserID string `json:"-"`
	PlanID string `json:"-"`

	// ConversationID threads this request onto an existing conversation's
	// stored history (see conversation/). Empty starts a new one --
	// handle generates an ID and returns it in chatResponse so the client
	// can pass it back on the next turn.
	ConversationID string `json:"conversation_id,omitempty"`
	Message        string `json:"message"`
	RequestedMode  string `json:"requested_mode"` // "auto" | "instant" | "thinking" | "max" | "manual"
	ManualModelID  string `json:"manual_model_id,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
	Incognito      bool   `json:"incognito,omitempty"`
	// IncognitoHistory carries the private conversation context back from
	// the browser. The server uses it for this generation only and never
	// writes it to the conversation store.
	IncognitoHistory []incognitoMessage `json:"incognito_history,omitempty"`

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

type incognitoMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
	MessageID        int64   `json:"message_id,omitempty"`

	// Blocked is true when Layer 1 moderation flagged the request before
	// any generation happened -- ResponseText is then tosViolationMessage,
	// not a model reply, and every other field is zero.
	Blocked bool `json:"blocked,omitempty"`
}

// Mux returns an http.ServeMux with routes registered. POST /chat and
// POST /chat/stream go through, outermost first: recovered ->
// rateLimited (per client IP) -> authenticated -> userRateLimited (per
// authenticated user_id) -> the handler. GET /health goes through
// recovered only -- container/orchestrator health checks shouldn't
// compete with real traffic for either quota, and have no credential to
// check anyway.
//
// The ordering is the point: the cheapest check that can reject a
// request runs first. A per-IP counter costs nothing and needs no
// identity, so it goes ahead of the Authenticate call (a database
// lookup) it would otherwise let an unauthenticated flood drive; the
// per-user counter can only run after that call, since its key doesn't
// exist until the token resolves. See IPRateLimiter/UserRateLimiter for
// why both exist rather than either alone.
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", recovered(s.frontend))
	mux.Handle("GET /assets/", http.FileServer(http.FS(frontendAssets)))
	mux.HandleFunc("POST /chat", recovered(s.rateLimited(s.authenticated(s.userRateLimited(s.handleChat)))))
	mux.HandleFunc("POST /chat/stream", recovered(s.rateLimited(s.authenticated(s.userRateLimited(s.handleChatStream)))))
	mux.HandleFunc("POST /chat/regenerate/stream", recovered(s.rateLimited(s.authenticated(s.userRateLimited(s.handleRegenerateStream)))))
	mux.HandleFunc("GET /conversations", recovered(s.rateLimited(s.authenticated(s.userRateLimited(s.withRequestSlot(s.handleConversationList))))))
	mux.HandleFunc("GET /conversations/{conversation_id}", recovered(s.rateLimited(s.authenticated(s.userRateLimited(s.withRequestSlot(s.handleConversationHistory))))))
	mux.HandleFunc("PATCH /conversations/{conversation_id}", recovered(s.rateLimited(s.authenticated(s.userRateLimited(s.withRequestSlot(s.handleConversationUpdate))))))
	mux.HandleFunc("DELETE /conversations/{conversation_id}", recovered(s.rateLimited(s.authenticated(s.userRateLimited(s.withRequestSlot(s.handleConversationDelete))))))
	mux.HandleFunc("GET /projects", recovered(s.rateLimited(s.authenticated(s.userRateLimited(s.withRequestSlot(s.handleProjectList))))))
	mux.HandleFunc("POST /projects", recovered(s.rateLimited(s.authenticated(s.userRateLimited(s.withRequestSlot(s.handleProjectCreate))))))
	mux.HandleFunc("GET /projects/{project_id}", recovered(s.rateLimited(s.authenticated(s.userRateLimited(s.withRequestSlot(s.handleProjectGet))))))
	mux.HandleFunc("GET /health", recovered(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	return mux
}

type conversationListResponse struct {
	Conversations []conversation.Overview `json:"conversations"`
}

type conversationHistoryResponse struct {
	NextCursor     int64                  `json:"next_cursor,omitempty"`
	ConversationID string                 `json:"conversation_id"`
	Messages       []conversation.Message `json:"messages"`
}

type conversationUpdateRequest struct {
	Title     *string `json:"title"`
	Pinned    *bool   `json:"pinned"`
	ProjectID *string `json:"project_id"`
}

type projectCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type projectDetailResponse struct {
	Project       conversation.Project    `json:"project"`
	Conversations []conversation.Overview `json:"conversations"`
}

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	projects, err := s.Conversations.ListProjects(r.Context(), identity.UserID)
	if err != nil {
		log.Printf("server: list projects for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"projects": projects}); err != nil {
		log.Printf("server: encode project list: %v", err)
	}
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var request projectCreateRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid project", http.StatusBadRequest)
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.Description = strings.TrimSpace(request.Description)
	if request.Name == "" || len([]rune(request.Name)) > 80 || len([]rune(request.Description)) > 2000 {
		http.Error(w, "invalid project", http.StatusBadRequest)
		return
	}
	project := conversation.Project{ID: conversation.NewID(), Name: request.Name, Description: request.Description, CreatedAt: time.Now()}
	if err := s.Conversations.CreateProject(r.Context(), identity.UserID, project); err != nil {
		log.Printf("server: create project for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(project); err != nil {
		log.Printf("server: encode created project: %v", err)
	}
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	id := strings.TrimSpace(r.PathValue("project_id"))
	project, err := s.Conversations.GetProject(r.Context(), identity.UserID, id)
	if errors.Is(err, conversation.ErrConversationNotFound) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("server: get project_id=%s for user_id=%s: %v", id, identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	chats, err := s.Conversations.ListByProject(r.Context(), identity.UserID, id, 100)
	if err != nil {
		log.Printf("server: list conversations for project_id=%s user_id=%s: %v", id, identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(projectDetailResponse{Project: project, Conversations: chats}); err != nil {
		log.Printf("server: encode project detail: %v", err)
	}
}

func conversationIDFromRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	conversationID := strings.TrimSpace(r.PathValue("conversation_id"))
	if conversationID == "" || len(conversationID) > 128 {
		http.Error(w, "invalid conversation_id", http.StatusBadRequest)
		return "", false
	}
	return conversationID, true
}

func (s *Server) handleConversationUpdate(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	conversationID, ok := conversationIDFromRequest(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var request conversationUpdateRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || (request.Title == nil && request.Pinned == nil && request.ProjectID == nil) {
		http.Error(w, "invalid update", http.StatusBadRequest)
		return
	}
	if request.Title != nil {
		title := strings.TrimSpace(*request.Title)
		if title == "" || len([]rune(title)) > 80 {
			http.Error(w, "title must be between 1 and 80 characters", http.StatusBadRequest)
			return
		}
		request.Title = &title
	}
	if request.ProjectID != nil && *request.ProjectID != "" {
		if _, err := s.Conversations.GetProject(r.Context(), identity.UserID, *request.ProjectID); err != nil {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
	}
	if err := s.Conversations.UpdateMetadata(r.Context(), identity.UserID, conversationID, conversation.MetadataUpdate{Title: request.Title, Pinned: request.Pinned, ProjectID: request.ProjectID}); err != nil {
		if errors.Is(err, conversation.ErrConversationNotFound) {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		log.Printf("server: update conversation_id=%s for user_id=%s: %v", conversationID, identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConversationDelete(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	conversationID, ok := conversationIDFromRequest(w, r)
	if !ok {
		return
	}
	if err := s.Conversations.Delete(r.Context(), identity.UserID, conversationID); err != nil {
		if errors.Is(err, conversation.ErrConversationNotFound) {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		log.Printf("server: delete conversation_id=%s for user_id=%s: %v", conversationID, identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConversationList(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	conversations, err := s.Conversations.List(r.Context(), identity.UserID, 100)
	if err != nil {
		log.Printf("server: list conversations for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(conversationListResponse{Conversations: conversations}); err != nil {
		log.Printf("server: encode conversation list: %v", err)
	}
}

func (s *Server) handleConversationHistory(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	conversationID := strings.TrimSpace(r.PathValue("conversation_id"))
	if conversationID == "" {
		http.Error(w, "conversation_id is required", http.StatusBadRequest)
		return
	}
	before := int64(0)
	if raw := r.URL.Query().Get("before"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			http.Error(w, "invalid history cursor", http.StatusBadRequest)
			return
		}
		before = value
	}
	page, err := s.Conversations.HistoryPage(r.Context(), identity.UserID, conversationID, before, 20)
	if err != nil {
		log.Printf("server: load conversation_id=%s for user_id=%s: %v", conversationID, identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	response := conversationHistoryResponse{ConversationID: conversationID, Messages: page.Messages, NextCursor: page.NextCursor}
	data, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "could not encode history", http.StatusInternalServerError)
		return
	}
	// Each message is database-bounded; also cap the final JSON response at 1 MiB.
	for len(data) > 1024*1024 && len(response.Messages) > 1 {
		response.Messages = response.Messages[1:]
		response.NextCursor = response.Messages[0].ID
		data, err = json.Marshal(response)
		if err != nil {
			http.Error(w, "could not encode history", http.StatusInternalServerError)
			return
		}
	}
	if len(data) > 1024*1024 {
		http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
		return
	}
	if _, err := w.Write(data); err != nil {
		log.Printf("server: encode conversation history: %v", err)
	}
}

// recovered turns a panic in next into a logged 500 for that one request.
// net/http already recovers handler panics per connection, so this is
// partly belt-and-braces -- what it adds is a stack trace in the log and
// an actual HTTP response, instead of net/http silently dropping the
// connection and leaving the caller staring at a reset. The panics that
// would genuinely take the whole process down are the ones in goroutines
// this handler spawns, which no HTTP-level middleware can see; those are
// handled at their own source (see recoverGoroutine).
//
// A response that has already started writing (notably /chat/stream,
// which sends its headers before generating anything) can't be turned
// into a clean 500 any more -- http.Error's status is ignored and its
// body lands mid-stream. The log line is the real deliverable in that
// case; the write is best-effort.
func recovered(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is net/http's own "abort this handler
			// quietly" sentinel, not a bug -- re-panic so net/http handles
			// it the way it expects to.
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			log.Printf("server: recovered panic handling %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
			http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		}()
		next(w, r)
	}
}

// recoverGoroutine converts a panic in a spawned goroutine into an error
// stored through errp, instead of letting it unwind past the goroutine's
// entry and take the entire process down with it.
//
// This is the case that actually matters (audit.md's panic finding):
// net/http's per-connection recovery only covers the handler goroutine,
// so before this, one panic anywhere in classify/moderate -- or in any
// dependency they call -- dropped every in-flight request for every user,
// not just the one request that tripped it.
//
// Must be deferred directly (defer recoverGoroutine(...)), since recover
// only works when called by a deferred function of the panicking
// goroutine itself. Register it after the goroutine's defer wg.Done() so
// it runs before that Done and the error is visible to whoever waits.
func recoverGoroutine(what string, errp *error) {
	rec := recover()
	if rec == nil {
		return
	}
	log.Printf("server: recovered panic in %s: %v\n%s", what, rec, debug.Stack())
	*errp = fmt.Errorf("server: panic in %s: %v", what, rec)
}

// rateLimited wraps next with s.IPRateLimiter, if one is configured. A
// request from an IP that has exceeded its quota gets a 429 and next
// never runs. A limiter error (e.g. Redis unreachable) fails open --
// logged and the request let through -- rather than turning a Redis blip
// into a chat outage; see IPRateLimiter's doc comment for what this
// check catches that userRateLimited's cannot.
func (s *Server) rateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.IPRateLimiter != nil {
			ip := s.requestIP(r)
			allowed, err := s.IPRateLimiter.Allow(r.Context(), ip)
			if err != nil {
				log.Printf("server: rate limiter check failed for ip=%s, rejecting request: %v", ip, err)
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			} else if !allowed {
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
		}
		next(w, r)
	}
}

// userRateLimited wraps next with s.UserRateLimiter, if one is
// configured, keyed on the already-authenticated identity.UserID -- so it
// must sit inside authenticated, which is what produces that identity
// (see Mux). A user over quota gets a 429 and next never runs.
//
// A limiter failure rejects the request: Redis errors must not disable abuse protection.
func (s *Server) userRateLimited(next identityHandler) identityHandler {
	return func(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
		if s.UserRateLimiter != nil {
			allowed, err := s.UserRateLimiter.Allow(r.Context(), identity.UserID)
			if err != nil {
				log.Printf("server: user rate limiter check failed for user_id=%s, rejecting request: %v", identity.UserID, err)
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			} else if !allowed {
				http.Error(w, "too many requests for this API key", http.StatusTooManyRequests)
				return
			}
		}
		next(w, r, identity)
	}
}

// clientIP extracts the connecting IP from r.RemoteAddr (host:port).
// Falls back to the raw RemoteAddr string if it isn't in that form --
// still a usable (if coarser) rate-limit key rather than a hard failure.
// Does not consult X-Forwarded-For/X-Real-IP: those are only trustworthy
// behind a reverse proxy that sets them itself, which this repo doesn't
// have yet (see README "Next steps") -- trusting a client-supplied header
// here would let the rate limit itself be spoofed per request.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// identityHandler is an HTTP handler that additionally receives the
// caller's authenticated identity -- handleChat/handleChatStream's actual
// signature once wrapped by authenticated below.
type identityHandler func(w http.ResponseWriter, r *http.Request, identity auth.Identity)

// authenticated verifies the request's "Authorization: Bearer <api-key>"
// header via s.Auth before calling next, resolving it to the caller's
// real auth.Identity instead of the client-supplied user_id/plan_id
// decodeChatRequest used to trust directly out of the JSON body (audit.md
// finding #1). A missing/malformed header and a token s.Auth doesn't
// recognize both get the same 401 with the same message -- see
// auth.ErrInvalidToken's doc comment for why they're not distinguished.
func (s *Server) authenticated(next identityHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.RequireHTTPS && r.TLS == nil && !(s.trustedProxy(clientIP(r)) && r.Header.Get("X-Forwarded-Proto") == "https") {
			http.Error(w, "HTTPS is required", http.StatusBadRequest)
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing or malformed Authorization header, want: Bearer <api-key>", http.StatusUnauthorized)
			return
		}
		identity, err := s.Auth.Authenticate(r.Context(), token)
		if err != nil {
			if !errors.Is(err, auth.ErrInvalidToken) {
				log.Printf("server: auth check failed: %v", err)
			}
			http.Error(w, "invalid API key", http.StatusUnauthorized)
			return
		}
		next(w, r, identity)
	}
}

// bearerToken extracts the credential from an "Authorization: Bearer
// <token>" request header, reporting false if the header is absent, uses
// a different scheme, or the token portion is empty/all-whitespace.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	return token, token != ""
}

// decodeChatRequest parses and validates the POST /chat and POST
// /chat/stream request bodies -- both endpoints run the exact same
// pipeline (see handle/handleStream) and differ only in how the result is
// delivered, so their input handling is shared here rather than
// duplicated per handler. identity is the caller authenticated already
// resolved; decodeChatRequest is what actually stamps it onto the
// returned chatRequest (see chatRequest.UserID/PlanID's doc comment).
func decodeChatRequest(w http.ResponseWriter, r *http.Request, plans map[string]limits.PlanLimits, identity auth.Identity) (chatRequest, limits.PlanLimits, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, fmt.Sprintf("request body exceeds %d byte limit", maxRequestBodyBytes), http.StatusRequestEntityTooLarge)
			return chatRequest{}, limits.PlanLimits{}, false
		}
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return chatRequest{}, limits.PlanLimits{}, false
	}
	req.UserID = identity.UserID
	req.PlanID = identity.PlanID

	if req.Message == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return chatRequest{}, limits.PlanLimits{}, false
	}
	plan, ok := plans[req.PlanID]
	if !ok {
		// identity.PlanID came from api_keys (see auth.Store.IssueKey), not
		// from this request -- landing here means the plan attached to this
		// caller's key was since removed from configs/plans.json, a
		// server-side data-integrity problem, not a mistake in this
		// request. Logged with the user_id so it's traceable, but not
		// phrased to the caller as their input being wrong.
		log.Printf("server: authenticated user_id=%s has unknown plan_id %q, check configs/plans.json", req.UserID, req.PlanID)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return chatRequest{}, limits.PlanLimits{}, false
	}
	if req.Persona != "" && !isValidPersona(req.Persona) {
		http.Error(w, fmt.Sprintf("unknown persona %q (want one of %v)", req.Persona, SystemPromptNames), http.StatusBadRequest)
		return chatRequest{}, limits.PlanLimits{}, false
	}
	return req, plan, true
}

// clientErrorMessage returns the text safe to send back to a POST /chat
// or /chat/stream caller for an error out of handle/handleStream --
// always logged in full server-side first (see handleChat's/
// handleChatStream's log.Printf right before each call site below), but
// only a small allowlist of errors this package builds specifically to
// be user-facing get their own Error() text quoted back verbatim.
// Everything else -- prepare's classify/moderate/route/generate/
// persistence failures -- gets replaced with a generic message instead:
// those can carry a vendor's raw response text, a model/provider
// identifier, or a database/Redis failure detail that a caller who just
// sent a chat message has no business seeing (audit.md's error-leakage
// finding). Add a new case here, not a bare error string at the call
// site, for any future error that's genuinely meant to reach the client.
func clientErrorMessage(err error) string {
	if errors.Is(err, ErrInstantCapExceeded) || errors.Is(err, idempotency.ErrInFlight) || errors.Is(err, limits.ErrBudgetExceeded) || errors.Is(err, errInvalidRequest) {
		return err.Error()
	}
	return "an internal error occurred processing this request"
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	req, plan, ok := decodeChatRequest(w, r, s.Plans, identity)
	if !ok {
		return
	}

	resp, err := s.handle(r.Context(), req, plan)
	if err != nil {
		log.Printf("server: /chat error for user_id=%s: %v", req.UserID, err)
		status := http.StatusBadGateway
		if errors.Is(err, ErrInstantCapExceeded) || errors.Is(err, limits.ErrBudgetExceeded) || errors.Is(err, idempotency.ErrInFlight) {
			status = http.StatusTooManyRequests
		}
		if errors.Is(err, errInvalidRequest) {
			status = http.StatusBadRequest
		}
		http.Error(w, clientErrorMessage(err), status)
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
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	req, plan, ok := decodeChatRequest(w, r, s.Plans, identity)
	if !ok {
		return
	}
	s.writeChatStream(w, r, req, plan)
}

type regenerateRequest struct {
	ConversationID string `json:"conversation_id"`
	MessageID      int64  `json:"message_id,omitempty"`
	RequestedMode  string `json:"requested_mode"`
	ManualModelID  string `json:"manual_model_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	Persona        string `json:"persona,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
}

func (s *Server) handleRegenerateStream(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var input regenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(input.ConversationID) == "" || input.MessageID < 0 {
		http.Error(w, "conversation_id and a valid message_id are required", http.StatusBadRequest)
		return
	}
	plan, ok := s.Plans[identity.PlanID]
	if !ok {
		log.Printf("server: authenticated user_id=%s has unknown plan_id %q", identity.UserID, identity.PlanID)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	history, err := s.Conversations.History(r.Context(), identity.UserID, input.ConversationID, 0)
	if err != nil {
		log.Printf("server: load regeneration target for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	if len(history) < 2 {
		http.Error(w, "response is not available for regeneration", http.StatusNotFound)
		return
	}
	target := history[len(history)-1]
	if target.Role != conversation.RoleAssistant || (input.MessageID != 0 && target.ID != input.MessageID) || history[len(history)-2].Role != conversation.RoleUser {
		http.Error(w, "only the latest response can be regenerated", http.StatusConflict)
		return
	}
	versionCount := len(target.Versions)
	if versionCount == 0 {
		versionCount = 1
	}
	if versionCount-1 >= conversation.MaxRegenerationAttempts {
		http.Error(w, "regeneration limit reached", http.StatusTooManyRequests)
		return
	}
	req := chatRequest{
		UserID: identity.UserID, PlanID: identity.PlanID,
		ConversationID: input.ConversationID, Message: history[len(history)-2].Content,
		RequestedMode: input.RequestedMode, ManualModelID: input.ManualModelID,
		IdempotencyKey: input.IdempotencyKey, Persona: input.Persona,
		ProjectID:           input.ProjectID,
		regenerateMessageID: target.ID, regenerationHistoryDrop: 2,
	}
	if req.Persona != "" && !isValidPersona(req.Persona) {
		http.Error(w, "unknown persona", http.StatusBadRequest)
		return
	}
	s.writeChatStream(w, r, req, plan)
}

func (s *Server) writeChatStream(w http.ResponseWriter, r *http.Request, req chatRequest, plan limits.PlanLimits) {

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
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}

	if err := s.handleStream(r.Context(), req, plan, send); err != nil {
		log.Printf("server: /chat/stream error for user_id=%s: %v", req.UserID, err)
		send("error", map[string]string{"message": clientErrorMessage(err)})
	}
}

// preparedRequest holds everything shared between handle and handleStream
// once classification, moderation, and history loading have all run --
// only how the actual generation call is made (and its result delivered)
// differs between the two.
type preparedRequest struct {
	plan           limits.PlanLimits
	conversationID string
	classified     router.ClassifierOutput
	locked         bool
	messages       []provider.Message

	// estimatedContextTokens is tokenizer.EstimateMessages(messages) --
	// computed server-side from what's actually about to be sent, not
	// taken from chatRequest.EstimatedContextTokens (see that field's doc
	// comment for why the client-supplied number is no longer trusted).
	estimatedContextTokens int

	// requestID identifies this one /chat call's costlog.Store.Record
	// entries -- shared by the classify/moderate entries recordAuxCostLog
	// logs partway through prepare and the generation entry finalize logs
	// afterward, so all three of one call's billed model calls tie
	// together for reconciliation. Distinct from conversationID (spans
	// every turn of a thread) and chatRequest.IdempotencyKey
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
	if err := s.validateRequest(req); err != nil {
		return preparedRequest{}, nil, err
	}
	var project conversation.Project
	if req.ProjectID != "" {
		project, err = s.Conversations.GetProject(ctx, req.UserID, req.ProjectID)
		if errors.Is(err, conversation.ErrConversationNotFound) {
			return preparedRequest{}, nil, fmt.Errorf("%w: unknown project", errInvalidRequest)
		}
		if err != nil {
			return preparedRequest{}, nil, fmt.Errorf("load project: %w", err)
		}
	}
	classifierForRequest := s.Classifier
	classifierForRequest.Client = auxiliaryClient{server: s, request: req, plan: plan, client: s.Classifier.Client, inputRate: s.Classifier.CostInputPerMTok, outputRate: s.Classifier.CostOutputPerMTok}
	moderatorForRequest := s.Moderator
	moderatorForRequest.Client = auxiliaryClient{server: s, request: req, plan: plan, client: s.Moderator.Client, inputRate: s.Moderator.CostInputPerMTok, outputRate: s.Moderator.CostOutputPerMTok}

	// Checked first, before anything else in prepare runs: classify/
	// moderate are themselves billed calls (see recordAuxCostLog below), so
	// an already-over-the-anti-bot-ceiling request should never reach even
	// those, let alone routing/generation -- see ErrInstantCapExceeded's
	// doc comment and audit.md finding #2. limits.CheckInstantOverCap
	// existed before this but was never wired into the request pipeline.
	overInstantCap, err := limits.CheckInstantOverCap(ctx, s.Store, plan, req.UserID)
	if err != nil {
		return preparedRequest{}, nil, fmt.Errorf("check instant cap: %w", err)
	}
	if overInstantCap {
		return preparedRequest{}, nil, ErrInstantCapExceeded
	}

	conversationID := req.ConversationID
	if conversationID == "" {
		conversationID = conversation.NewID()
	}

	// Minted here rather than at the bottom of prepare (where it used to
	// live) so the classify/moderate cost_log entries below and the
	// eventual generation's entry (finalize) share one RequestID -- all
	// three of a single /chat call's billed model calls tie together for
	// reconciliation, not just the generation.
	requestID := conversation.NewID()

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
		classified    router.ClassifierOutput
		classifyUsage *provider.GenerateResult
		classifyErr   error
		modResult     moderation.Result
		modUsage      *provider.GenerateResult
		modErr        error
	)
	var wg sync.WaitGroup
	wg.Add(2)
	// Both goroutines recover their own panics into their err variable
	// (see recoverGoroutine): a panic escaping either one would otherwise
	// take the whole process down, dropping every other user's in-flight
	// request along with this one. Moderation failing closed then applies
	// to a panic exactly as it does to an ordinary error below.
	go func() {
		defer wg.Done()
		defer recoverGoroutine("classifier.Classify", &classifyErr)
		classified, classifyUsage, classifyErr = classifierForRequest.Classify(ctx, req.Message)
	}()
	go func() {
		defer wg.Done()
		defer recoverGoroutine("moderation.Moderate", &modErr)
		modResult, modUsage, modErr = moderatorForRequest.Moderate(ctx, req.Message)
	}()
	wg.Wait()

	// Logged unconditionally here, before any of the branches below
	// return early: both calls already happened (and were billed) by this
	// point regardless of whether moderation flags/errors or classify
	// errors afterward -- see README pre-launch checklist item 7
	// ("classifier and moderation calls" were the still-open piece).
	s.recordAuxCostLog(ctx, req.UserID, requestID, "classify", s.Classifier.APIModelID, s.Classifier.CostInputPerMTok, s.Classifier.CostOutputPerMTok, classifyUsage)
	s.recordAuxCostLog(ctx, req.UserID, requestID, "moderate", s.Moderator.APIModelID, s.Moderator.CostInputPerMTok, s.Moderator.CostOutputPerMTok, modUsage)

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

	var tail []conversation.Message
	var summaryText string
	if req.Incognito {
		tail = make([]conversation.Message, 0, len(req.IncognitoHistory))
		for _, message := range req.IncognitoHistory {
			tail = append(tail, conversation.Message{Role: conversation.Role(message.Role), Content: message.Content})
		}
	} else {
		// Only saved chats load and summarize server-side history. Private
		// history exists solely in this request body and is discarded after it.
		summaryState, history, err := s.loadHistory(ctx, req.UserID, conversationID)
		if err != nil {
			return preparedRequest{}, nil, fmt.Errorf("load conversation history: %w", err)
		}
		if req.regenerationHistoryDrop > 0 {
			if len(history) < req.regenerationHistoryDrop || history[len(history)-1].ID != req.regenerateMessageID {
				return preparedRequest{}, nil, fmt.Errorf("%w: regeneration target changed", errInvalidRequest)
			}
			history = history[:len(history)-req.regenerationHistoryDrop]
		}
		summaryServer := *s
		summaryServer.Summarizer.Client = auxiliaryClient{server: s, request: req, plan: plan, client: s.Summarizer.Client, inputRate: s.Summarizer.CostInputPerMTok, outputRate: s.Summarizer.CostOutputPerMTok}
		tail, summaryText = summaryServer.maybeSummarize(ctx, req.UserID, conversationID, summaryState, history)
	}

	persona := req.Persona
	if persona == "" {
		persona = defaultPersona
	}
	systemPrompt := s.SystemPrompts[persona] // "" for a valid-but-not-yet-written persona -- see LoadSystemPrompts

	messages := make([]provider.Message, 0, len(tail)+3)
	if systemPrompt != "" {
		messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	}
	if project.ID != "" && project.Description != "" {
		messages = append(messages, provider.Message{Role: "system", Content: "Project " + project.Name + " instructions:\n" + project.Description})
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
		requestID:              requestID,
		plan:                   plan,
	}, nil, nil
}

// recordAuxCostLog logs a cost_log entry for the classify/moderate call
// identified by mode ("classify" or "moderate") -- the generation entry
// itself is still logged separately by finalize, since it alone knows the
// selected model/mode and needs RouteResult. usage nil means the
// underlying Client.Generate call never completed (see
// Classifier.Classify/Moderator.Moderate's doc comments), i.e. nothing
// was billed, so there's nothing to log. Best-effort, matching
// ModerationLog.Record just above: a logging failure costs one missing
// analytics row, not the request itself.
func (s *Server) recordAuxCostLog(ctx context.Context, userID, requestID, mode, apiModelID string, costInputPerMTok, costOutputPerMTok float64, usage *provider.GenerateResult) {
	if usage == nil {
		return
	}
	entry := router.CostLogEntry{
		UserID:       userID,
		RequestID:    requestID,
		ModelID:      apiModelID,
		Mode:         mode,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CostUSD:      router.ComputeCostUSDRates(costInputPerMTok, costOutputPerMTok, usage.InputTokens, usage.OutputTokens),
		Timestamp:    time.Now(),
	}
	if err := s.CostLog.Record(ctx, entry); err != nil {
		log.Printf("server: failed to record %s cost log entry for user_id=%s: %v", mode, userID, err)
	}
}

// loadHistory reads the part of a conversation that still has to be sent
// verbatim -- everything after what the stored Summary already covers --
// together with that Summary itself.
//
// Reading from Summary.CoversThrough rather than from the beginning is
// what keeps this bounded: once summarization is active, the unsummarized
// remainder is re-folded (and CoversThrough advanced) every time it grows
// past SummaryTriggerTokens, so the window this returns stays around that
// threshold no matter how long the conversation gets. Reading the whole
// history instead -- what this used to do -- meant a conversation with N
// stored messages did O(N) database and memory work on every single turn,
// forever, even though maybeSummarize discarded all but the last few (see
// audit.md's unbounded-history finding).
//
// A GetSummary failure is not fatal, matching maybeSummarize's own
// treatment of summarization trouble: it logs and falls back to the full
// history with no summary, i.e. exactly the pre-summarization behavior.
// The same fallback covers SummaryTriggerTokens <= 0 (feature off), where
// there is no summary state to trust an offset from.
func (s *Server) loadHistory(ctx context.Context, userID, conversationID string) (conversation.Summary, []conversation.Message, error) {
	if s.SummaryTriggerTokens > 0 {
		state, err := s.Conversations.GetSummary(ctx, userID, conversationID)
		if err != nil {
			log.Printf("server: get summary for user_id=%s conversation_id=%s: %v", userID, conversationID, err)
		} else {
			history, err := s.Conversations.History(ctx, userID, conversationID, state.CoversThrough)
			return state, history, err
		}
	}

	history, err := s.Conversations.History(ctx, userID, conversationID, 0)
	return conversation.Summary{}, history, err
}

// maybeSummarize decides whether the unsummarized window loadHistory
// returned is small enough to send as-is, or whether its older part
// should now be folded into the rolling summary too. It returns the
// messages to send verbatim (the whole window, or just its most recent
// SummaryTailMessages) and the summary text to inject alongside them.
//
// state is the Summary those messages sit after, so index i of window is
// absolute index state.CoversThrough+i -- that correspondence is what
// makes the new CoversThrough written below correct.
//
// An already-stored summary is returned on every path, including the ones
// that fold nothing new: it describes messages that were deliberately not
// read back, so dropping it would silently amputate the conversation
// rather than merely skip an optimization. Only a conversation that has
// never been summarized yields "".
//
// Failure to summarize (the summarizer model call failing) is not fatal:
// it logs and falls back to sending the window unchanged alongside
// whatever summary already existed -- worst case a very large
// conversation still hits applyHardFilters' context-window check, the
// same outcome as before this feature existed.
func (s *Server) maybeSummarize(ctx context.Context, userID, conversationID string, state conversation.Summary, window []conversation.Message) ([]conversation.Message, string) {
	// SummaryTriggerTokens <= 0 means the feature is unconfigured (the
	// zero-value Server, e.g. in tests that don't set it) rather than
	// "summarize everything" -- a real deployment always sets a positive
	// threshold, see cmd/server/main.go. loadHistory already read the full
	// history in that case, so there is no summary to carry either.
	if s.SummaryTriggerTokens <= 0 {
		return window, ""
	}
	if len(window) <= s.SummaryTailMessages {
		return window, state.Text
	}

	windowAsMessages := make([]provider.Message, len(window))
	for i, m := range window {
		windowAsMessages[i] = provider.Message{Role: string(m.Role), Content: m.Content}
	}
	if tokenizer.EstimateMessages(windowAsMessages) < s.SummaryTriggerTokens {
		return window, state.Text
	}

	// Everything before the tail is unsummarized by construction (that's
	// what loadHistory's offset guarantees), so the whole leading part of
	// the window is what gets folded -- no intersecting with
	// state.CoversThrough needed the way there was when this received the
	// full history.
	tailStart := len(window) - s.SummaryTailMessages
	toFold := window[:tailStart]

	newSummary, err := s.Summarizer.Summarize(ctx, state.Text, toFold)
	if err != nil {
		log.Printf("server: summarize user_id=%s conversation_id=%s: %v", userID, conversationID, err)
		return window, state.Text
	}
	if err := s.Conversations.SetSummary(ctx, userID, conversationID, conversation.Summary{
		Text:          newSummary,
		CoversThrough: state.CoversThrough + tailStart,
		UpdatedAt:     time.Now(),
	}); err != nil {
		log.Printf("server: set summary for user_id=%s conversation_id=%s: %v", userID, conversationID, err)
	}

	return window[tailStart:], newSummary
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

		genResult, err := s.billedCall(ctx, req, prepared.plan, generationPool(result, model), model.CostInputPerMTok, model.CostOutputPerMTok, model.MaxOutputTokens, gen, model.ResolveAPIModelID(), prepared.messages, call)
		if err == nil {
			return result, model, genResult, nil
		}

		var circuitErr *provider.CircuitOpenError
		if !errors.As(err, &circuitErr) || attempt >= maxCircuitFailoverAttempts-1 {
			// result and model are returned even though this failed: the
			// call reached a specific model, so the vendor may already
			// have generated (and charged for) tokens. recordAbortedSpend
			// needs to know which model to price that against -- returning
			// zero values here is what made that cost invisible.
			return result, model, provider.GenerateResult{}, fmt.Errorf("generate: %w", err)
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	now := time.Now()
	var regenerated conversation.Message
	if req.Incognito {
		// Incognito turns intentionally never reach Conversations. Usage and
		// cost accounting below still run so private mode cannot bypass limits.
	} else if req.regenerateMessageID > 0 {
		var err error
		regenerated, err = s.Conversations.AddResponseVersion(ctx, req.UserID, prepared.conversationID, req.regenerateMessageID, conversation.ResponseVersion{Content: genResult.Text, ModelID: model.ID, CreatedAt: now})
		if err != nil {
			return chatResponse{}, fmt.Errorf("persist regenerated response: %w", err)
		}
	} else {
		if err := s.Conversations.Append(ctx, req.UserID, prepared.conversationID, conversation.Message{Role: conversation.RoleUser, Content: req.Message, CreatedAt: now}); err != nil {
			log.Printf("server: failed to persist user message for conversation_id=%s: %v", prepared.conversationID, err)
		}
		if err := s.Conversations.Append(ctx, req.UserID, prepared.conversationID, conversation.Message{Role: conversation.RoleAssistant, Content: genResult.Text, ModelID: model.ID, CreatedAt: now}); err != nil {
			log.Printf("server: failed to persist assistant message for conversation_id=%s: %v", prepared.conversationID, err)
		}
	}
	if !req.Incognito && req.ProjectID != "" {
		if err := s.Conversations.UpdateMetadata(ctx, req.UserID, prepared.conversationID, conversation.MetadataUpdate{ProjectID: &req.ProjectID}); err != nil {
			log.Printf("server: assign conversation_id=%s to project_id=%s: %v", prepared.conversationID, req.ProjectID, err)
			if req.ConversationID == "" {
				if cleanupErr := s.Conversations.Delete(ctx, req.UserID, prepared.conversationID); cleanupErr != nil && !errors.Is(cleanupErr, conversation.ErrConversationNotFound) {
					log.Printf("server: clean up unassigned project conversation_id=%s: %v", prepared.conversationID, cleanupErr)
				}
			}
		}
	}

	actualCost := router.ComputeCostUSD(model, genResult.InputTokens, genResult.OutputTokens)
	// Best-effort like Conversations.Append above: the request already
	// succeeded and the user already has their answer, so a logging
	// failure here shouldn't fail the request -- it just means this one
	// request is missing from the per-request cost log (README pre-launch
	// checklist item 7), not from the spend store already settled by billedCall,
	// which is what actually gates the Thinking+Max cap.
	entry := router.NewCostLogEntry(req.UserID, prepared.requestID, model, result.SelectedMode, genResult.InputTokens, genResult.OutputTokens, now)
	if err := s.CostLog.Record(ctx, entry); err != nil {
		log.Printf("server: failed to record cost log entry for request_id=%s: %v", prepared.requestID, err)
	}

	response := chatResponse{
		ConversationID:   prepared.conversationID,
		SelectedModelID:  result.SelectedModelID,
		SelectedMode:     result.SelectedMode,
		Reason:           result.Reason,
		EstimatedCostUSD: result.EstimatedCostUSD,
		ActualCostUSD:    actualCost,
		ResponseText:     genResult.Text,
	}
	if req.regenerateMessageID > 0 {
		response.MessageID = regenerated.ID
	}
	return response, nil
}

// abortedSpendTimeout bounds the accounting writes recordAbortedSpend
// makes after a request has already failed. Short on purpose: the user is
// gone, nothing is waiting on this, and it must not keep a connection
// tied up on the way out.
const abortedSpendTimeout = 5 * time.Second

// recordAbortedSpend logs measurable partial output for reconciliation. The
// budget is already protected by billedCall's retained upper reservation, even
// when the provider supplies no usage or the browser disconnects. This estimated
// cost-log row is marked "_aborted" and does not debit the spend store again.
func (s *Server) recordAbortedSpend(ctx context.Context, req chatRequest, prepared preparedRequest, result router.RouteResult, model router.Model, partialText string, cause error) {
	if model.ID == "" {
		// Never reached a model (routing failed, no generator configured,
		// every circuit open) -- nothing was sent, so nothing was billed.
		return
	}

	outputTokens := tokenizer.EstimateText(partialText)
	if outputTokens == 0 {
		log.Printf("server: generation aborted with no measurable output, upper budget reservation retained: request_id=%s user_id=%s model_id=%s: %v",
			prepared.requestID, req.UserID, model.ID, cause)
		return
	}

	// WithoutCancel because the usual cause of getting here is precisely
	// that ctx is already dead: billing against it would fail every write
	// and reinstate the very hole this exists to close.
	billCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortedSpendTimeout)
	defer cancel()

	now := time.Now()
	cost := router.ComputeCostUSD(model, prepared.estimatedContextTokens, outputTokens)
	log.Printf("server: recording aborted generation: request_id=%s user_id=%s model_id=%s estimated_output_tokens=%d estimated_cost_usd=%.6f: %v",
		prepared.requestID, req.UserID, model.ID, outputTokens, cost, cause)

	// billedCall already retained the upper reservation for uncertain usage.

	entry := router.NewCostLogEntry(req.UserID, prepared.requestID, model, result.SelectedMode+"_aborted", prepared.estimatedContextTokens, outputTokens, now)
	if err := s.CostLog.Record(billCtx, entry); err != nil {
		log.Printf("server: failed to record aborted cost log entry for request_id=%s: %v", prepared.requestID, err)
	}
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
func (s *Server) reserveIdempotent(ctx context.Context, req *chatRequest) (chatResponse, bool, error) {
	if req.Incognito || req.IdempotencyKey == "" || s.Idempotency == nil {
		return chatResponse{}, false, nil
	}

	rec, found, err := s.Idempotency.Reserve(ctx, req.UserID, "client:"+req.IdempotencyKey)
	req.idempotencyOwner = rec.Owner
	if err != nil {
		if errors.Is(err, idempotency.ErrInFlight) {
			// Wrapped with %w (not just formatted in) so clientErrorMessage
			// can still recognize this as idempotency.ErrInFlight and treat
			// its text as safe to show the caller -- see that function's
			// doc comment.
			return chatResponse{}, false, fmt.Errorf("a request with idempotency_key %q is already in progress for this user: %w", req.IdempotencyKey, idempotency.ErrInFlight)
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if req.Incognito || req.IdempotencyKey == "" || s.Idempotency == nil {
		return
	}

	if handleErr != nil {
		if err := s.Idempotency.Release(ctx, req.UserID, "client:"+req.IdempotencyKey, req.idempotencyOwner); err != nil {
			log.Printf("server: failed to release idempotency_key=%s for user_id=%s: %v", req.IdempotencyKey, req.UserID, err)
		}
		return
	}

	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("server: failed to encode idempotent response for idempotency_key=%s: %v", req.IdempotencyKey, err)
		if relErr := s.Idempotency.Release(ctx, req.UserID, "client:"+req.IdempotencyKey, req.idempotencyOwner); relErr != nil {
			log.Printf("server: failed to release idempotency_key=%s for user_id=%s: %v", req.IdempotencyKey, req.UserID, relErr)
		}
		return
	}
	if err := s.Idempotency.Complete(ctx, req.UserID, "client:"+req.IdempotencyKey, idempotency.Record{Response: data, Owner: req.idempotencyOwner}); err != nil {
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
	if err := s.validateRequest(req); err != nil {
		return chatResponse{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if cached, found, err := s.reserveIdempotent(ctx, &req); err != nil {
		return chatResponse{}, err
	} else if found {
		return cached, nil
	}

	release, err := s.acquireRequest(ctx, req)
	if err != nil {
		s.finishIdempotent(ctx, req, chatResponse{}, err)
		return chatResponse{}, err
	}
	defer release()
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
		// No partial text to price: a failed non-streaming call reports no
		// usage at all, so this only logs the possible unbilled cost -- see
		// recordAbortedSpend.
		s.recordAbortedSpend(ctx, req, prepared, result, model, "", err)
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
	if err := s.validateRequest(req); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if cached, found, err := s.reserveIdempotent(ctx, &req); err != nil {
		return err
	} else if found {
		if cached.Blocked {
			send("blocked", cached)
		} else {
			send("done", cached)
		}
		return nil
	}

	release, err := s.acquireRequest(ctx, req)
	if err != nil {
		s.finishIdempotent(ctx, req, chatResponse{}, err)
		return err
	}
	defer release()
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

	// Deltas are accumulated as they go out, not just forwarded: if the
	// stream then dies (client disconnect, vendor error), this is the only
	// record of how much the vendor actually generated and billed for --
	// the failed call itself reports no usage. See recordAbortedSpend.
	var streamed strings.Builder
	call := streamingGenerate(func(delta string) {
		streamed.WriteString(delta)
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
		s.recordAbortedSpend(ctx, req, prepared, result, model, streamed.String(), err)
		return chatResponse{}, err
	}

	resp, err := s.finalize(ctx, req, prepared, result, model, genResult)
	if err != nil {
		return chatResponse{}, err
	}
	if req.regenerateMessageID > 0 {
		history, err := s.Conversations.History(ctx, req.UserID, prepared.conversationID, 0)
		if err != nil {
			return chatResponse{}, fmt.Errorf("load regenerated response versions: %w", err)
		}
		if len(history) == 0 {
			return chatResponse{}, fmt.Errorf("load regenerated response versions: %w", conversation.ErrMessageNotFound)
		}
		latest := history[len(history)-1]
		send("versions", map[string]any{"message_id": latest.ID, "versions": latest.Versions})
	}
	send("done", resp)
	return resp, nil
}
