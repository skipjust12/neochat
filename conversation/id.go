package conversation

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns a new random conversation identifier: 16 bytes of
// crypto/rand, hex-encoded. No external UUID dependency -- this repo has
// none in go.mod, and a random hex string serves exactly the same
// purpose here (an opaque, practically-unique key) without adding one.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read only fails if the OS entropy source itself is
		// broken -- not a recoverable condition, and panicking beats
		// silently handing out a low-entropy or all-zero ID that could
		// collide across conversations.
		panic("conversation: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
