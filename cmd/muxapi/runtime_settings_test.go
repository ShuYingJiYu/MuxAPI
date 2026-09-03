package main

import (
	"path/filepath"
	"testing"

	"github.com/mirainya/muxapi/internal/store"
)

func TestRuntimeSettingSnapshotPublishesWholeRefresh(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSettings(map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatal(err)
	}
	initial, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := newRuntimeSettingSnapshot(st, initial)
	if snapshot.Get("a") != "1" || snapshot.Get("b") != "2" {
		t.Fatal("initial snapshot is incomplete")
	}
	if err := st.SetSettings(map[string]string{"a": "3", "b": "4"}); err != nil {
		t.Fatal(err)
	}
	if snapshot.Get("a") != "1" {
		t.Fatal("database writes must not mutate a published snapshot")
	}
	if err := snapshot.Reload(); err != nil {
		t.Fatal(err)
	}
	if snapshot.Get("a") != "3" || snapshot.Get("b") != "4" {
		t.Fatal("refreshed snapshot is incomplete")
	}
}
