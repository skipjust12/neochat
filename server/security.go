package server

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
)

var frontendCSP = buildFrontendCSP()

func buildFrontendCSP() string {
	hashes := []string{}
	for _, script := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllSubmatch(frontendHTML, -1) {
		digest := sha256.Sum256(script[1])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'")
	}
	return "default-src 'self'; script-src " + strings.Join(hashes, " ") + "; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
}

func (s *Server) trustedProxy(ip string) bool {
	address, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	for _, prefix := range s.TrustedProxies {
		if prefix.Contains(address.Unmap()) {
			return true
		}
	}
	return false
}

func (s *Server) requestIP(r *http.Request) string {
	remote := clientIP(r)
	if !s.trustedProxy(remote) {
		return remote
	}
	// Walk from the nearest hop; untrusted clients cannot prepend a forged IP.
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(forwarded) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(forwarded[i])
		address, err := netip.ParseAddr(candidate)
		if err != nil {
			return remote
		}
		if !s.trustedProxy(candidate) {
			return address.Unmap().String()
		}
	}
	return remote
}

func (s *Server) frontend(w http.ResponseWriter, r *http.Request) {
	if s.RequireHTTPS && r.TLS == nil && !(s.trustedProxy(clientIP(r)) && r.Header.Get("X-Forwarded-Proto") == "https") {
		http.Error(w, "HTTPS is required", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Security-Policy", frontendCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(frontendHTML)
}
