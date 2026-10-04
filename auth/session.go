package auth

import (
	"context"
	"strings"
	"time"
)

// Browser sign-in. The web UI doesn't keep the user's API key: signing in
// exchanges it for a session, a separate random token the server hands
// back as an HttpOnly cookie (see server/session.go). A script running in
// the page can't read that cookie, closing the tab doesn't end it, and
// logging out ends it on the server without touching the key itself.
//
// A session lasts SessionTTL from its last use, and dies with its API key:
// every lookup joins back to api_keys, so deleting a key signs out every
// browser that used it.

const (
	// SessionTTL is how long a session stays valid without being used.
	SessionTTL = 30 * 24 * time.Hour

	// sessionExtendEvery is how often a session in use gets its expiry
	// pushed back out to SessionTTL: at most once a day, so the expiry
	// keeps sliding without a database write on every request.
	sessionExtendEvery = 24 * time.Hour

	// MaxSessionsPerKey bounds the sessions one API key can hold; signing
	// in past it ends that key's oldest session.
	MaxSessionsPerKey = 20

	// sessionTokenPrefix tells a session token from an API key (nc_) at a
	// glance. The two are not interchangeable: a session token is only
	// accepted from the cookie, an API key never is.
	sessionTokenPrefix = "ncs_"
)

// Session is a signed-in browser.
type Session struct {
	Identity
	ExpiresAt time.Time
	// Extended reports that this lookup pushed ExpiresAt out, so the
	// cookie carrying the session should be sent again with the new expiry.
	Extended bool
}

// SessionStore keeps browser sessions. Every method treats an unknown,
// expired or malformed token the same way: ErrInvalidToken.
type SessionStore interface {
	// CreateSession signs in with an API key and returns the new session's
	// token -- the only copy, like IssueKey's. ErrInvalidToken means the
	// key isn't valid.
	CreateSession(ctx context.Context, apiKey string) (token string, session Session, err error)

	// ResumeSession resolves a session token, extending the session when
	// it's due (see sessionExtendEvery).
	ResumeSession(ctx context.Context, token string) (Session, error)

	// EndSession deletes a session. Ending one that doesn't exist is not
	// an error: logging out twice is still logged out.
	EndSession(ctx context.Context, token string) error

	// SweepSessions deletes sessions that expired by now and sessions
	// whose API key is gone, and reports how many it deleted.
	SweepSessions(ctx context.Context, now time.Time) (int64, error)
}

// generateSessionToken returns a fresh session token: sessionTokenPrefix
// plus the same 256 bits of entropy an API key carries.
func generateSessionToken() (string, error) {
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	return sessionTokenPrefix + strings.TrimPrefix(token, tokenPrefix), nil
}

func validSessionToken(token string) bool {
	return strings.HasPrefix(token, sessionTokenPrefix) && len(token) > len(sessionTokenPrefix)
}

// sessionDue reports whether a session expiring at expiresAt is due to be
// extended at now.
func sessionDue(expiresAt, now time.Time) bool {
	return expiresAt.Before(now.Add(SessionTTL - sessionExtendEvery))
}
