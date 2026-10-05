package imagegen

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"
)

// testMP4 is the start of an MP4 file: enough for attachment.VideoMIME.
func testMP4() []byte {
	ftyp := make([]byte, 16)
	binary.BigEndian.PutUint32(ftyp, 16)
	copy(ftyp[4:], "ftypisom")
	return append(ftyp, bytes.Repeat([]byte{0}, 64)...)
}

func TestGenerateVideoSendsChoicesAndDownloadsTheClip(t *testing.T) {
	f := &fakeMedia{statuses: []string{"pending", "processing", "completed"}, picture: testPNG(t, 16, 9), video: testMP4(),
		final: func(base string) map[string]any {
			// A cover picture comes before the clip; the clip is what counts.
			return map[string]any{"data": map[string]any{"cover": map[string]any{"url": base + "/picture.png"}, "video": map[string]any{"url": base + "/clip.mp4"}}, "usage": map[string]any{"cost_rub": 63}}
		}}
	client, _ := newFake(t, f)
	result, err := client.GenerateVideo(context.Background(), "pza_img", VideoRequest{
		Model: "google/gemini-omni-1.1-flash", Prompt: "кот прыгает через лужу", Duration: "6", Resolution: "1080p", AspectRatio: "9:16",
		Images: []Reference{{MIME: "image/png", Data: testPNG(t, 4, 4)}}, User: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Video.MIME != "video/mp4" || !bytes.Equal(result.Video.Data, f.video) || !result.HasCost || result.CostRUB != 63 {
		t.Fatalf("result = %s %d bytes, cost %v/%v", result.Video.MIME, len(result.Video.Data), result.CostRUB, result.HasCost)
	}
	input := f.created["input"].(map[string]any)
	if f.created["async"] != true || f.created["model"] != "google/gemini-omni-1.1-flash" || f.created["user"] != "u1" {
		t.Fatalf("request = %v", f.created)
	}
	if input["duration"] != "6" || input["resolution"] != "1080p" || input["aspect_ratio"] != "9:16" || input["videos"] != nil {
		t.Fatalf("input = %v", input)
	}
	images := input["images"].([]any)
	if first := images[0].(map[string]any); first["type"] != "base64" || !strings.HasPrefix(first["data"].(string), "data:image/png;base64,") {
		t.Fatalf("images = %v", images)
	}
}

func TestGenerateVideoEditsAClip(t *testing.T) {
	f := &fakeMedia{statuses: []string{"completed"}, video: testMP4(),
		final: func(base string) map[string]any {
			return map[string]any{"output": []any{map[string]any{"url": base + "/clip.mp4"}}}
		}}
	client, _ := newFake(t, f)
	_, err := client.GenerateVideo(context.Background(), "k", VideoRequest{Model: "m", Prompt: "добавь динамики", Duration: "6", Resolution: "720p",
		Videos: []Reference{{MIME: "video/quicktime", Data: testMP4()}}, VideoEnd: 10})
	if err != nil {
		t.Fatal(err)
	}
	input := f.created["input"].(map[string]any)
	videos := input["videos"].([]any)
	if first := videos[0].(map[string]any); first["type"] != "base64" || !strings.HasPrefix(first["data"].(string), "data:video/quicktime;base64,") {
		t.Fatalf("videos = %v", videos)
	}
	// Editing a clip, the model picks the length; a long clip is cut to 10 s.
	if input["duration"] != nil || input["video_start"] != float64(0) || input["video_end"] != float64(10) {
		t.Fatalf("input = %v", input)
	}
}

func TestGenerateVideoNeedsAClip(t *testing.T) {
	f := &fakeMedia{statuses: []string{"completed"}, picture: testPNG(t, 4, 4),
		final: func(base string) map[string]any {
			return map[string]any{"data": map[string]any{"url": base + "/picture.png"}}
		}}
	client, _ := newFake(t, f)
	if _, err := client.GenerateVideo(context.Background(), "k", VideoRequest{Model: "m", Prompt: "p", Duration: "4"}); err == nil || !strings.Contains(err.Error(), "without a video") {
		t.Fatalf("err = %v", err)
	}
	if _, err := client.GenerateVideo(context.Background(), " ", VideoRequest{Model: "m", Prompt: "p"}); err != ErrMissingKey {
		t.Fatalf("no key: err = %v", err)
	}
}
