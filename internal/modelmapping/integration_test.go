package modelmapping

import (
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/store"
)

func TestServiceResolveWithStore(t *testing.T) {
	st, err := store.Open("file::memory:?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st)

	// No mapping exists: should return original model
	got := svc.Resolve(1, "claude-haiku-4-5")
	if got != "claude-haiku-4-5" {
		t.Fatalf("expected original model, got %q", got)
	}

	// Create a static mapping for upstream 1
	err = st.UpsertModelMapping(&store.ModelMapping{
		UpstreamID:  1,
		SourceModel: "claude-haiku-4-5",
		TargetModel: "claude-haiku-4-5-20251001",
		MappingType: store.MappingStatic,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Invalidate cache so the new mapping is picked up
	svc.InvalidateAll()

	// Should resolve to the mapped name for upstream 1
	got = svc.Resolve(1, "claude-haiku-4-5")
	if got != "claude-haiku-4-5-20251001" {
		t.Fatalf("expected mapped model, got %q", got)
	}

	// Different upstream should not match
	got = svc.Resolve(2, "claude-haiku-4-5")
	if got != "claude-haiku-4-5" {
		t.Fatalf("expected original model for upstream 2, got %q", got)
	}

	// Global mapping (upstream_id=0) should apply to all upstreams without specific mapping
	err = st.UpsertModelMapping(&store.ModelMapping{
		UpstreamID:  0,
		SourceModel: "claude-haiku-4-5",
		TargetModel: "claude-haiku-4-5-global",
		MappingType: store.MappingStatic,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.InvalidateAll()

	// Upstream 1 should still use its specific mapping
	got = svc.Resolve(1, "claude-haiku-4-5")
	if got != "claude-haiku-4-5-20251001" {
		t.Fatalf("expected upstream-specific mapping, got %q", got)
	}

	// Upstream 2 should use global mapping
	got = svc.Resolve(2, "claude-haiku-4-5")
	if got != "claude-haiku-4-5-global" {
		t.Fatalf("expected global mapping for upstream 2, got %q", got)
	}
}

func TestServiceAutoLearn(t *testing.T) {
	st, err := store.Open("file::memory:?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st)

	// Record failures below threshold - no mapping should be created
	svc.RecordFailure(1, "claude-opus-4-6-thinking")
	svc.RecordFailure(1, "claude-opus-4-6-thinking")
	svc.InvalidateAll()
	got := svc.Resolve(1, "claude-opus-4-6-thinking")
	if got != "claude-opus-4-6-thinking" {
		t.Fatalf("expected no mapping before threshold, got %q", got)
	}

	// Third failure should trigger auto-learning
	svc.RecordFailure(1, "claude-opus-4-6-thinking")
	svc.InvalidateAll()
	got = svc.Resolve(1, "claude-opus-4-6-thinking")
	if got != "claude-opus-4-6" {
		t.Fatalf("expected auto-learned fallback, got %q", got)
	}

	// Verify the mapping has an expiry
	mapping, err := st.GetModelMapping(1, "claude-opus-4-6-thinking")
	if err != nil {
		t.Fatal(err)
	}
	if mapping == nil {
		t.Fatal("expected mapping to exist")
	}
	if mapping.MappingType != store.MappingAuto {
		t.Fatalf("expected auto mapping type, got %q", mapping.MappingType)
	}
	if mapping.ExpiresAt == nil {
		t.Fatal("expected expiry on auto-learned mapping")
	}
	if mapping.ExpiresAt.Before(time.Now()) {
		t.Fatal("expected future expiry")
	}
}

func TestServiceRecordSuccessResetsAuto(t *testing.T) {
	st, err := store.Open("file::memory:?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st)

	// Trigger auto-learning
	for i := 0; i < 3; i++ {
		svc.RecordFailure(1, "claude-opus-4-6-thinking")
	}
	svc.InvalidateAll()
	got := svc.Resolve(1, "claude-opus-4-6-thinking")
	if got != "claude-opus-4-6" {
		t.Fatalf("expected auto-learned fallback, got %q", got)
	}

	// Record success clears the auto mapping
	svc.RecordSuccess(1, "claude-opus-4-6-thinking")
	svc.InvalidateAll()
	got = svc.Resolve(1, "claude-opus-4-6-thinking")
	if got != "claude-opus-4-6-thinking" {
		t.Fatalf("expected original model after success reset, got %q", got)
	}
}
