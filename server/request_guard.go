package server

import (
	"context"
	"errors"
	"neochat/auth"
	"neochat/idempotency"
	"net/http"
	"time"
)

// Less than idempotency's 15-minute lease, leaving time for bounded cleanup.
const requestTimeout = 12 * time.Minute

// acquireRequest permits one active chat per user across server instances.
// Client idempotency keys use a different namespace in the same store.
func (s *Server) acquireRequest(ctx context.Context, req chatRequest) (func(), error) {
	if s.Idempotency == nil {
		return func() {}, nil
	}
	lease, _, err := s.Idempotency.Reserve(ctx, req.UserID, "active-request")
	if err != nil {
		return nil, err
	}
	return func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.Idempotency.Release(cleanup, req.UserID, "active-request", lease.Owner)
	}, nil
}

func (s *Server) withRequestSlot(next identityHandler) identityHandler {
	return func(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		release, err := s.acquireRequest(ctx, chatRequest{UserID: identity.UserID})
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, idempotency.ErrInFlight) {
				status = http.StatusTooManyRequests
			}
			http.Error(w, "request temporarily unavailable", status)
			return
		}
		defer release()
		next(w, r.WithContext(ctx), identity)
	}
}
