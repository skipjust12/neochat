// Package attachment stores the files users attach to chat messages and
// decides which files are acceptable at all. A file is uploaded on its own
// (server's POST /files) before the message that uses it is sent, so it
// starts out unclaimed; sending a message claims it for that conversation,
// and deleting the conversation deletes it. Unclaimed files (picked, then
// never sent) are pruned after a day.
package attachment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kinds decide how a file reaches the model: images and documents go as
// native multimodal parts (the model must support them), text is inlined
// into the message, so every model can read it.
const (
	KindImage    = "image"
	KindDocument = "document"
	KindText     = "text"
)

// Per-kind size ceilings. Images stay under the strictest vendor limit
// (Anthropic: 5 MB per image); documents under the common inline-PDF
// limits; text under what still fits a model's context comfortably.
const (
	MaxImageBytes    = 5 << 20
	MaxDocumentBytes = 20 << 20
	MaxTextBytes     = 512 << 10

	// MaxUploadBytes is the largest file any kind accepts.
	MaxUploadBytes = MaxDocumentBytes

	// MaxPerMessage bounds how many files one message can carry.
	MaxPerMessage = 10

	// MaxUnclaimedPerUser bounds files uploaded but not (yet) sent, so the
	// upload endpoint can't be used as free storage.
	MaxUnclaimedPerUser = 50

	// UnclaimedTTL is how long a picked-but-never-sent file is kept.
	UnclaimedTTL = 24 * time.Hour
)

const docxMIME = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

var (
	ErrNotFound        = errors.New("attachment: not found")
	ErrUnsupportedType = errors.New("unsupported file type: send images (PNG, JPEG, GIF, WebP), PDF, DOCX, or text and code files")
	ErrTooLarge        = errors.New("file is too large")
)

// File is one stored upload. ConversationID is empty until a sent message
// claims the file.
type File struct {
	ID             string
	UserID         string
	ConversationID string
	Name           string
	MIME           string
	Kind           string
	Size           int64
	Data           []byte
	CreatedAt      time.Time
}

// Store persists uploads. Every lookup is scoped by user, so one user can
// never read or claim another user's file.
type Store interface {
	Put(ctx context.Context, f File) error
	Get(ctx context.Context, userID, id string) (File, error)
	// Claim attaches still-unclaimed files to conversationID. Files that
	// already belong to it are left alone, so a retried send is harmless.
	Claim(ctx context.Context, userID, conversationID string, ids []string) error
	DeleteConversation(ctx context.Context, userID, conversationID string) error
	// PruneUnclaimed deletes the user's unclaimed files created before cutoff.
	PruneUnclaimed(ctx context.Context, userID string, cutoff time.Time) error
	CountUnclaimed(ctx context.Context, userID string) (int, error)
}

// Classify decides what a file is from its bytes, not from the name or the
// Content-Type the browser claimed: both are trivially wrong or spoofed.
// The name only breaks the zip tie for DOCX.
func Classify(name string, data []byte) (kind, mime string, err error) {
	switch detected := http.DetectContentType(data); detected {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		kind, mime = KindImage, detected
	case "application/pdf":
		kind, mime = KindDocument, detected
	case "application/zip":
		if strings.EqualFold(filepath.Ext(name), ".docx") && bytes.Contains(data, []byte("word/document.xml")) {
			kind, mime = KindDocument, docxMIME
		}
	}
	if kind == "" && isText(data) {
		kind, mime = KindText, "text/plain"
	}
	if kind == "" {
		return "", "", ErrUnsupportedType
	}
	if limit := MaxBytes(kind); len(data) > limit {
		return "", "", fmt.Errorf("%w: %s files are limited to %d MB", ErrTooLarge, kind, limit>>20)
	}
	return kind, mime, nil
}

// MaxBytes is the size ceiling for kind.
func MaxBytes(kind string) int {
	switch kind {
	case KindImage:
		return MaxImageBytes
	case KindText:
		return MaxTextBytes
	}
	return MaxDocumentBytes
}

func isText(data []byte) bool {
	if len(data) == 0 || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	return true
}

// Text returns a text file's content without a UTF-8 byte-order mark.
func Text(f File) string {
	return strings.TrimPrefix(string(f.Data), "\uFEFF")
}

// SanitizeName keeps only the base name, drops control characters and
// caps the length, so a name is safe to echo in JSON, headers and prompts.
func SanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if runes := []rune(name); len(runes) > 120 {
		ext := []rune(filepath.Ext(name))
		if len(ext) > 12 {
			ext = nil
		}
		name = string(runes[:120-len(ext)]) + string(ext)
	}
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}
