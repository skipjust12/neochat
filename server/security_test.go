package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"neochat/idempotency"
)

func TestSecurityRequestSlotIsPerUser(t *testing.T) {
	s := &Server{Idempotency: idempotency.NewInMemoryStore()}
	release, err := s.acquireRequest(context.Background(), chatRequest{UserID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquireRequest(context.Background(), chatRequest{UserID: "user"}); err == nil {
		t.Fatal("parallel request accepted")
	}
	other, err := s.acquireRequest(context.Background(), chatRequest{UserID: "other"})
	if err != nil {
		t.Fatal(err)
	}
	other()
	release()
	next, err := s.acquireRequest(context.Background(), chatRequest{UserID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	next()
}

func TestSecurityTrustForwardedHeadersOnlyFromConfiguredProxy(t *testing.T) {
	s := &Server{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}, RequireHTTPS: true}
	req := httptest.NewRequest("GET", "http://example.test/", nil)
	req.RemoteAddr = "198.51.100.5:8000"
	req.Header.Set("X-Forwarded-For", "203.0.113.2")
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := s.requestIP(req); got != "198.51.100.5" {
		t.Fatalf("spoofed IP trusted: %s", got)
	}
	denied := httptest.NewRecorder()
	s.Mux().ServeHTTP(denied, req)
	if denied.Code != http.StatusBadRequest {
		t.Fatalf("untrusted HTTPS bypass: %d", denied.Code)
	}
	req.RemoteAddr = "192.0.2.1:8000"
	allowed := httptest.NewRecorder()
	s.Mux().ServeHTTP(allowed, req)
	if allowed.Code != http.StatusOK {
		t.Fatalf("proxy rejected: %d", allowed.Code)
	}
	if s.requestIP(req) != "203.0.113.2" {
		t.Fatal("proxy client IP not resolved")
	}
	if !strings.Contains(allowed.Header().Get("Content-Security-Policy"), "'sha256-") || allowed.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing frontend protection")
	}
}
