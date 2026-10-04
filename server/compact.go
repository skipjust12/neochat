package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"neochat/auth"
	"neochat/conversation"
	"neochat/provider"
	"neochat/summarizer"
)

// Settings sync: GET /account/settings returns what the user saved (or
// {} before they saved anything), PUT replaces it. The web UI applies it on
// sign-in and saves every change, so tone, instructions, the default model
// and the theme follow the user between devices. Keys stay in each
// browser.

const maxSettingsBytes = 32 << 10

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if s.Settings == nil {
		http.Error(w, "settings sync isn't available on this server", http.StatusServiceUnavailable)
		return
	}
	value, err := s.Settings.Get(r.Context(), identity.UserID)
	if err != nil {
		log.Printf("server: get settings for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	if len(value) == 0 {
		value = json.RawMessage(`{}`)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(value)
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if s.Settings == nil {
		http.Error(w, "settings sync isn't available on this server", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSettingsBytes))
	var object map[string]json.RawMessage
	if err != nil || json.Unmarshal(body, &object) != nil || object == nil {
		http.Error(w, "settings must be a JSON object under 32 KB", http.StatusBadRequest)
		return
	}
	for name := range object {
		if strings.Contains(strings.ToLower(name), "key") {
			http.Error(w, "API keys are not synced", http.StatusBadRequest)
			return
		}
	}
	if err := s.Settings.Put(r.Context(), identity.UserID, body); err != nil {
		log.Printf("server: put settings for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Compact: POST /conversations/{id}/compact folds everything but the last
// exchange into the chat's summary, written by compactModelID on the
// user's own key. Later turns send the summary instead of those messages
// (the same mechanism automatic summarization uses), so a long chat gets
// cheaper and faster without losing its thread. The messages stay on
// screen; the last exchange stays verbatim, which also keeps it
// regenerable.
const compactModelID = "gpt-6-luna"

type compactResponse struct {
	// CompactedThroughID is the last message the summary covers.
	CompactedThroughID int64 `json:"compacted_through_id"`
}

func (s *Server) handleConversationCompact(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	conversationID, ok := conversationIDFromRequest(w, r)
	if !ok {
		return
	}
	plan, ok := s.Plans[identity.PlanID]
	if !ok {
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	model, found := s.Router.Catalog.FindModel(compactModelID)
	gen := s.Generators[model.Provider]
	if !found || gen == nil {
		http.Error(w, "compacting isn't available on this server", http.StatusServiceUnavailable)
		return
	}
	key := strings.TrimSpace(r.Header.Get(providerKeyHeader))
	if key == "" && s.RouterDisabled {
		http.Error(w, errMissingProviderKey.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if key != "" {
		ctx = provider.WithAPIKey(ctx, key)
	}
	state, err := s.Conversations.GetSummary(ctx, identity.UserID, conversationID)
	if err != nil {
		log.Printf("server: compact: get summary for conversation_id=%s: %v", conversationID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	history, err := s.Conversations.History(ctx, identity.UserID, conversationID, 0)
	if err != nil {
		log.Printf("server: compact: load conversation_id=%s: %v", conversationID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	if len(history) == 0 {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	keep := len(history) - 2
	if keep <= state.CoversThrough {
		http.Error(w, "Nothing to compact yet: the chat is already as short as it gets.", http.StatusConflict)
		return
	}
	request := chatRequest{UserID: identity.UserID, PlanID: identity.PlanID}
	compactor := summarizer.New(auxiliaryClient{server: s, request: request, plan: plan, client: gen, inputRate: model.CostInputPerMTok, outputRate: model.CostOutputPerMTok}, model.ResolveAPIModelID(), s.Summarizer.SystemPrompt)
	text, err := compactor.Summarize(ctx, state.Text, summaryInput(history[state.CoversThrough:keep]))
	if err != nil {
		log.Printf("server: compact conversation_id=%s: %v", conversationID, err)
		message := clientErrorMessage(err)
		if message == "an internal error occurred processing this request" {
			message = "Couldn't compact the chat right now. Try again in a moment."
		}
		http.Error(w, message, http.StatusBadGateway)
		return
	}
	if err := s.Conversations.SetSummary(ctx, identity.UserID, conversationID, conversation.Summary{Text: text, CoversThrough: keep, UpdatedAt: time.Now()}); err != nil {
		log.Printf("server: compact: store summary for conversation_id=%s: %v", conversationID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(compactResponse{CompactedThroughID: history[keep-1].ID})
}

// compactedThroughID is the last message a chat's summary covers, for the
// "compacted" divider in the thread; 0 when nothing is summarized.
func (s *Server) compactedThroughID(r *http.Request, userID, conversationID string) int64 {
	state, err := s.Conversations.GetSummary(r.Context(), userID, conversationID)
	if err != nil || state.CoversThrough <= 0 {
		return 0
	}
	rest, err := s.Conversations.History(r.Context(), userID, conversationID, state.CoversThrough-1)
	if err != nil || len(rest) == 0 {
		if err != nil && !errors.Is(err, conversation.ErrConversationNotFound) {
			log.Printf("server: compacted divider for conversation_id=%s: %v", conversationID, err)
		}
		return 0
	}
	return rest[0].ID
}
