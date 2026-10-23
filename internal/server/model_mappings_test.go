package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/modelmapping"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestMappingAdminLifecycle(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := &upstream.Upstream{Name: "provider", BaseURL: "http://provider.test", APIKey: "test"}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "test-token", st, health.New(3, time.Minute), nil, nil, 0)
	previous := modelMappingSvc
	defer func() { modelMappingSvc = previous }()
	svc := modelmapping.New(st)
	s.SetModelMappingService(svc)
	h := s.Handler()
	request := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if auth {
			r.Header.Set("Authorization", "Bearer test-token")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, id := range []int64{0, u.ID} {
		target := "target"
		if id == 0 {
			target = "global-target"
		}
		body := fmt.Sprintf(`{"upstream_id":%d,"source_model":"source","target_model":%q}`, id, target)
		if w := request("POST", "/admin/model-mappings", body, true); w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if got := svc.Resolve(u.ID, "source"); got != "target" {
		t.Fatal(got)
	}
	if w := request("GET", "/admin/model-mappings?upstream_id=0", "", true); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w := request("GET", "/admin/model-mappings", "", true)
	var rows []store.ModelMapping
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 2 || rows[1].UpstreamName != u.Name {
		t.Fatalf("metadata: %+v %v", rows, err)
	}
	if w := request("POST", "/admin/model-mappings/cleanup", fmt.Sprintf(`{"ids":[%d,%d]}`, rows[0].ID, rows[1].ID), true); w.Code != 200 || w.Body.String() != "{\"deleted\":0}\n" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("DELETE", fmt.Sprintf("/admin/upstreams/%d", u.ID), "", true); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if row, err := st.GetModelMapping(u.ID, "source"); err != nil || row != nil {
		t.Fatalf("upstream deletion retained its mapping: %+v %v", row, err)
	}
	if got := svc.Resolve(u.ID, "source"); got != "global-target" {
		t.Fatalf("stale per-upstream cache after deletion: %s", got)
	}
	// Removing the fallback also invalidates the cached global resolution.
	if w := request("DELETE", fmt.Sprintf("/admin/model-mappings/%d", rows[0].ID), "", true); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if got := svc.Resolve(u.ID, "source"); got != "source" {
		t.Fatalf("stale cache after deletion: %s", got)
	}
	if w := request("POST", "/admin/model-mappings", fmt.Sprintf(`{"upstream_id":%d,"source_model":"source","target_model":"target"}`, u.ID), true); w.Code != 400 {
		t.Fatal("accepted deleted upstream", w.Code)
	}
	for _, tc := range []struct {
		method, path, body string
		auth               bool
		code               int
	}{
		{"POST", "/admin/model-mappings/cleanup", `{"ids":[1]}`, false, 401},
		{"GET", "/admin/model-mappings/cleanup", "", true, 405},
		{"POST", "/admin/model-mappings/cleanup", `{"ids":[]}`, true, 400},
		{"POST", "/admin/model-mappings/cleanup", `{"ids":[-1]}`, true, 400},
		{"POST", "/admin/model-mappings/cleanup", `{"ids":[0]}`, true, 400},
		{"GET", "/admin/model-mappings?upstream_id=bad", "", true, 400},
		{"DELETE", "/admin/model-mappings/99999", "", true, 404},
	} {
		if w := request(tc.method, tc.path, tc.body, tc.auth); w.Code != tc.code {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	s.SetReadOnly(true)
	if w := request("POST", "/admin/model-mappings/cleanup", `{"ids":[1]}`, true); w.Code != 423 {
		t.Fatal(w.Code)
	}
}
