package store

import (
	"path/filepath"
	"testing"

	"github.com/mirainya/muxapi/internal/upstream"
)

func TestUpstreamProtocolPersistence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "protocol.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.Create(&upstream.Upstream{Name: "claude", BaseURL: "http://example.com", APIKey: "key", Protocol: "claude", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Protocol != "claude" {
		t.Fatalf("protocol was not persisted: %+v", list)
	}
	loaded, err := store.Get(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Protocol != "claude" {
		t.Fatalf("Get protocol = %q", loaded.Protocol)
	}
}
