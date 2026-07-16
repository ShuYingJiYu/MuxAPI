package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestAdminUpstreamProtocolValidationAndPersistence(t *testing.T) {
	server, store, token := newAdminTestServer(t)
	url := server.URL + "/admin/upstreams"

	invalid := adminReq(t, http.MethodPost, url, token, `{"name":"bad","base_url":"http://example.com","protocol":"unknown"}`)
	invalid.Body.Close()
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid protocol status = %d", invalid.StatusCode)
	}

	created := adminReq(t, http.MethodPost, url, token, `{"name":"claude","base_url":"http://example.com","protocol":"claude","enabled":true}`)
	created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", created.StatusCode)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Protocol != "claude" {
		t.Fatalf("stored upstreams = %+v", list)
	}

	response := adminReq(t, http.MethodGet, url, token, "")
	defer response.Body.Close()
	var payload []struct {
		Protocol string `json:"protocol"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 1 || payload[0].Protocol != "claude" {
		t.Fatalf("admin payload = %+v", payload)
	}
}
