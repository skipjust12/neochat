package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPoeImageNonstreamResponsesAndReferences(t *testing.T) {
	picture := testPNG(t, 7, 5)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer poe-media" {
			t.Errorf("request=%s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		if b["stream"] != false || b["store"] != false || b["model"] != "nano-banana-pro" || b["aspect_ratio"] != "16:9" || b["input"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)["type"] != "input_image" {
			t.Errorf("body=%v", b)
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "image_generation_call", "result": base64.StdEncoding.EncodeToString(picture)}}})
	}))
	defer api.Close()
	c := NewPoeClient()
	c.BaseURL = api.URL
	out, err := c.Generate(context.Background(), "poe-media", Request{Model: "nano-banana-pro", Prompt: "draw", Endpoint: "/responses", AspectRatio: "16:9", References: []Reference{{MIME: "image/png", Data: picture}}})
	if err != nil || len(out.Images) != 1 || out.Images[0].Width != 7 || out.HasCost || !bytes.Equal(out.Images[0].Data, picture) {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}
func TestPoeImageMarkdownDownloadDoesNotForwardKey(t *testing.T) {
	picture := testPNG(t, 2, 3)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("API key leaked to CDN")
		}
		w.Write(picture)
	}))
	defer cdn.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "Here\n![image](" + cdn.URL + "/result.png)"}}}})
	}))
	defer api.Close()
	c := NewPoeClient()
	c.BaseURL = api.URL
	c.AllowPrivate = true
	out, err := c.Generate(context.Background(), "poe", Request{Model: "image", Prompt: "draw"})
	if err != nil || len(out.Images) != 1 || out.Text != "Here" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	c.AllowPrivate = false
	_, err = c.Generate(context.Background(), "poe", Request{Model: "image", Prompt: "draw"})
	if err == nil {
		t.Error("private download allowed in production")
	}
}
func TestPoeVideoNativePollingAndAuthenticatedContent(t *testing.T) {
	gets := 0
	video := testMP4()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer video-key" {
			t.Error("missing video auth")
		}
		switch r.URL.Path {
		case "/videos":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["seconds"] != float64(6) || b["size"] != "1280x720" || b["input_image"] != base64.StdEncoding.EncodeToString([]byte("png")) {
				t.Errorf("video body=%v", b)
			}
			fmt.Fprint(w, `{"id":"v1","status":"queued"}`)
		case "/videos/v1":
			gets++
			fmt.Fprint(w, `{"id":"v1","status":"completed"}`)
		case "/videos/v1/content":
			w.Write(video)
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	c := NewPoeClient()
	c.BaseURL = api.URL
	c.PollInterval = time.Millisecond
	out, err := c.GenerateVideo(context.Background(), "video-key", VideoRequest{Endpoint: "/videos", Model: "veo-3.1", Prompt: "run", Duration: "6", Resolution: "720p", AspectRatio: "16:9", Images: []Reference{{Data: []byte("png")}}})
	if err != nil || gets != 1 || !bytes.Equal(out.Video.Data, video) || out.HasCost {
		t.Fatalf("out=%+v gets=%d err=%v", out, gets, err)
	}
}
func TestPoeVideoEditingUsesFileParts(t *testing.T) {
	video := testMP4()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/clip.mp4" {
			if r.Header.Get("Authorization") != "" {
				t.Error("key leaked")
			}
			w.Write(video)
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["duration"] != nil || b["resolution"] != "720p" {
			t.Errorf("editing options=%v", b)
		}
		parts := b["messages"].([]any)[0].(map[string]any)["content"].([]any)
		file := parts[1].(map[string]any)
		if file["type"] != "file" || !strings.HasPrefix(file["file"].(map[string]any)["file_data"].(string), "data:video/mp4;base64,") {
			t.Errorf("file=%v", file)
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "[Video](http://" + r.Host + "/clip.mp4)"}}}})
	}))
	defer api.Close()
	c := NewPoeClient()
	c.BaseURL = api.URL
	c.AllowPrivate = true
	out, err := c.GenerateVideo(context.Background(), "poe", VideoRequest{Model: "gemini-omni-1.1-flash", Prompt: "edit", Duration: "6", Resolution: "720p", Videos: []Reference{{MIME: "video/mp4", Data: video}}})
	if err != nil || out.Video.MIME != "video/mp4" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}
