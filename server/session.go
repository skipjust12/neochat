package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"neochat/auth"
)

// Browser sessions: how the web UI stays signed in (see auth/session.go).
// POST /auth/session trades an API key for a session cookie, GET reports
// whom the cookie signs in, if anyone (and re-sends it with its current
// expiry), DELETE logs out. Every other endpoint takes the cookie in place of an
// Authorization header -- see authenticated.
//
// The cookie is HttpOnly, so no script in the page can read it;
// SameSite=Strict, so a page on another site can't make the browser send
// it; and over HTTPS it is Secure with the __Host- prefix, so only this
// exact origin can set it and a sibling subdomain can't plant its own.
// On top of that, every request that relies on the cookie must carry
// sessionHeader. An HTML form or an <img> on another origin can't add a
// header, and a script there that tries is stopped by CORS, which this
// server never grants -- so the cookie only counts on requests this app's
// own page made.
const (
	sessionCookieName       = "neochat_session"
	secureSessionCookieName = "__Host-neochat_session"
	sessionHeader           = "X-NeoChat-Request"
	maxSessionRequestBytes  = 4 << 10
	sessionExpiredMessage   = "your session has expired, sign in again"
)

type sessionCreateRequest struct {
	APIKey string `json:"api_key"`
}

// sessionResponse answers POST and GET /auth/session. A browser without a
// (live) session gets {"signed_in": false} from GET -- a normal answer for
// every visitor of the landing page, so not an error status either.
type sessionResponse struct {
	SignedIn  bool       `json:"signed_in"`
	UserID    string     `json:"user_id,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// requestIsHTTPS reports whether the browser reached us over HTTPS:
// directly, or through a trusted proxy that says so.
func (s *Server) requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || (s.trustedProxy(clientIP(r)) && r.Header.Get("X-Forwarded-Proto") == "https")
}

func (s *Server) sessionCookieName(r *http.Request) string {
	if s.requestIsHTTPS(r) {
		return secureSessionCookieName
	}
	return sessionCookieName
}

// sessionToken is the session cookie's value, or "" without one.
func (s *Server) sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(s.sessionCookieName(r))
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt) / time.Second)
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.sessionCookieName(r),
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   s.requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.sessionCookieName(r),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// sessionPreflight runs the checks every /auth/session call shares, and
// writes the error response itself when one fails.
func (s *Server) sessionPreflight(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case s.RequireHTTPS && !s.requestIsHTTPS(r):
		http.Error(w, "HTTPS is required", http.StatusBadRequest)
	case s.Sessions == nil:
		http.Error(w, "browser sign-in is not available on this server", http.StatusServiceUnavailable)
	case !hasSessionHeader(w, r):
	default:
		return true
	}
	return false
}

// hasSessionHeader checks for sessionHeader, answering 403 without it.
func hasSessionHeader(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(sessionHeader) == "1" {
		return true
	}
	http.Error(w, "requests signed in with the session cookie must send "+sessionHeader+": 1", http.StatusForbidden)
	return false
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	if !s.sessionPreflight(w, r) {
		return
	}
	var req sessionCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionRequestBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid request body, want {\"api_key\": \"nc_…\"}", http.StatusBadRequest)
		return
	}
	token, session, err := s.Sessions.CreateSession(r.Context(), strings.TrimSpace(req.APIKey))
	if errors.Is(err, auth.ErrInvalidToken) {
		http.Error(w, "invalid API key", http.StatusUnauthorized)
		return
	}
	if err != nil {
		log.Printf("server: create session: %v", err)
		http.Error(w, "an internal error occurred processing this request", http.StatusInternalServerError)
		return
	}
	// Signing in again replaces the session this browser had.
	if old := s.sessionToken(r); old != "" {
		if err := s.Sessions.EndSession(r.Context(), old); err != nil {
			log.Printf("server: end replaced session: %v", err)
		}
	}
	s.setSessionCookie(w, r, token, session.ExpiresAt)
	writeSessionResponse(w, session)
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	if !s.sessionPreflight(w, r) {
		return
	}
	token := s.sessionToken(r)
	if token == "" {
		writeSignedOut(w)
		return
	}
	session, err := s.Sessions.ResumeSession(r.Context(), token)
	if errors.Is(err, auth.ErrInvalidToken) {
		s.clearSessionCookie(w, r)
		writeSignedOut(w)
		return
	}
	if err != nil {
		log.Printf("server: resume session: %v", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	// Re-sent on every check, not only when the session was extended, so
	// the browser's copy never expires before the server's.
	s.setSessionCookie(w, r, token, session.ExpiresAt)
	writeSessionResponse(w, session)
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if !s.sessionPreflight(w, r) {
		return
	}
	// The cookie goes either way: a server-side failure must not leave the
	// browser signed in after the user logged out.
	s.clearSessionCookie(w, r)
	if token := s.sessionToken(r); token != "" {
		if err := s.Sessions.EndSession(r.Context(), token); err != nil {
			log.Printf("server: end session: %v", err)
			http.Error(w, "could not end the session on the server", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// resumeSession resolves the session cookie's token, answering for the
// handler when it can't: 401 (and the cookie cleared) for a session that
// is gone, 503 when the store failed -- a database blip must not sign the
// user out.
func (s *Server) resumeSession(w http.ResponseWriter, r *http.Request, token string) (auth.Session, bool) {
	session, err := s.Sessions.ResumeSession(r.Context(), token)
	if errors.Is(err, auth.ErrInvalidToken) {
		s.clearSessionCookie(w, r)
		http.Error(w, sessionExpiredMessage, http.StatusUnauthorized)
		return auth.Session{}, false
	}
	if err != nil {
		log.Printf("server: resume session: %v", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return auth.Session{}, false
	}
	return session, true
}

func writeSessionResponse(w http.ResponseWriter, session auth.Session) {
	expiresAt := session.ExpiresAt.UTC()
	writeSessionJSON(w, sessionResponse{SignedIn: true, UserID: session.UserID, ExpiresAt: &expiresAt})
}

func writeSignedOut(w http.ResponseWriter) {
	writeSessionJSON(w, sessionResponse{})
}

func writeSessionJSON(w http.ResponseWriter, body sessionResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("server: encode session: %v", err)
	}
}
