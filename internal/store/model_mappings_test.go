package store

import (
	"errors"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

func TestMappingUpstreamLifecycle(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &upstream.Upstream{Name: "A", BaseURL: "http://a.test", APIKey: "test"}
	b := &upstream.Upstream{Name: "B", BaseURL: "http://b.test", APIKey: "test"}
	for _, u := range []*upstream.Upstream{a, b} {
		if err := st.Create(u); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{0, a.ID, b.ID} {
		if err := st.UpsertModelMapping(&ModelMapping{UpstreamID: id, SourceModel: "source", TargetModel: "target", MappingType: MappingStatic}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.IncrementMappingFailure(a.ID, "pending"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(0); err == nil {
		t.Fatal("zero id must not remove global mappings")
	}
	if err := st.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListModelMappings(nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("remaining rules: %+v %v", rows, err)
	}
	if rows[0].UpstreamID != 0 || rows[0].UpstreamMissing || rows[1].UpstreamName != "B" || rows[1].UpstreamMissing {
		t.Fatalf("scope metadata incorrect: %+v", rows)
	}
	for _, id := range []int64{a.ID, -1, 9999} {
		err := st.UpsertModelMapping(&ModelMapping{UpstreamID: id, SourceModel: "source", TargetModel: "target", MappingType: MappingAuto})
		if !errors.Is(err, ErrMappingUpstreamMissing) {
			t.Fatalf("orphan write accepted for %d: %v", id, err)
		}
		if _, err := st.IncrementMappingFailure(id, "pending"); !errors.Is(err, ErrMappingUpstreamMissing) {
			t.Fatalf("orphan failure counter accepted: %v", err)
		}
	}
	if rows, _ := st.ListModelMappings(&a.ID); len(rows) != 0 {
		t.Fatalf("deleted upstream retained rows: %+v", rows)
	}
}

func TestMappingOrphanCleanupOnlyDeletesConfirmedOrphans(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := &upstream.Upstream{Name: "existing", BaseURL: "http://existing.test", APIKey: "test"}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rows := []ModelMappingModel{
		{UpstreamID: 0, SourceModel: "global", TargetModel: "target", CreatedAt: now, UpdatedAt: now},
		{UpstreamID: u.ID, SourceModel: "existing", TargetModel: "target", CreatedAt: now, UpdatedAt: now},
		{UpstreamID: 777, SourceModel: "orphan", TargetModel: "target", CreatedAt: now, UpdatedAt: now},
		{UpstreamID: 888, SourceModel: "not-confirmed", TargetModel: "target", CreatedAt: now, UpdatedAt: now},
	}
	for i := range rows {
		if err := st.queryRow(`INSERT INTO model_mappings(upstream_id,source_model,target_model,mapping_type,failure_count,created_at,updated_at) VALUES(?,?,?,'static',0,?,?) RETURNING id`,
			rows[i].UpstreamID, rows[i].SourceModel, rows[i].TargetModel, st.timeValue(now), st.timeValue(now)).Scan(&rows[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := st.ListModelMappings(nil)
	if err != nil || len(listed) != 4 || !listed[2].UpstreamMissing || listed[2].UpstreamName != "" {
		t.Fatalf("orphan metadata: %+v %v", listed, err)
	}
	deleted, err := st.DeleteOrphanModelMappings([]int64{rows[0].ID, rows[1].ID, rows[2].ID, rows[2].ID, 99999})
	if err != nil || deleted != 1 {
		t.Fatalf("cleanup: %d %v", deleted, err)
	}
	listed, err = st.ListModelMappings(nil)
	if err != nil || len(listed) != 3 {
		t.Fatalf("remaining: %+v %v", listed, err)
	}
	if listed[0].SourceModel != "global" || listed[1].SourceModel != "existing" || listed[2].SourceModel != "not-confirmed" {
		t.Fatalf("unconfirmed rules removed: %+v", listed)
	}
	if n, err := st.DeleteOrphanModelMappings(nil); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestMappingCascadeRollback(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := &upstream.Upstream{Name: "A", BaseURL: "http://a.test", APIKey: "test"}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertModelMapping(&ModelMapping{UpstreamID: u.ID, SourceModel: "source", TargetModel: "target", MappingType: MappingStatic}); err != nil {
		t.Fatal(err)
	}
	if err := st.gormDB.Exec(`CREATE TRIGGER prevent_upstream_delete BEFORE DELETE ON upstreams BEGIN SELECT RAISE(ABORT, 'test rollback'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(u.ID); err == nil {
		t.Fatal("expected injected delete failure")
	}
	if _, err := st.Get(u.ID); err != nil {
		t.Fatalf("upstream was not rolled back: %v", err)
	}
	if row, err := st.GetModelMapping(u.ID, "source"); err != nil || row == nil {
		t.Fatalf("mapping was not rolled back: %+v %v", row, err)
	}
}
