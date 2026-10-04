package imagegen

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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"neochat/provider"
)

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 200, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeMedia imitates the Media API: each GET advances through statuses.
type fakeMedia struct {
	t        *testing.T
	mu       sync.Mutex
	statuses []string // returned by POST, then by each GET
	final    func(base string) map[string]any
	created  map[string]any
	auth     string
	gets     int
	picture  []byte
	postCode int
}

func (f *fakeMedia) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := "http://" + r.Host
	switch {
	case r.URL.Path == "/picture.png":
		w.Header().Set("Content-Type", "image/png")
		w.Write(f.picture)
		return
	case r.URL.Path == "/page.html":
		w.Write([]byte("<html>not a picture</html>"))
		return
	case r.Method == http.MethodPost && r.URL.Path == "/v1/media":
		f.auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&f.created)
		if f.postCode != 0 {
			w.WriteHeader(f.postCode)
			fmt.Fprint(w, `{"error":{"code":"UNAUTHORIZED","message":"Неверный ключ"}}`)
			return
		}
	case r.Method == http.MethodGet && r.URL.Path == "/v1/media/aig_1":
		f.gets++
	default:
		http.NotFound(w, r)
		return
	}
	step := f.gets
	if step >= len(f.statuses) {
		step = len(f.statuses) - 1
	}
	body := map[string]any{"id": "aig_1", "object": "media.generation", "status": f.statuses[step]}
	if f.statuses[step] != "pending" && f.statuses[step] != "processing" && f.final != nil {
		for k, v := range f.final(base) {
			body[k] = v
		}
	}
	json.NewEncoder(w).Encode(body)
}

func newFake(t *testing.T, f *fakeMedia) (*Client, *httptest.Server) {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL + "/v1", PollInterval: 10 * time.Millisecond, MaxWait: 5 * time.Second, AllowPrivate: true}, srv
}

func TestGeneratePollsDownloadsAndReadsTheSize(t *testing.T) {
	f := &fakeMedia{statuses: []string{"pending", "processing", "completed"}, picture: testPNG(t, 64, 48),
		final: func(base string) map[string]any {
			return map[string]any{"data": map[string]any{"url": base + "/picture.png"}, "usage": map[string]any{"output_units": 1, "cost_rub": 2.9}}
		}}
	client, _ := newFake(t, f)
	ref := testPNG(t, 8, 8)
	result, err := client.Generate(context.Background(), " pza_img ", Request{Model: "google/gemini-3.1-flash-lite-image", Prompt: "кот в космосе", AspectRatio: "1:1", References: []Reference{{MIME: "image/png", Data: ref}}, User: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 || result.Images[0].MIME != "image/png" || result.Images[0].Width != 64 || result.Images[0].Height != 48 || !bytes.Equal(result.Images[0].Data, f.picture) {
		t.Fatalf("images = %+v", result.Images)
	}
	if !result.HasCost || result.CostRUB != 2.9 || f.gets != 2 {
		t.Fatalf("cost = %v/%v, polls = %d", result.CostRUB, result.HasCost, f.gets)
	}
	input := f.created["input"].(map[string]any)
	images := input["images"].([]any)
	if f.auth != "Bearer pza_img" || f.created["model"] != "google/gemini-3.1-flash-lite-image" || f.created["user"] != "u1" || input["prompt"] != "кот в космосе" || input["aspect_ratio"] != "1:1" ||
		images[0].(map[string]any)["data"] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(ref) || images[0].(map[string]any)["type"] != "base64" {
		t.Fatalf("request = %v, auth %q", f.created, f.auth)
	}
}

func TestGenerateReadsOtherResultShapes(t *testing.T) {
	picture := testPNG(t, 10, 20)
	for name, final := range map[string]func(string) map[string]any{
		"output list": func(base string) map[string]any {
			return map[string]any{"output": []any{map[string]any{"url": base + "/picture.png"}}, "content": "Вот кот.", "usage": map[string]any{"cost": 5.0}}
		},
		"inline base64": func(string) map[string]any {
			return map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(picture)}}}
		},
		"data uri": func(string) map[string]any {
			return map[string]any{"data": map[string]any{"images": []any{"data:image/png;base64," + base64.StdEncoding.EncodeToString(picture)}}}
		},
	} {
		f := &fakeMedia{statuses: []string{"completed"}, picture: picture, final: final}
		client, _ := newFake(t, f)
		result, err := client.Generate(context.Background(), "k", Request{Model: "m", Prompt: "p"})
		if err != nil || len(result.Images) != 1 || result.Images[0].Height != 20 {
			t.Errorf("%s: %+v, %v", name, result, err)
		}
		if name == "output list" && (result.Text != "Вот кот." || result.CostRUB != 5 || !result.HasCost) {
			t.Errorf("%s: text %q cost %v", name, result.Text, result.CostRUB)
		}
	}
}

func TestGenerateFailures(t *testing.T) {
	// Polza refused the key.
	client, _ := newFake(t, &fakeMedia{statuses: []string{"pending"}, postCode: http.StatusUnauthorized})
	_, err := client.Generate(context.Background(), "k", Request{Model: "m", Prompt: "p"})
	var statusErr *provider.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 401 || provider.VendorMessage(statusErr.Body) != "Неверный ключ" {
		t.Fatalf("refused key: %v", err)
	}

	// The generation failed.
	client, _ = newFake(t, &fakeMedia{statuses: []string{"pending", "failed"}, final: func(string) map[string]any {
		return map[string]any{"error": map[string]any{"code": "BAD_GATEWAY", "message": "Нарушение политики контента", "metadata": map[string]any{"raw": "content policy violation"}}}
	}})
	_, err = client.Generate(context.Background(), "k", Request{Model: "m", Prompt: "p"})
	var failed *FailedError
	if !errors.As(err, &failed) || failed.Code != "BAD_GATEWAY" || failed.Message != "Нарушение политики контента (content policy violation)" {
		t.Fatalf("failed generation: %v", err)
	}

	// It never finished.
	client, _ = newFake(t, &fakeMedia{statuses: []string{"processing"}})
	client.MaxWait = 200 * time.Millisecond
	_, err = client.Generate(context.Background(), "k", Request{Model: "m", Prompt: "p"})
	var timeout *TimeoutError
	if !errors.As(err, &timeout) || timeout.ID != "aig_1" {
		t.Fatalf("slow generation: %v", err)
	}

	// The caller gave up.
	client, _ = newFake(t, &fakeMedia{statuses: []string{"processing"}})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cancel()
	if _, err = client.Generate(ctx, "k", Request{Model: "m", Prompt: "p"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}

	// No key at all.
	if _, err := (&Client{}).Generate(context.Background(), "  ", Request{}); !errors.Is(err, ErrMissingKey) {
		t.Fatalf("missing key: %v", err)
	}

	// A result that isn't a picture.
	client, _ = newFake(t, &fakeMedia{statuses: []string{"completed"}, final: func(base string) map[string]any {
		return map[string]any{"data": map[string]any{"url": base + "/page.html"}}
	}})
	if _, err := client.Generate(context.Background(), "k", Request{Model: "m", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "not an image") {
		t.Fatalf("html result: %v", err)
	}

	// Finished with nothing.
	client, _ = newFake(t, &fakeMedia{statuses: []string{"completed"}, final: func(string) map[string]any { return map[string]any{"data": map[string]any{}} }})
	if _, err := client.Generate(context.Background(), "k", Request{Model: "m", Prompt: "p"}); err == nil {
		t.Fatal("empty result accepted")
	}
}

func TestDownloadRefusesPrivateAndPlainAddresses(t *testing.T) {
	f := &fakeMedia{statuses: []string{"completed"}, picture: testPNG(t, 4, 4), final: func(base string) map[string]any {
		return map[string]any{"data": map[string]any{"url": base + "/picture.png"}}
	}}
	client, _ := newFake(t, f)
	client.AllowPrivate = false
	if _, err := client.Generate(context.Background(), "k", Request{Model: "m", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "refusing to download") {
		t.Fatalf("plain http result: %v", err)
	}
	if _, err := client.download(context.Background(), "https://127.0.0.1:1/x.png"); err == nil || !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("private result: %v", err)
	}
}
