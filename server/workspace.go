package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"neochat/auth"
	"neochat/conversation"
)

// maxConversationList is how many chats GET /conversations returns: in
// practice all of them (the list used to stop at 100, and older chats
// simply vanished from the sidebar).
const maxConversationList = 10000

// maxSearchResults bounds GET /conversations/search.
const maxSearchResults = 50

type searchResponse struct {
	Results []conversation.SearchResult `json:"results"`
}

// handleConversationSearch finds chats by what's in them: titles and the
// text of every message, projects included.
func (s *Server) handleConversationSearch(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	query := strings.Join(strings.Fields(r.URL.Query().Get("q")), " ")
	if n := len([]rune(query)); n < 2 || n > 200 {
		http.Error(w, "search for 2 to 200 characters", http.StatusBadRequest)
		return
	}
	results, err := s.Conversations.Search(r.Context(), identity.UserID, query, maxSearchResults)
	if err != nil {
		log.Printf("server: search conversations for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(searchResponse{Results: results}); err != nil {
		log.Printf("server: encode search results: %v", err)
	}
}

// handleProjectDelete removes a project; its chats move back to the
// general list rather than going with it.
func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	id := strings.TrimSpace(r.PathValue("project_id"))
	err := s.Conversations.DeleteProject(r.Context(), identity.UserID, id)
	if errors.Is(err, conversation.ErrConversationNotFound) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("server: delete project_id=%s for user_id=%s: %v", id, identity.UserID, err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Poe usage endpoints are outside /v1 and report points, never currency.
const defaultBalanceURL = "https://api.poe.com/usage/current_balance"

type balanceResponse struct {
	CurrentPointBalance int64  `json:"current_point_balance"`
	Unit                string `json:"unit"`
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	key := strings.TrimSpace(r.Header.Get(providerKeyHeader))
	if key == "" || len(key) > 512 || strings.ContainsAny(key, " \t\r\n") {
		http.Error(w, "Add your Poe API key in Settings → Account to see the balance.", http.StatusBadRequest)
		return
	}
	endpoint := s.BalanceURL
	if endpoint == "" {
		endpoint = defaultBalanceURL
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		http.Error(w, "Could not create balance request.", http.StatusInternalServerError)
		return
	}
	request.Header.Set("Authorization", "Bearer "+key)
	client := *http.DefaultClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		log.Printf("server: Poe balance user_id=%s: %v", identity.UserID, err)
		http.Error(w, "Couldn't reach Poe. Try again in a moment.", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		http.Error(w, "Could not read Poe balance.", http.StatusBadGateway)
		return
	}
	if response.StatusCode != http.StatusOK {
		http.Error(w, "Poe could not report the balance. Check your API key and try again.", http.StatusBadGateway)
		return
	}
	var parsed struct {
		Balance *int64 `json:"current_point_balance"`
	}
	if err = json.Unmarshal(body, &parsed); err != nil || parsed.Balance == nil {
		http.Error(w, "Poe sent a balance this server couldn't read.", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err = json.NewEncoder(w).Encode(balanceResponse{CurrentPointBalance: *parsed.Balance, Unit: "points"}); err != nil {
		log.Printf("server: encode balance: %v", err)
	}
}

// Artifacts: a model's complete HTML page (a ```html block holding a whole
// document) opens full screen in the UI. It runs in an iframe sandboxed
// without allow-same-origin, so it gets an opaque origin -- no cookies, no
// storage, nothing of this site -- and its HTML arrives by postMessage
// into this host page, whose own CSP is what the page runs under: inline
// code and the common CDNs allowed, no network requests of its own, and
// only this site may frame it.
const artifactFrameCSP = "default-src 'none'; " +
	"script-src 'unsafe-inline' 'unsafe-eval' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com https://unpkg.com https://cdn.tailwindcss.com; " +
	"style-src 'unsafe-inline' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com https://unpkg.com https://fonts.googleapis.com; " +
	"font-src data: https://fonts.gstatic.com https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; " +
	"img-src data: blob: https:; media-src data: blob: https:; connect-src 'none'; " +
	"form-action 'none'; base-uri 'none'; frame-ancestors 'self'"

const artifactFrameHTML = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head><body><script>
addEventListener('message',function load(event){
  if(event.source!==parent||!event.data||event.data.type!=='neochat-artifact')return;
  removeEventListener('message',load);
  document.open();document.write(String(event.data.html));document.close();
});
parent.postMessage({type:'neochat-artifact-ready'},'*');
</script></body></html>`

func artifactFrame(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", artifactFrameCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, artifactFrameHTML)
}
