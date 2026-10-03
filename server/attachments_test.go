package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"neochat/attachment"
	"neochat/conversation"
)

var testPNG = func() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

func uploadVia(t *testing.T, mux http.Handler, token, name string, data []byte) (*httptest.ResponseRecorder, uploadResponse) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(data)
	form.Close()
	request := httptest.NewRequest(http.MethodPost, "/files", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	var uploaded uploadResponse
	if response.Code == http.StatusCreated {
		if err := json.Unmarshal(response.Body.Bytes(), &uploaded); err != nil {
			t.Fatal(err)
		}
	}
	return response, uploaded
}

func issueTestKey(t *testing.T, s *Server, userID string) string {
	t.Helper()
	key, err := s.Auth.(interface {
		IssueKey(context.Context, string, string) (string, error)
	}).IssueKey(context.Background(), userID, "pro")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// lastUserContent returns the content of the final message the vendor got.
func lastUserContent(t *testing.T, standIn *polzaStandIn) any {
	t.Helper()
	messages, _ := standIn.lastBody(t)["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("vendor got no messages")
	}
	return messages[len(messages)-1].(map[string]any)["content"]
}

func TestUploadClassifiesAndScopesFiles(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{reply: "ok"})
	mux := s.Mux()
	alice, bob := issueTestKey(t, s, "alice"), issueTestKey(t, s, "bob")

	response, image := uploadVia(t, mux, alice, `C:\pics\cat.png`, testPNG)
	if response.Code != http.StatusCreated || image.Kind != attachment.KindImage || image.MIME != "image/png" || image.Name != "cat.png" {
		t.Fatalf("image upload = %d %+v", response.Code, image)
	}
	if response, _ := uploadVia(t, mux, alice, "blob.bin", []byte{0, 1, 2, 0xff}); response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("binary upload status = %d, want 415", response.Code)
	}
	if response, _ := uploadVia(t, mux, alice, "huge.txt", bytes.Repeat([]byte("a"), attachment.MaxTextBytes+1)); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized text status = %d, want 413", response.Code)
	}
	if response, _ := uploadVia(t, mux, "", "cat.png", testPNG); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous upload status = %d, want 401", response.Code)
	}

	download := func(token, id string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/files/"+id, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		return response
	}
	own := download(alice, image.ID)
	if cache := own.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("Cache-Control = %q: a deleted chat's files must not live on in browser caches", cache)
	}
	if own.Code != http.StatusOK || own.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(own.Header().Get("Content-Disposition"), "inline") || !bytes.Equal(own.Body.Bytes(), testPNG) {
		t.Fatalf("own download = %d %v", own.Code, own.Header())
	}
	if other := download(bob, image.ID); other.Code != http.StatusNotFound {
		t.Fatalf("another user's download status = %d, want 404", other.Code)
	}
	_, page := uploadVia(t, mux, alice, "page.html", []byte("<script>alert(1)</script>"))
	html := download(alice, page.ID)
	if html.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(html.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(html.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("an uploaded HTML file must download, not render: %v", html.Header())
	}
}

func TestChatSendsImageAndPersistsAttachment(t *testing.T) {
	standIn := &polzaStandIn{reply: "a cat"}
	s := newDirectServer(t, standIn)
	_, image := uploadVia(t, s.Mux(), issueTestKey(t, s, "u1"), "cat.png", testPNG)

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "what is this?", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", Attachments: []string{image.ID}}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	parts, ok := lastUserContent(t, standIn).([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("user content = %v, want [image, text]", lastUserContent(t, standIn))
	}
	imagePart := parts[0].(map[string]any)
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(testPNG)
	if imagePart["type"] != "image_url" || imagePart["image_url"].(map[string]any)["url"] != wantURL {
		t.Fatalf("image part = %v", imagePart)
	}
	if text := parts[1].(map[string]any); text["type"] != "text" || text["text"] != "what is this?" {
		t.Fatalf("text part = %v", text)
	}

	conversationID := events["done"][0].(chatResponse).ConversationID
	history, _ := s.Conversations.History(context.Background(), "u1", conversationID, 0)
	if len(history) != 2 || len(history[0].Attachments) != 1 || history[0].Attachments[0].Name != "cat.png" {
		t.Fatalf("stored history = %+v", history)
	}
	if stored, _ := s.Attachments.Get(context.Background(), "u1", image.ID); stored.ConversationID != conversationID {
		t.Fatalf("file not claimed by the conversation: %q", stored.ConversationID)
	}

	// The next turn re-sends the image so the model can keep talking about it.
	follow := chatRequest{UserID: "u1", PlanID: "pro", ConversationID: conversationID, Message: "and its color?", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user"}
	if _, err := collectStream(s, context.Background(), follow); err != nil {
		t.Fatal(err)
	}
	messages := standIn.lastBody(t)["messages"].([]any)
	first := messages[len(messages)-3].(map[string]any)["content"].([]any)
	if first[0].(map[string]any)["type"] != "image_url" {
		t.Fatalf("history turn lost its image: %v", first)
	}

	// A text-only model can continue the chat: the old image becomes a note.
	follow.ManualModelID = "deepseek-text"
	if _, err := collectStream(s, context.Background(), follow); err != nil {
		t.Fatalf("text-only follow-up failed: %v", err)
	}
	messages = standIn.lastBody(t)["messages"].([]any)
	note := messages[len(messages)-5].(map[string]any)["content"].([]any)[0].(map[string]any)
	if note["type"] != "text" || !strings.Contains(note["text"].(string), "cat.png") {
		t.Fatalf("old image on a text-only model = %v, want a note", note)
	}
}

func TestChatRejectsImageForTextOnlyModel(t *testing.T) {
	standIn := &polzaStandIn{reply: "ok"}
	s := newDirectServer(t, standIn)
	_, image := uploadVia(t, s.Mux(), issueTestKey(t, s, "u1"), "cat.png", testPNG)

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "describe", RequestedMode: "manual", ManualModelID: "deepseek-text", providerKey: "pza_user", Attachments: []string{image.ID}}
	_, err := collectStream(s, context.Background(), req)
	var friendly userError
	if !errors.As(err, &friendly) || !strings.Contains(clientErrorMessage(err), "DeepSeek Text can't read images") {
		t.Fatalf("err = %v (%q)", err, clientErrorMessage(err))
	}
	if len(standIn.bodies) != 0 {
		t.Fatal("the vendor was called for an unreadable attachment")
	}
}

func TestChatInlinesTextFilesForAnyModel(t *testing.T) {
	standIn := &polzaStandIn{reply: "looks fine"}
	s := newDirectServer(t, standIn)
	_, code := uploadVia(t, s.Mux(), issueTestKey(t, s, "u1"), "main.go", []byte("\ufeffpackage main\n"))

	req := chatRequest{UserID: "u1", PlanID: "pro", RequestedMode: "manual", ManualModelID: "deepseek-text", providerKey: "pza_user", Attachments: []string{code.ID}}
	if _, err := collectStream(s, context.Background(), req); err != nil {
		t.Fatal(err)
	}
	parts := lastUserContent(t, standIn).([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "<file name=\"main.go\">\npackage main\n\n</file>" {
		t.Fatalf("inlined text file = %v", parts)
	}
}

func TestChatAttachmentGuards(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{reply: "ok"})
	ctx := context.Background()
	_, image := uploadVia(t, s.Mux(), issueTestKey(t, s, "u1"), "cat.png", testPNG)
	base := chatRequest{UserID: "u1", PlanID: "pro", Message: "hi", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user"}

	incognito := base
	incognito.Incognito, incognito.Attachments = true, []string{image.ID}
	if _, err := collectStream(s, ctx, incognito); !strings.Contains(clientErrorMessage(err), "incognito") {
		t.Fatalf("incognito with files: %v", err)
	}

	foreign := base
	foreign.UserID, foreign.Attachments = "u2", []string{image.ID}
	if _, err := collectStream(s, ctx, foreign); !strings.Contains(clientErrorMessage(err), "no longer available") {
		t.Fatalf("another user's file: %v", err)
	}

	first := base
	first.Attachments = []string{image.ID}
	if _, err := collectStream(s, ctx, first); err != nil {
		t.Fatal(err)
	}
	elsewhere := base
	elsewhere.ConversationID, elsewhere.Attachments = "another-chat", []string{image.ID}
	if _, err := collectStream(s, ctx, elsewhere); !strings.Contains(clientErrorMessage(err), "belongs to another chat") {
		t.Fatalf("reusing a claimed file in another chat: %v", err)
	}

	tooMany := base
	for i := 0; i <= attachment.MaxPerMessage; i++ {
		tooMany.Attachments = append(tooMany.Attachments, "id"+string(rune('a'+i)))
	}
	if _, err := collectStream(s, ctx, tooMany); !strings.Contains(clientErrorMessage(err), "up to") {
		t.Fatalf("too many files: %v", err)
	}
}

func TestRegenerateKeepsAttachmentsAndDeleteRemovesThem(t *testing.T) {
	standIn := &polzaStandIn{reply: "answer"}
	s := newDirectServer(t, standIn)
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	_, image := uploadVia(t, mux, token, "cat.png", testPNG)

	req := chatRequest{UserID: "u1", PlanID: "pro", Message: "what is this?", RequestedMode: "manual", ManualModelID: "gpt-6-luna", providerKey: "pza_user", Attachments: []string{image.ID}}
	events, err := collectStream(s, context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	conversationID := events["done"][0].(chatResponse).ConversationID

	body, _ := json.Marshal(map[string]any{"conversation_id": conversationID, "requested_mode": "manual", "manual_model_id": "gpt-6-luna"})
	request := httptest.NewRequest(http.MethodPost, "/chat/regenerate/stream", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(providerKeyHeader, "pza_user")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), "event: done") {
		t.Fatalf("regenerate failed: %s", response.Body.String())
	}
	if parts, ok := lastUserContent(t, standIn).([]any); !ok || parts[0].(map[string]any)["type"] != "image_url" {
		t.Fatalf("regenerated request lost the image: %v", lastUserContent(t, standIn))
	}

	request = httptest.NewRequest(http.MethodDelete, "/conversations/"+conversationID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", response.Code)
	}
	if _, err := s.Attachments.Get(context.Background(), "u1", image.ID); !errors.Is(err, attachment.ErrNotFound) {
		t.Fatalf("deleting the chat kept its file: %v", err)
	}
}

func TestHistoryBudgetTurnsOldFilesIntoNotes(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{reply: "ok"})
	ctx := context.Background()
	big := attachment.File{ID: "big", UserID: "u1", Name: "scan.pdf", MIME: "application/pdf", Kind: attachment.KindDocument, Size: 20 << 20, Data: []byte("%PDF")}
	if err := s.Attachments.Put(ctx, big); err != nil {
		t.Fatal(err)
	}
	budget := int64(10 << 20)
	msg := s.historyMessage(ctx, "u1", conversation.Message{Role: conversation.RoleUser, Content: "see this", Attachments: []conversation.Attachment{{ID: "big", Name: "scan.pdf", Size: 20 << 20}}}, &budget)
	if len(msg.Parts) != 2 || msg.Parts[0].Type != "text" || !strings.Contains(msg.Parts[0].Text, "scan.pdf") {
		t.Fatalf("over-budget history file = %+v, want a note", msg.Parts)
	}
}

func TestUploadShrinksLargeImages(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{reply: "ok"})
	img := image.NewNRGBA(image.Rect(0, 0, 1800, 1200))
	seed := uint32(7)
	for i := 0; i < len(img.Pix); i += 4 {
		seed = seed*1664525 + 1013904223
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = uint8(seed>>8), uint8(seed>>16), uint8(seed>>24), 0xff
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	response, uploaded := uploadVia(t, s.Mux(), issueTestKey(t, s, "u1"), "screen.png", buf.Bytes())
	if response.Code != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", response.Code, response.Body.String())
	}
	if uploaded.Name != "screen.jpg" || uploaded.MIME != "image/jpeg" || uploaded.Size >= int64(buf.Len()) {
		t.Fatalf("stored %+v from a %d-byte PNG", uploaded, buf.Len())
	}
	stored, _ := s.Attachments.Get(context.Background(), "u1", uploaded.ID)
	config, _, err := image.DecodeConfig(bytes.NewReader(stored.Data))
	if err != nil || config.Width != 1568 || config.Height != 1045 {
		t.Fatalf("stored image is %dx%d (%v), want 1568x1045", config.Width, config.Height, err)
	}
}

func TestAttachmentSweeperStopsWithContext(t *testing.T) {
	s := newDirectServer(t, &polzaStandIn{reply: "ok"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunCleanup(ctx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper did not stop when its context was canceled")
	}
}
