package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"neochat/imagegen"
	"neochat/limits"
	"neochat/router"
)

// mp4Of builds an MP4 file whose movie header says it lasts seconds.
func mp4Of(seconds uint32, filler int) []byte {
	box := func(boxType string, payload []byte) []byte {
		out := make([]byte, 8, 8+len(payload))
		binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
		copy(out[4:], boxType)
		return append(out, payload...)
	}
	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[12:], 1000)
	binary.BigEndian.PutUint32(mvhd[16:], seconds*1000)
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00"))
	return append(append(ftyp, box("mdat", make([]byte, filler))...), box("moov", box("mvhd", mvhd))...)
}

// videoStandIn imitates the Media API for video: each POST makes a clip,
// ready after one poll, priced at 63 RUB.
type videoStandIn struct {
	mu      sync.Mutex
	created []map[string]any
	shots   int
}

func (v *videoStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/media":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		v.created = append(v.created, body)
		v.shots++
		json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("aig_%d", v.shots), "status": "pending"})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/media/aig_"):
		n := strings.TrimPrefix(r.URL.Path, "/v1/media/aig_")
		json.NewEncoder(w).Encode(map[string]any{"id": "aig_" + n, "status": "completed", "data": map[string]any{"video": map[string]any{"url": "http://" + r.Host + "/clips/" + n + ".mp4"}}, "usage": map[string]any{"cost_rub": 63}})
	case strings.HasPrefix(r.URL.Path, "/clips/"):
		w.Write(mp4Of(6, 256))
	default:
		http.NotFound(w, r)
	}
}

func (v *videoStandIn) last(t *testing.T) map[string]any {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.created) == 0 {
		t.Fatal("no generation reached the Media API")
	}
	return v.created[len(v.created)-1]
}

func newVideoServer(t *testing.T) (*Server, *videoStandIn, *polzaStandIn) {
	t.Helper()
	chat := &polzaStandIn{reply: "answer"}
	s := newDirectServer(t, chat)
	media := &videoStandIn{}
	vendor := httptest.NewServer(media)
	t.Cleanup(vendor.Close)
	s.Images = &imagegen.Client{BaseURL: vendor.URL + "/v1", PollInterval: 5 * time.Millisecond, AllowPrivate: true}
	catalog := s.Router.Catalog
	catalog.Models = append(catalog.Models,
		router.Model{
			ID: "omni", DisplayName: "Gemini Omni", APIModelID: "google/gemini-omni-1.1-flash", Provider: "google", Kind: router.KindVideo, ManualOnly: true,
			Modes: []string{"max"}, MaxReferenceImages: 7, InputModalities: []string{"text", "image", "video"}, ContextWindow: 65536,
			Video: &router.VideoOptions{
				Durations: []string{"4", "6", "8", "10"}, Resolutions: []string{"360p", "720p", "1080p", "4k"}, AspectRatios: []string{"16:9", "9:16"}, MaxReferenceVideos: 1,
				Prices: []router.VideoPrice{{RUB: 189}, {When: map[string]string{"duration": "6", "has_video": "false"}, RUB: 63}, {When: map[string]string{"has_video": "true"}, RUB: 126}},
			},
		},
		router.Model{ID: "gemini-flash", DisplayName: "Gemini Flash", APIModelID: "google/gemini-3.8-flash", Provider: "google", Modes: []string{"instant"}, InputModalities: []string{"text", "image", "file", "video"}, CostInputPerMTok: 0.3, CostOutputPerMTok: 2, ContextWindow: 1_000_000, MaxOutputTokens: 16000},
	)
	s.Router = router.NewRouter(catalog, testWeights())
	s.Generators["google"] = s.Generators["openai"]
	return s, media, chat
}

func videoTurn(extra map[string]any) map[string]any {
	body := map[string]any{"requested_mode": "manual", "manual_model_id": "omni"}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestVideoModelMakesAClipAndKeepsItWithTheChat(t *testing.T) {
	s, media, chat := newVideoServer(t)
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	key := map[string]string{imageKeyHeader: "pza_media"}

	first := doneEvent(t, sendStream(t, mux, "/chat/stream", token, key, videoTurn(map[string]any{
		"message": "кот прыгает через лужу", "video": map[string]any{"duration": "6", "resolution": "1080p", "aspect_ratio": "9:16"},
	})))
	if len(first.Videos) != 1 || len(first.Images) != 0 {
		t.Fatalf("done = %+v", first)
	}
	clip := first.Videos[0]
	if clip.Kind != "video" || clip.MIME != "video/mp4" || clip.Width != 1080 || clip.Height != 1920 || clip.Seconds != 6 || !strings.HasSuffix(clip.Name, ".mp4") {
		t.Fatalf("clip = %+v", clip)
	}
	created := media.last(t)
	input := created["input"].(map[string]any)
	if created["async"] != true || created["model"] != "google/gemini-omni-1.1-flash" || input["prompt"] != "кот прыгает через лужу" ||
		input["duration"] != "6" || input["resolution"] != "1080p" || input["aspect_ratio"] != "9:16" || input["videos"] != nil || input["images"] != nil {
		t.Fatalf("media request = %v", created)
	}
	if len(chat.bodies) != 0 {
		t.Fatal("a video request reached the chat API")
	}
	// Billed what Polza said, to the Thinking+Max pool.
	spent, _ := s.Store.(*limits.InMemorySpendStore).Sum(context.Background(), "u1", limits.PoolThinkingMax, time.Hour)
	if want := 63 / rubPerUSD; math.Abs(spent-want) > 1e-9 {
		t.Fatalf("spent %.6f, want %.6f", spent, want)
	}

	// Kept with the chat, served as a video, back with the thread.
	stored, err := s.Attachments.Get(context.Background(), "u1", clip.ID)
	if err != nil || stored.ConversationID != first.ConversationID {
		t.Fatalf("stored clip = %+v, %v", stored.ConversationID, err)
	}
	request := httptest.NewRequest(http.MethodGet, "/files/"+clip.ID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("Content-Type") != "video/mp4" || !strings.HasPrefix(response.Header().Get("Content-Disposition"), "inline") || !bytes.Equal(response.Body.Bytes(), stored.Data) {
		t.Fatalf("GET /files: %d %q %q", response.Code, response.Header().Get("Content-Type"), response.Header().Get("Content-Disposition"))
	}
	history, _ := s.Conversations.History(context.Background(), "u1", first.ConversationID, 0)
	if len(history) != 2 || len(history[1].Versions[0].Videos) != 1 || history[1].Versions[0].Videos[0].ID != clip.ID {
		t.Fatalf("history = %+v", history)
	}

	// A follow-up starts over; it doesn't edit the last clip by itself.
	doneEvent(t, sendStream(t, mux, "/chat/stream", token, key, videoTurn(map[string]any{"conversation_id": first.ConversationID, "message": "теперь собака"})))
	if input := media.last(t)["input"].(map[string]any); input["videos"] != nil || input["duration"] != "4" || input["resolution"] != "720p" || input["aspect_ratio"] != "16:9" {
		t.Fatalf("follow-up input = %v (want the defaults, no video)", input)
	}

	// The chat's clip, attached again, is edited: the model picks the
	// length; a long one is cut to its first 10 seconds.
	_, long := uploadVia(t, mux, token, "trip.mp4", mp4Of(25, 512))
	_, photo := uploadVia(t, mux, token, "photo.png", pngOf(t, 8, 8, 99))
	if long.Kind != "video" || long.MIME != "video/mp4" {
		t.Fatalf("upload = %+v", long)
	}
	edited := doneEvent(t, sendStream(t, mux, "/chat/stream", token, key, videoTurn(map[string]any{
		"conversation_id": first.ConversationID, "message": "добавь динамики", "attachments": []string{long.ID, photo.ID}, "video": map[string]any{"duration": "8"},
	})))
	if len(edited.Videos) != 1 {
		t.Fatalf("edited = %+v", edited)
	}
	input = media.last(t)["input"].(map[string]any)
	attached, _ := s.Attachments.Get(context.Background(), "u1", long.ID)
	videos, images := input["videos"].([]any), input["images"].([]any)
	if len(videos) != 1 || videos[0].(map[string]any)["data"] != "data:video/mp4;base64,"+base64.StdEncoding.EncodeToString(attached.Data) || len(images) != 1 ||
		input["duration"] != nil || input["video_start"] != float64(0) || input["video_end"] != float64(10) {
		t.Fatalf("edit input: %d videos, %d images, duration %v, %v-%v", len(videos), len(images), input["duration"], input["video_start"], input["video_end"])
	}

	// Regenerating edits the same files again, with the choice sent now.
	again := doneEvent(t, sendStream(t, mux, "/chat/regenerate/stream", token, key, videoTurn(map[string]any{
		"conversation_id": first.ConversationID, "video": map[string]any{"resolution": "360p", "aspect_ratio": "9:16"},
	})))
	input = media.last(t)["input"].(map[string]any)
	if len(again.Videos) != 1 || again.Videos[0].ID == edited.Videos[0].ID || len(input["videos"].([]any)) != 1 || input["resolution"] != "360p" || input["aspect_ratio"] != "9:16" {
		t.Fatalf("regenerated = %+v, input resolution %v shape %v", again.Videos, input["resolution"], input["aspect_ratio"])
	}
	history, _ = s.Conversations.History(context.Background(), "u1", first.ConversationID, 0)
	if versions := history[len(history)-1].Versions; len(versions) != 2 || versions[1].Videos[0].ID != again.Videos[0].ID {
		t.Fatalf("versions after regenerate = %+v", versions)
	}
}

func TestVideoModelRefusals(t *testing.T) {
	s, media, _ := newVideoServer(t)
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	key := map[string]string{imageKeyHeader: "pza_media"}
	cases := []struct {
		name    string
		headers map[string]string
		body    map[string]any
		want    string
	}{
		{"no image key", map[string]string{providerKeyHeader: "pza_chat"}, videoTurn(map[string]any{"message": "кот"}), "Image API key in Settings → Account to make videos"},
		{"no prompt", key, videoTurn(map[string]any{"message": ""}), "message is required"},
		{"length it doesn't offer", key, videoTurn(map[string]any{"message": "кот", "video": map[string]any{"duration": "7"}}), "doesn't offer length 7. Pick one of: 4, 6, 8, 10."},
		{"shape it doesn't offer", key, videoTurn(map[string]any{"message": "кот", "video": map[string]any{"aspect_ratio": "1:1"}}), "doesn't offer shape 1:1."},
	}
	for _, tc := range cases {
		if out := sendStream(t, mux, "/chat/stream", token, tc.headers, tc.body); !strings.Contains(out, tc.want) {
			t.Fatalf("%s: %q", tc.name, out)
		}
	}
	_, notes := uploadVia(t, mux, token, "notes.txt", []byte("заметки"))
	if out := sendStream(t, mux, "/chat/stream", token, key, videoTurn(map[string]any{"message": "кот", "attachments": []string{notes.ID}})); !strings.Contains(out, "works from pictures and videos only") {
		t.Fatalf("text attachment: %s", out)
	}
	_, a := uploadVia(t, mux, token, "a.mp4", mp4Of(5, 64))
	_, b := uploadVia(t, mux, token, "b.mp4", mp4Of(5, 64))
	if out := sendStream(t, mux, "/chat/stream", token, key, videoTurn(map[string]any{"message": "кот", "attachments": []string{a.ID, b.ID}})); !strings.Contains(out, "takes one video at a time") {
		t.Fatalf("two videos: %s", out)
	}
	if len(media.created) != 0 {
		t.Fatalf("%d refused requests reached the Media API", len(media.created))
	}
}

func TestGeminiChatWatchesAnAttachedVideo(t *testing.T) {
	s, _, chat := newVideoServer(t)
	mux := s.Mux()
	token := issueTestKey(t, s, "u1")
	data := mp4Of(3, 64)
	copy(data[8:12], "qt  ")
	_, clip := uploadVia(t, mux, token, "clip.mov", data)
	if clip.Kind != "video" || clip.MIME != "video/quicktime" {
		t.Fatalf("upload = %+v", clip)
	}
	headers := map[string]string{providerKeyHeader: "pza_chat"}
	done := doneEvent(t, sendStream(t, mux, "/chat/stream", token, headers, map[string]any{"message": "что на видео?", "requested_mode": "manual", "manual_model_id": "gemini-flash", "attachments": []string{clip.ID}}))
	messages := chat.lastBody(t)["messages"].([]any)
	parts := messages[len(messages)-1].(map[string]any)["content"].([]any)
	video := parts[0].(map[string]any)
	if video["type"] != "video_url" || video["video_url"].(map[string]any)["url"] != "data:video/mov;base64,"+base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("first part = %v", video["type"])
	}
	// A model that can't watch videos says so before anything is sent.
	calls := len(chat.bodies)
	if out := sendStream(t, mux, "/chat/stream", token, headers, map[string]any{"conversation_id": done.ConversationID, "message": "что на видео?", "requested_mode": "manual", "manual_model_id": "gpt-6-luna", "attachments": []string{clip.ID}}); !strings.Contains(out, "can't watch videos") {
		t.Fatalf("GPT with a video: %q", out)
	}
	if len(chat.bodies) != calls {
		t.Fatal("a video reached a model that can't watch it")
	}
}
