package attachment

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 64)...)
	pdfBytes  = []byte("%PDF-1.4\n1 0 obj << /Type /Page >> endobj\n%%EOF")
	docxBytes = append([]byte("PK\x03\x04\x14\x00\x00\x00"), []byte("....word/document.xml....")...)
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		data       []byte
		kind, mime string
	}{
		{"png", "a.png", pngBytes, KindImage, "image/png"},
		{"png with a lying name", "a.txt", pngBytes, KindImage, "image/png"},
		{"pdf", "report.pdf", pdfBytes, KindDocument, "application/pdf"},
		{"docx", "notes.docx", docxBytes, KindDocument, docxMIME},
		{"code", "main.go", []byte("package main\n\nfunc main() {}\n"), KindText, "text/plain"},
		{"utf-8 text", "заметка.md", []byte("# Привет\nтекст"), KindText, "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, mime, err := Classify(tc.file, tc.data)
			if err != nil || kind != tc.kind || mime != tc.mime {
				t.Fatalf("Classify = %q %q %v, want %q %q", kind, mime, err, tc.kind, tc.mime)
			}
		})
	}
}

func TestClassifyRejects(t *testing.T) {
	if _, _, err := Classify("x.bin", []byte{0x01, 0x00, 0xff, 0x13}); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("binary: err = %v, want ErrUnsupportedType", err)
	}
	if _, _, err := Classify("archive.zip", docxBytes); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("plain zip: err = %v, want ErrUnsupportedType", err)
	}
	big := bytes.Repeat([]byte("a"), MaxTextBytes+1)
	if _, _, err := Classify("big.txt", big); !errors.Is(err, ErrTooLarge) || !strings.HasSuffix(err.Error(), "text files are limited to 512 KB") {
		t.Errorf("oversized text: err = %v, want ErrTooLarge naming 512 KB", err)
	}
	bigImage := append(append([]byte(nil), pngBytes...), make([]byte, MaxUploadBytes)...)
	if _, _, err := Classify("big.png", bigImage); !errors.Is(err, ErrTooLarge) || !strings.HasSuffix(err.Error(), "image files are limited to 20 MB") {
		t.Errorf("oversized image: err = %v, want ErrTooLarge naming 20 MB", err)
	}
}

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\me\photo.png`:  "photo.png",
		"../../etc/passwd":       "passwd",
		"a\"b\x00c\n.txt":        "abc.txt",
		"   ":                    "file",
		"..":                     "file",
		strings.Repeat("x", 300): strings.Repeat("x", 120),
	} {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("y", 300) + ".pdf"
	if got := SanitizeName(long); len([]rune(got)) != 120 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("long name kept %q", got)
	}
}

func TestInMemoryStoreScopingAndLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()
	old := time.Now().Add(-48 * time.Hour)
	for _, f := range []File{
		{ID: "a", UserID: "u1", Data: []byte("1"), CreatedAt: time.Now()},
		{ID: "b", UserID: "u1", Data: []byte("2"), CreatedAt: old},
		{ID: "c", UserID: "u2", Data: []byte("3"), CreatedAt: old},
	} {
		if err := s.Put(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Get(ctx, "u2", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's file was readable: %v", err)
	}
	if err := s.Claim(ctx, "u1", "conv1", []string{"a", "c"}); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.Get(ctx, "u2", "c"); f.ConversationID != "" {
		t.Fatal("Claim touched another user's file")
	}
	if err := s.Claim(ctx, "u1", "conv2", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.Get(ctx, "u1", "a"); f.ConversationID != "conv1" {
		t.Fatalf("a claimed file moved to %q", f.ConversationID)
	}
	if n, _ := s.CountUnclaimed(ctx, "u1"); n != 1 {
		t.Fatalf("unclaimed = %d, want 1", n)
	}
	if err := s.PruneUnclaimed(ctx, "u1", time.Now().Add(-UnclaimedTTL)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "u1", "b"); !errors.Is(err, ErrNotFound) {
		t.Fatal("stale unclaimed file survived pruning")
	}
	if _, err := s.Get(ctx, "u2", "c"); err != nil {
		t.Fatal("pruning one user deleted another user's file")
	}
	if err := s.DeleteConversation(ctx, "u1", "conv1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "u1", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleting the conversation kept its file")
	}
}

func TestInMemoryStoreSweep(t *testing.T) {
	ctx := context.Background()
	s := NewInMemoryStore()
	old := time.Now().Add(-48 * time.Hour)
	for _, f := range []File{
		{ID: "a", UserID: "u1", CreatedAt: old},
		{ID: "b", UserID: "u2", CreatedAt: old},
		{ID: "c", UserID: "u2", CreatedAt: time.Now()},
		{ID: "d", UserID: "u2", ConversationID: "chat", CreatedAt: old},
	} {
		if err := s.Put(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.Sweep(ctx, time.Now().Add(-UnclaimedTTL)); err != nil || n != 2 {
		t.Fatalf("Sweep = %d, %v; want 2 stale unclaimed files across users", n, err)
	}
}
