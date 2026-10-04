package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"neochat/auth"
	"neochat/conversation"
)

func newSessionTestServer(t *testing.T) (*Server, *auth.InMemoryStore, string) {
	t.Helper()
	store := auth.NewInMemoryStore()
	key, err := store.IssueKey(context.Background(), "u1", "pro")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Auth: store, Sessions: store, Conversations: conversation.NewInMemoryStore()}, store, key
}

type sessionCall struct {
	method, path, body string
	cookie             *http.Cookie
	bearer             string
	noHeader           bool
	https              bool
}

func (c sessionCall) do(srv *Server) *httptest.ResponseRecorder {
	req := httptest.NewRequest(c.method, "http://neochat.test"+c.path, strings.NewReader(c.body))
	req.RemoteAddr = "192.0.2.1:5000"
	if c.https {
		req.Header.Set("X-Forwarded-Proto", "https")
	}
	if !c.noHeader {
		req.Header.Set(sessionHeader, "1")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	rec := httptest.NewRecorder()
	srv.Mux().ServeHTTP(rec, req)
	return rec
}

func responseCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response sets no %s cookie; Set-Cookie = %q", name, rec.Header().Values("Set-Cookie"))
	return nil
}

func signIn(t *testing.T, srv *Server, key string) *http.Cookie {
	t.Helper()
	rec := sessionCall{method: "POST", path: "/auth/session", body: `{"api_key":"` + key + `"}`}.do(srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in: %d %s", rec.Code, rec.Body)
	}
	return responseCookie(t, rec, sessionCookieName)
}

func TestSessionSignInSetsAProtectedCookie(t *testing.T) {
	srv, _, key := newSessionTestServer(t)

	rec := sessionCall{method: "POST", path: "/auth/session", body: `{"api_key":" ` + key + ` "}`}.do(srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.SignedIn || body.UserID != "u1" || body.ExpiresAt == nil {
		t.Fatalf("body = %s, %v", rec.Body, err)
	}
	if d := time.Until(*body.ExpiresAt); d < auth.SessionTTL-time.Minute || d > auth.SessionTTL {
		t.Fatalf("expires_at = %v", body.ExpiresAt)
	}
	cookie := responseCookie(t, rec, sessionCookieName)
	if !strings.HasPrefix(cookie.Value, "ncs_") || cookie.Value == key {
		t.Fatalf("cookie carries %q, want a session token, never the key", cookie.Value)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Secure {
		t.Fatalf("cookie attributes = %+v", cookie)
	}
	if cookie.MaxAge < int((auth.SessionTTL-time.Minute)/time.Second) {
		t.Fatalf("cookie Max-Age = %d, want about %v: closing the tab must not sign out", cookie.MaxAge, auth.SessionTTL)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestSessionSignInRefusals(t *testing.T) {
	srv, _, key := newSessionTestServer(t)
	for name, c := range map[string]sessionCall{
		"unknown key":    {body: `{"api_key":"nc_never-issued"}`},
		"empty key":      {body: `{"api_key":""}`},
		"no header":      {body: `{"api_key":"` + key + `"}`, noHeader: true},
		"not json":       {body: key},
		"oversized body": {body: `{"api_key":"` + strings.Repeat("x", maxSessionRequestBytes) + `"}`},
	} {
		c.method, c.path = "POST", "/auth/session"
		rec := c.do(srv)
		want := map[string]int{"unknown key": 401, "empty key": 401, "no header": 403}[name]
		if want == 0 {
			want = 400
		}
		if rec.Code != want || len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: status %d, cookies %v; want %d and no cookie", name, rec.Code, rec.Result().Cookies(), want)
		}
	}

	srv.Sessions = nil
	if rec := (sessionCall{method: "POST", path: "/auth/session", body: `{"api_key":"` + key + `"}`}).do(srv); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without a session store: %d", rec.Code)
	}
}

func TestSessionCookieAuthenticatesRequests(t *testing.T) {
	srv, store, key := newSessionTestServer(t)
	cookie := signIn(t, srv, key)

	if rec := (sessionCall{method: "GET", path: "/conversations", cookie: cookie}).do(srv); rec.Code != http.StatusOK {
		t.Fatalf("GET /conversations with the cookie: %d %s", rec.Code, rec.Body)
	}
	// Without the header a cookie alone counts for nothing: that's what a
	// forged cross-site request would look like.
	if rec := (sessionCall{method: "GET", path: "/conversations", cookie: cookie, noHeader: true}).do(srv); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie without %s: %d, want 403", sessionHeader, rec.Code)
	}
	// API clients keep using the key; an Authorization header always wins
	// over a cookie, so a bad key isn't rescued by one.
	if rec := (sessionCall{method: "GET", path: "/conversations", bearer: key, noHeader: true}).do(srv); rec.Code != http.StatusOK {
		t.Fatalf("Bearer key: %d", rec.Code)
	}
	if rec := (sessionCall{method: "GET", path: "/conversations", bearer: "nc_wrong", cookie: cookie}).do(srv); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad Bearer with a good cookie: %d, want 401", rec.Code)
	}
	// The session token isn't an API key.
	if rec := (sessionCall{method: "GET", path: "/conversations", bearer: cookie.Value}).do(srv); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session token as Bearer: %d, want 401", rec.Code)
	}
	// A made-up cookie is refused and cleared.
	rec := sessionCall{method: "GET", path: "/conversations", cookie: &http.Cookie{Name: sessionCookieName, Value: "ncs_forged"}}.do(srv)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "sign in again") {
		t.Fatalf("forged cookie: %d %s", rec.Code, rec.Body)
	}
	if cleared := responseCookie(t, rec, sessionCookieName); cleared.MaxAge >= 0 {
		t.Fatalf("forged cookie not cleared: %+v", cleared)
	}

	// Deleting the key signs the browser out.
	store.RevokeKey(key)
	if rec := (sessionCall{method: "GET", path: "/conversations", cookie: cookie}).do(srv); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie after the key was deleted: %d, want 401", rec.Code)
	}
}

func TestSessionCheckAndLogOut(t *testing.T) {
	srv, _, key := newSessionTestServer(t)

	// Not being signed in is an answer, not an error: every landing page
	// visit asks.
	if rec := (sessionCall{method: "GET", path: "/auth/session"}).do(srv); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"signed_in":false}` {
		t.Fatalf("GET /auth/session signed out: %d %s", rec.Code, rec.Body)
	}
	cookie := signIn(t, srv, key)
	rec := sessionCall{method: "GET", path: "/auth/session", cookie: cookie}.do(srv)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"signed_in":true,"user_id":"u1"`) {
		t.Fatalf("GET /auth/session: %d %s", rec.Code, rec.Body)
	}
	if again := responseCookie(t, rec, sessionCookieName); again.Value != cookie.Value || again.MaxAge <= 0 || !again.HttpOnly {
		t.Fatalf("session check re-sent %+v, want the same session with its expiry", again)
	}
	if rec := (sessionCall{method: "GET", path: "/auth/session", cookie: cookie, noHeader: true}).do(srv); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /auth/session without %s: %d", sessionHeader, rec.Code)
	}

	// Logging out needs the header too, so another site can't do it.
	if rec := (sessionCall{method: "DELETE", path: "/auth/session", cookie: cookie, noHeader: true}).do(srv); rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE without %s: %d", sessionHeader, rec.Code)
	}
	rec = sessionCall{method: "DELETE", path: "/auth/session", cookie: cookie}.do(srv)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /auth/session: %d", rec.Code)
	}
	if cleared := responseCookie(t, rec, sessionCookieName); cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Fatalf("log out left the cookie: %+v", cleared)
	}
	// A copy of the cookie kept from before is dead on the server too.
	if rec := (sessionCall{method: "GET", path: "/conversations", cookie: cookie}).do(srv); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie after log out: %d, want 401", rec.Code)
	}
	if rec := (sessionCall{method: "DELETE", path: "/auth/session"}).do(srv); rec.Code != http.StatusNoContent {
		t.Fatalf("log out while signed out: %d", rec.Code)
	}
}

func TestSessionSignInAgainReplacesTheOldSession(t *testing.T) {
	srv, _, key := newSessionTestServer(t)
	first := signIn(t, srv, key)
	rec := sessionCall{method: "POST", path: "/auth/session", body: `{"api_key":"` + key + `"}`, cookie: first}.do(srv)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	second := responseCookie(t, rec, sessionCookieName)
	if second.Value == first.Value {
		t.Fatal("sign in reused the session")
	}
	rec = sessionCall{method: "GET", path: "/auth/session", cookie: first}.do(srv)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"signed_in":false`) {
		t.Fatalf("replaced session still works: %d %s", rec.Code, rec.Body)
	}
	if cleared := responseCookie(t, rec, sessionCookieName); cleared.MaxAge >= 0 {
		t.Fatalf("dead session's cookie not cleared: %+v", cleared)
	}
	if rec := (sessionCall{method: "GET", path: "/auth/session", cookie: second}).do(srv); rec.Code != http.StatusOK {
		t.Fatalf("new session: %d", rec.Code)
	}
}

func TestSessionCookieOverHTTPSIsHostLocked(t *testing.T) {
	srv, _, key := newSessionTestServer(t)
	srv.RequireHTTPS = true
	srv.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}

	if rec := (sessionCall{method: "POST", path: "/auth/session", body: `{"api_key":"` + key + `"}`}).do(srv); rec.Code != http.StatusBadRequest {
		t.Fatalf("sign in over plain HTTP with RequireHTTPS: %d, want 400", rec.Code)
	}
	rec := sessionCall{method: "POST", path: "/auth/session", body: `{"api_key":"` + key + `"}`, https: true}.do(srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in over HTTPS: %d %s", rec.Code, rec.Body)
	}
	cookie := responseCookie(t, rec, secureSessionCookieName)
	if !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("HTTPS cookie = %+v, want Secure, HttpOnly, Path=/ and no Domain (the __Host- rules)", cookie)
	}
	if rec := (sessionCall{method: "GET", path: "/conversations", cookie: cookie, https: true}).do(srv); rec.Code != http.StatusOK {
		t.Fatalf("HTTPS cookie: %d", rec.Code)
	}
	// Over HTTPS only the __Host- cookie counts: a plain-named one could
	// have been planted by a sibling subdomain.
	planted := &http.Cookie{Name: sessionCookieName, Value: cookie.Value}
	if rec := (sessionCall{method: "GET", path: "/conversations", cookie: planted, https: true}).do(srv); rec.Code != http.StatusUnauthorized {
		t.Fatalf("plain-named cookie over HTTPS: %d, want 401", rec.Code)
	}
}

func TestCleanupSweepsSessions(t *testing.T) {
	srv, store, key := newSessionTestServer(t)
	signIn(t, srv, key)
	store.RevokeKey(key)
	srv.cleanupOnce(context.Background())
	if deleted, _ := store.SweepSessions(context.Background(), time.Now()); deleted != 0 {
		t.Fatalf("cleanup left %d dead sessions", deleted)
	}
}
