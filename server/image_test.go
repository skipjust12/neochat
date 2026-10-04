package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"neochat/attachment"
	"neochat/conversation"
	"neochat/imagegen"
	"neochat/limits"
	"neochat/router"
)

func pngOf(t *testing.T, w, h int, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{R: shade, G: 10, B: 10, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mediaStandIn imitates Polza's Media API: every POST makes a new picture
// (a different shade each time), ready after one poll.
type mediaStandIn struct {
	t       *testing.T
	mu      sync.Mutex
	created []map[string]any
	keys    []string
	fail    map[string]any // status body to answer with instead of a picture
	code    int            // HTTP status for POST instead of a generation
	shots   int
}

func (m *mediaStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/media":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		m.created = append(m.created, body)
		m.keys = append(m.keys, r.Header.Get("Authorization"))
		if m.code != 0 {
			w.WriteHeader(m.code)
			fmt.Fprint(w, `{"error":{"code":"X","message":"отказ"}}`)
			return
		}
		m.shots++
		json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("aig_%d", m.shots), "status": "pending"})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/media/aig_"):
		if m.fail != nil {
			body := map[string]any{"id": "aig_x", "status": "failed"}
			for k, v := range m.fail {
				body[k] = v
			}
			json.NewEncoder(w).Encode(body)
			return
		}
		n := strings.TrimPrefix(r.URL.Path, "/v1/media/aig_")
		json.NewEncoder(w).Encode(map[string]any{"id": "aig_" + n, "status": "completed", "data": map[string]any{"url": "http://" + r.Host + "/files/" + n + ".png"}, "usage": map[string]any{"cost_rub": 2.9}})
	case strings.HasPrefix(r.URL.Path, "/files/"):
		var shot int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/files/"), "%d.png", &shot)
		w.Write(pngOf(m.t, 32, 16, uint8(shot*40)))
	default:
		http.NotFound(w, r)
	}
}

func (m *mediaStandIn) lastCreated(t *testing.T) map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.created) == 0 {
		t.Fatal("no generation reached the Media API")
	}
	return m.created[len(m.created)-1]
}

func newImageServer(t *testing.T) (*Server, *mediaStandIn, *polzaStandIn) {
	t.Helper()
	chat := &polzaStandIn{reply: "answer"}
	s := newDirectServer(t, chat)
	media := &mediaStandIn{t: t}
	vendor := httptest.NewServer(media)
	t.Cleanup(vendor.Close)
	s.Images = &imagegen.Client{BaseURL: vendor.URL + "/v1", PollInterval: 5 * time.Millisecond, AllowPrivate: true}
	catalog := s.Router.Catalog
	catalog.Models = append(catalog.Models, router.Model{
		ID: "nano-lite", DisplayName: "Nano Lite", APIModelID: "google/nano-lite", Provider: "google", Kind: router.KindImage,
		Modes: []string{"instant"}, CostPerImageUSD: 0.05, MaxReferenceImages: 2, InputModalities: []string{"text", "image"}, ContextWindow: 65536,
	})
	s.Router = router.NewRouter(catalog, testWeights())
	return s, media, chat
}

// sendStream posts to the real mux and returns the SSE transcript.
func sendStream(t *testing.T, mux http.Handler, path, token string, headers map[string]string, body map[string]any) string {
	t.Helper()
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	return response.Body.String()
}

func doneEvent(t *testing.T, transcript string) chatResponse {
	t.Helper()
	for _, block := range strings.Split(transcript, "\n\n") {
		if strings.HasPrefix(block, "event: done\n") {
			var done chatResponse
			if err := json.Unmarshal([]byte(strings.TrimPrefix(block, "event: done\ndata: ")), &done); err != nil {
				t.Fatal(err)
			}
			return done
		}
	}
	t.Fatalf("no done event in %s", transcript)
	return chatResponse{}
}

func imageTurn(extra map[string]any) map[string]any {
	body := map[string]any{"requested_mode": "manual", "manual_model_id": "nano-lite"}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestImageModelMakesAPictureAndKeepsItWithTheChat(t *testing.T) {
	s, media, chat := newImageServer(t)
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	key := map[string]string{imageKeyHeader: "pza_images"}

	// No chat key needed: the image key alone runs an image model.
	first := doneEvent(t, sendStream(t, mux, "/chat/stream", token, key, imageTurn(map[string]any{"message": "кот в космосе"})))
	if len(first.Images) != 1 || first.Images[0].Width != 32 || first.Images[0].Height != 16 || first.Images[0].Kind != "image" || first.ResponseText != "" {
		t.Fatalf("done = %+v", first)
	}
	created := media.lastCreated(t)
	if media.keys[0] != "Bearer pza_images" || created["model"] != "google/nano-lite" || created["input"].(map[string]any)["prompt"] != "кот в космосе" || created["input"].(map[string]any)["images"] != nil {
		t.Fatalf("media request = %v (key %q)", created, media.keys[0])
	}
	if len(chat.bodies) != 0 {
		t.Fatal("an image request reached the chat API")
	}
	stored, err := s.Attachments.Get(context.Background(), "u1", first.Images[0].ID)
	if err != nil || stored.ConversationID != first.ConversationID || stored.MIME != "image/png" {
		t.Fatalf("stored picture = %+v, %v", stored, err)
	}
	// Billed what Polza said it cost.
	spent, _ := s.Store.(*limits.InMemorySpendStore).Sum(context.Background(), "u1", limits.PoolInstant, time.Hour)
	if want := 2.9 / rubPerUSD; spent < want-1e-9 || spent > want+1e-9 {
		t.Fatalf("spent %.6f, want %.6f", spent, want)
	}

	// The picture is served like any file of the chat, and comes back with
	// the thread.
	request := httptest.NewRequest(http.MethodGet, "/files/"+first.Images[0].ID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), stored.Data) {
		t.Fatalf("GET /files: %d", response.Code)
	}
	history, _ := s.Conversations.History(context.Background(), "u1", first.ConversationID, 0)
	if len(history) != 2 || len(history[1].Versions) != 1 || len(history[1].Versions[0].Images) != 1 || history[1].Versions[0].Images[0].ID != first.Images[0].ID {
		t.Fatalf("history = %+v", history)
	}

	// A follow-up edits the last picture.
	second := doneEvent(t, sendStream(t, mux, "/chat/stream", token, key, imageTurn(map[string]any{"conversation_id": first.ConversationID, "message": "сделай темнее"})))
	refs := media.lastCreated(t)["input"].(map[string]any)["images"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["data"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(stored.Data) {
		t.Fatalf("follow-up references = %v", refs)
	}

	// An attached picture is what the model works from instead.
	_, upload := uploadVia(t, mux, token, "photo.png", pngOf(t, 8, 8, 99))
	doneEvent(t, sendStream(t, mux, "/chat/stream", token, key, imageTurn(map[string]any{"conversation_id": first.ConversationID, "message": "в стиле этого", "attachments": []string{upload.ID}})))
	refs = media.lastCreated(t)["input"].(map[string]any)["images"].([]any)
	attached, _ := s.Attachments.Get(context.Background(), "u1", upload.ID)
	if len(refs) != 1 || refs[0].(map[string]any)["data"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(attached.Data) {
		t.Fatalf("attached references = %d", len(refs))
	}

	// Regenerating makes another picture; the first one stays with its
	// version.
	regenerated := doneEvent(t, sendStream(t, mux, "/chat/regenerate/stream", token, key, imageTurn(map[string]any{"conversation_id": first.ConversationID})))
	if len(regenerated.Images) != 1 || regenerated.Images[0].ID == second.Images[0].ID {
		t.Fatalf("regenerated = %+v", regenerated.Images)
	}
	for _, id := range []string{second.Images[0].ID, regenerated.Images[0].ID} {
		if f, err := s.Attachments.Get(context.Background(), "u1", id); err != nil || f.ConversationID != first.ConversationID {
			t.Fatalf("picture %s = %+v, %v", id, f, err)
		}
	}

	// A chat model in the same chat knows a picture was made.
	sendStream(t, mux, "/chat/stream", token, map[string]string{providerKeyHeader: "pza_user"}, map[string]any{"conversation_id": first.ConversationID, "message": "опиши что получилось", "requested_mode": "manual", "manual_model_id": "gpt-6-luna", "web_search": "off"})
	for _, m := range chat.lastBody(t)["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "assistant" && !strings.Contains(fmt.Sprint(msg["content"]), "You made an image") {
			t.Fatalf("assistant image turn reads %q to a chat model", msg["content"])
		}
	}

	// Deleting the chat deletes every picture it made.
	request = httptest.NewRequest(http.MethodDelete, "/conversations/"+first.ConversationID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", response.Code)
	}
	for _, id := range []string{first.Images[0].ID, second.Images[0].ID, regenerated.Images[0].ID} {
		if _, err := s.Attachments.Get(context.Background(), "u1", id); !errors.Is(err, attachment.ErrNotFound) {
			t.Fatalf("picture %s survived the chat: %v", id, err)
		}
	}
}

func TestImageModelErrors(t *testing.T) {
	s, media, _ := newImageServer(t)
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")

	// No image key: told where to add it, nothing generated.
	if out := sendStream(t, mux, "/chat/stream", token, map[string]string{providerKeyHeader: "pza_chat"}, imageTurn(map[string]any{"message": "кот"})); !strings.Contains(out, "Add your Image API key") || len(media.created) != 0 {
		t.Fatalf("no image key: %s", out)
	}
	// A chat model still wants the chat key, not the image key.
	if out := sendStream(t, mux, "/chat/stream", token, map[string]string{imageKeyHeader: "pza_images"}, map[string]any{"message": "привет", "requested_mode": "manual", "manual_model_id": "gpt-6-luna"}); !strings.Contains(out, "Polza AI API key") {
		t.Fatalf("chat model without chat key: %s", out)
	}

	key := map[string]string{imageKeyHeader: "pza_images"}
	// A generation Polza reports as failed: its reason, and no charge.
	media.fail = map[string]any{"error": map[string]any{"code": "BAD_GATEWAY", "message": "Запрос нарушает политику"}}
	if out := sendStream(t, mux, "/chat/stream", token, key, imageTurn(map[string]any{"message": "кот"})); !strings.Contains(out, "The image couldn't be made: Запрос нарушает политику") {
		t.Fatalf("failed generation: %s", out)
	}
	if spent, _ := s.Store.(*limits.InMemorySpendStore).Sum(context.Background(), "u1", limits.PoolInstant, time.Hour); spent != 0 {
		t.Fatalf("failed generation was billed %.4f", spent)
	}
	media.fail = nil

	// A rejected key names the image key.
	media.code = http.StatusUnauthorized
	if out := sendStream(t, mux, "/chat/stream", token, key, imageTurn(map[string]any{"message": "кот"})); !strings.Contains(out, "rejected your Image API key") {
		t.Fatalf("rejected key: %s", out)
	}
	media.code = 0

	// Image models take pictures only.
	_, upload := uploadVia(t, mux, token, "notes.txt", []byte("заметки"))
	if out := sendStream(t, mux, "/chat/stream", token, key, imageTurn(map[string]any{"message": "кот", "attachments": []string{upload.ID}})); !strings.Contains(out, "works from pictures only") {
		t.Fatalf("text attachment: %s", out)
	}
}

func TestIncognitoPicturesStayUnclaimed(t *testing.T) {
	s, _, _ := newImageServer(t)
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	done := doneEvent(t, sendStream(t, mux, "/chat/stream", token, map[string]string{imageKeyHeader: "pza_images"}, imageTurn(map[string]any{"message": "кот", "incognito": true})))
	if len(done.Images) != 1 {
		t.Fatalf("done = %+v", done)
	}
	stored, err := s.Attachments.Get(context.Background(), "u1", done.Images[0].ID)
	if err != nil || stored.ConversationID != "" {
		t.Fatalf("incognito picture = %+v, %v (want unclaimed, so the sweep takes it)", stored.ConversationID, err)
	}
	var _ = io.Discard
	var _ = conversation.Attachment{}
}
