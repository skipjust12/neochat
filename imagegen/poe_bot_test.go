package imagegen

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNativeBotQueryImageDownload(t *testing.T) {
	picture := testPNG(t, 3, 4)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("key leaked to CDN")
		}
		w.Write(picture)
	}))
	defer cdn.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Nano-Banana-2.1" || r.Header.Get("Authorization") != "Bearer native-key" {
			t.Errorf("wrong native media request %s", r.URL.Path)
		}
		fmt.Fprintf(w, "event: file\ndata: {\"url\":%q,\"content_type\":\"image/png\",\"name\":\"image.png\"}\n\nevent: done\ndata: {}\n\n", cdn.URL+"/image.png")
	}))
	defer api.Close()
	c := NewPoeClient()
	c.BotQuery = true
	c.BotBaseURL = api.URL
	c.AllowPrivate = true
	out, err := c.Generate(context.Background(), "native-key", Request{Model: "Nano-Banana-2.1", Prompt: "draw"})
	if err != nil || len(out.Images) != 1 || out.Images[0].Width != 3 {
		t.Fatalf("media %+v %v", out, err)
	}
}
