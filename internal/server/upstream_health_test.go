package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/monitor"
	"github.com/mirainya/muxapi/internal/scheduler"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestUpstreamConfigurationChangeInvalidatesOnlyOldHealthEvidence(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	manager := health.New(1, time.Minute)
	if err := manager.SetModelExclusionStore(st); err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New(func(int64) []*upstream.Upstream { return nil }, manager)
	server := httptest.NewServer(New(forward.New(sched, manager, 1), "test-token", st, manager, monitor.New(st), nil, 32<<20).Handler())
	defer server.Close()
	item := &upstream.Upstream{Name: "provider", BaseURL: "https://old.example.com", APIKey: "old-key", Protocol: "passthrough", Enabled: true}
	if err := st.Create(item); err != nil {
		t.Fatal(err)
	}
	update := func(baseURL, key, protocol string) {
		t.Helper()
		payload := fmt.Sprintf(`{"name":"renamed","base_url":%q,"api_key":%q,"protocol":%q,"enabled":true}`, baseURL, key, protocol)
		response := adminReq(t, http.MethodPut, server.URL+"/admin/upstreams/"+itoa(item.ID), "test-token", payload)
		defer response.Body.Close()
		if response.StatusCode != 204 {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("configuration update returned %d: %s", response.StatusCode, body)
		}
	}
	manager.MarkModelUnsupported(item.ID, "model")
	update(item.BaseURL, "", item.Protocol)
	if !manager.IsModelUnsupported(item.ID, "model") {
		t.Fatal("renaming must preserve health evidence")
	}
	old, _ := manager.Claim(1, item.ID, "other-model")
	update("https://new.example.com", "new-key", "openai")
	manager.CompleteModelUnsupported(old, 0, 404, "old configuration")
	if len(manager.ModelStates(item.ID)) != 0 || manager.EffectiveState(item.ID) != "CLOSED" {
		t.Fatal("changed endpoint retained stale health")
	}
	restarted := health.New(1, time.Minute)
	if err := restarted.SetModelExclusionStore(st); err != nil {
		t.Fatal(err)
	}
	if !restarted.IsAvailable(item.ID, "model") || !restarted.IsAvailable(item.ID, "other-model") {
		t.Fatal("configuration invalidation was not durable")
	}
}
