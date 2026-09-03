package main

import (
	"sync/atomic"

	"github.com/mirainya/muxapi/internal/store"
)

type runtimeSettingValues map[string]string

// runtimeSettingSnapshot keeps request-path configuration reads in memory.
// Every refresh publishes a new map so readers never observe a partial update.
type runtimeSettingSnapshot struct {
	store  *store.Store
	values atomic.Pointer[runtimeSettingValues]
}

func newRuntimeSettingSnapshot(st *store.Store, initial map[string]string) *runtimeSettingSnapshot {
	snapshot := &runtimeSettingSnapshot{store: st}
	snapshot.replace(initial)
	return snapshot
}

func (s *runtimeSettingSnapshot) Get(key string) string {
	values := s.values.Load()
	if values == nil {
		return ""
	}
	return (*values)[key]
}

func (s *runtimeSettingSnapshot) Reload() error {
	values, err := s.store.Settings()
	if err != nil {
		return err
	}
	s.replace(values)
	return nil
}

func (s *runtimeSettingSnapshot) replace(values map[string]string) {
	clone := make(runtimeSettingValues, len(values))
	for key, value := range values {
		clone[key] = value
	}
	s.values.Store(&clone)
}
