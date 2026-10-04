package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"neochat/auth"
	"neochat/conversation"
	"neochat/provider"
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

// defaultBalanceURL is Polza's balance endpoint (GET /v2/balance).
const defaultBalanceURL = "https://polza.ai/api/v2/balance"

type balanceResponse struct {
	// AmountRUB is the wallet; AvailableRUB what can still be spent after
	// reservations and the key's own spending limit.
	AmountRUB    float64 `json:"amount_rub"`
	AvailableRUB float64 `json:"available_rub"`
}

// handleBalance reports the Polza balance behind the user's own key
// (X-Provider-Key), for Settings -> Usage. The browser can't ask Polza
// itself: the page's CSP keeps it to this origin.
func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	key := strings.TrimSpace(r.Header.Get(providerKeyHeader))
	if key == "" || len(key) > 512 || strings.ContainsAny(key, " \t\r\n") {
		http.Error(w, "Add your Polza AI key in Settings → Account to see the balance.", http.StatusBadRequest)
		return
	}
	url := s.BalanceURL
	if url == "" {
		url = defaultBalanceURL
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		log.Printf("server: balance for user_id=%s: %v", identity.UserID, err)
		http.Error(w, "Couldn't reach Polza AI. Try again in a moment.", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		message := vendorErrorMessage(&provider.StatusError{StatusCode: response.StatusCode, Body: string(body)})
		if message == "" {
			message = "Polza AI couldn't report the balance right now."
		}
		http.Error(w, message, http.StatusBadGateway)
		return
	}
	// Polza sends the amounts as decimal strings ("1234.56000000").
	var parsed struct {
		Amount    json.RawMessage `json:"amount"`
		Available json.RawMessage `json:"available"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		http.Error(w, "Polza AI sent a balance this server couldn't read.", http.StatusBadGateway)
		return
	}
	out := balanceResponse{AmountRUB: decimal(parsed.Amount), AvailableRUB: decimal(parsed.Available)}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("server: encode balance: %v", err)
	}
}

func decimal(raw json.RawMessage) float64 {
	value, _ := strconv.ParseFloat(strings.Trim(string(raw), `" `), 64)
	return value
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
