package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// tokenPrefix marks a string as a neochat API key at a glance -- in logs,
// in a pasted support message, in a git-history scan -- the same purpose
// Stripe's sk_/GitHub's ghp_ prefixes serve. Not a security boundary by
// itself.
const tokenPrefix = "nc_"

// tokenRandomBytes is the raw entropy encoded into every issued token.
// 32 bytes (256 bits) is comfortably beyond brute-force range for an
// opaque bearer credential.
const tokenRandomBytes = 32

// generateToken returns a fresh, random bearer token (tokenPrefix + 64
// hex chars). Only the caller ever sees the raw value -- see hashToken
// for what actually gets persisted.
func generateToken() (string, error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return tokenPrefix + hex.EncodeToString(buf), nil
}

// hashToken returns the SHA-256 hex digest stored/looked up in place of
// the raw token -- api_keys (and InMemoryStore) never hold a value that
// is itself usable as a credential, so a database read (a backup, a
// leaked dump, a stray log line from a different bug) doesn't hand out
// live access the way storing raw tokens would. A bearer token generated
// with tokenRandomBytes of real entropy has no meaningful risk of offline
// dictionary attack against the hash, unlike a user-chosen password --
// this is not a case that needs a slow/salted KDF.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
