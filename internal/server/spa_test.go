package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandlerServesEmbeddedSPAHistoryRoute(t *testing.T) {
	handler := New(nil, "", nil, nil, nil, nil, 0).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("GET /settings status = %d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Fatalf("GET /settings content-type = %q", contentType)
	}
	if !strings.Contains(response.Body.String(), `<div id="app">`) || !strings.Contains(response.Body.String(), `boot-screen`) {
		t.Fatal("GET /settings did not serve the embedded frontend entry")
	}
}

func TestSPAFileServerSupportsHistoryRoutes(t *testing.T) {
	root := fstest.MapFS{
		"index.html":    {Data: []byte(`<div id="app"></div>`)},
		"assets/app.js": {Data: []byte(`console.log("app")`)},
	}
	handler := spaFileServer(root)

	for _, route := range []string{"/", "/overview", "/groups", "/upstreams", "/monitors", "/logs", "/settings"} {
		t.Run(route, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, route, nil))
			if response.Code != http.StatusOK || response.Body.String() != `<div id="app"></div>` {
				t.Fatalf("GET %s = %d %q", route, response.Code, response.Body.String())
			}
		})
	}
}

func TestSPAFileServerKeepsAssetResponses(t *testing.T) {
	root := fstest.MapFS{
		"index.html":    {Data: []byte(`<div id="app"></div>`)},
		"assets/app.js": {Data: []byte(`console.log("app")`)},
	}
	handler := spaFileServer(root)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || string(body) != `console.log("app")` {
		t.Fatalf("existing asset = %d %q", response.Code, body)
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d", missing.Code)
	}
}

func TestHandlerDoesNotServeSPAForUnknownBackendRoutes(t *testing.T) {
	handler := New(nil, "", nil, nil, nil, nil, 0).Handler()
	for _, route := range []string{"/v1/unknown", "/admin/unknown", "/unknown"} {
		t.Run(route, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, route, nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want %d", route, response.Code, http.StatusNotFound)
			}
		})
	}
}
