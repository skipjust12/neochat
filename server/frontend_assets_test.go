package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFrontendAssetsAreEmbedded(t *testing.T) {
	mux := (&Server{}).Mux()
	for _, asset := range []struct {
		path string
		kind string
		min  int
	}{
		{"/assets/fluid.css", "text/css", 1000},
		{"/assets/InterVariable.ttf", "font/ttf", 100000},
		{"/assets/Charter-Regular.woff2", "font/woff2", 10000},
	} {
		t.Run(asset.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, asset.path, nil))
			if response.Code != http.StatusOK || response.Body.Len() < asset.min {
				t.Fatalf("asset not embedded: status=%d bytes=%d", response.Code, response.Body.Len())
			}
			if !strings.HasPrefix(response.Header().Get("Content-Type"), asset.kind) {
				t.Fatalf("incorrect asset type: %s", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestFrontendRevalidatesAfterDeployment(t *testing.T) {
	response := httptest.NewRecorder()
	(&Server{}).Mux().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("HTML must revalidate after a frontend deployment")
	}
	if !strings.Contains(response.Body.String(), `/assets/fluid.css?v=3`) {
		t.Fatal("frontend does not load the embedded design system")
	}
}
