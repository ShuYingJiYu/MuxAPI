package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/store"
)

func TestSecurityQueryFiltersAndActualPage(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().Add(-10 * time.Minute).Unix()
	st.EnqueueSecurity(store.SecurityLog{FirstAt: at, Kind: "login", Success: true, ClientIP: "192.0.2.1", UserAgent: "browser"})
	if err := st.FlushSecurity(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, event := range []store.SecurityLog{
		{FirstAt: at + 60, Kind: "access", Success: true, UserAgent: "browser"},
		{FirstAt: at + 120, Kind: "access", UserAgent: "browser"},
		{FirstAt: at + 180, Kind: "access", Success: true, UserAgent: "new browser"},
	} {
		event.ClientIP = "192.0.2.1"
		st.EnqueueSecurity(event)
	}
	if err := st.FlushSecurity(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "test-token", st, nil, nil, nil, 0)
	s.SetReadOnly(true)
	h := s.Handler()
	get := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/admin/security-logs?"+query, nil)
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := get("view=events&range=24h&page=999&size=2")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var page store.SecurityPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || page.Page != 2 || page.Size != 2 || len(page.Entries) != 1 || page.Entries[0].Kind != "login" {
		t.Fatalf("actual page: %+v", page)
	}
	if page.To-page.From != 86400 || page.Summary.LoginSuccesses != 1 || page.Summary.ManagementAccesses != 3 || page.Summary.LatestLogin != at {
		t.Fatalf("bounds/summary: %+v", page)
	}
	for _, query := range []string{"kind=invalid", "view=invalid", "range=invalid", "range=24h&from=100", "range=24h&to=100", "page=1000001"} {
		if w := get(query); w.Code != 400 {
			t.Fatalf("accepted %s: %d", query, w.Code)
		}
	}
	w = get("view=all&kind=login&range=1h")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Summary.ManagementAccesses != 3 {
		t.Fatalf("kind filter: %+v", page)
	}
	if w := get("view=events&page=10001"); w.Code != 200 {
		t.Fatal("deep page should normalize, not fail")
	}
}
