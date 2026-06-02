package store

import (
	"path/filepath"
	"testing"

	"github.com/mirainya/muxapi/internal/upstream"
)

func TestStoreCRUD(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 空库
	if list, _ := st.ListEnabledByGroup(1); len(list) != 0 {
		t.Fatal("空库应无上游")
	}

	// 建分组 + 全局上游（B 停用）
	g1, _ := st.CreateGroup("g1", "first")
	st.Create(&upstream.Upstream{Name: "A", BaseURL: "http://a", APIKey: "ka", Enabled: true})
	st.Create(&upstream.Upstream{Name: "B", BaseURL: "http://b", APIKey: "kb", Enabled: false})
	ups, _ := st.List()
	if len(ups) != 2 {
		t.Fatalf("全局池应有 2 个上游，实际 %d", len(ups))
	}
	idA, idB := ups[0].ID, ups[1].ID

	// 加入 g1：A 组内优先级 1，B 优先级 2
	st.AddMember(g1, idA, 1, 1)
	st.AddMember(g1, idB, 2, 1)

	// ListEnabledByGroup 只返回启用成员（A），且带组内 priority
	list, err := st.ListEnabledByGroup(g1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "A" || list[0].Priority != 1 {
		t.Fatalf("应只返回启用的 A(组内prio=1)，实际 %+v", list)
	}
	if list[0].APIKey != "ka" || list[0].BaseURL != "http://a" {
		t.Fatalf("字段读取错误 %+v", list[0])
	}

	// M:N：同一上游 A 加入 g2，组内优先级不同(10)
	g2, _ := st.CreateGroup("g2", "")
	st.AddMember(g2, idA, 10, 5)
	l2, _ := st.ListEnabledByGroup(g2)
	if len(l2) != 1 || l2[0].Priority != 10 || l2[0].Weight != 5 {
		t.Fatalf("A 在 g2 应有独立组内策略 prio=10 weight=5，实际 %+v", l2)
	}
	// g1 里 A 的策略不受影响
	l1, _ := st.ListEnabledByGroup(g1)
	if l1[0].Priority != 1 {
		t.Fatalf("g1 中 A 的组内优先级应仍为 1，实际 %d", l1[0].Priority)
	}

	// 系统生成密钥 + 启停过滤
	key, _ := st.CreateKey("k1", g1)
	if gid, ok := st.GroupByKey(key); !ok || gid != g1 {
		t.Fatalf("启用 key 应解析到 g1")
	}
	ks, _ := st.ListKeys(g1)
	st.SetKeyEnabled(ks[0].ID, false)
	if _, ok := st.GroupByKey(key); ok {
		t.Fatal("停用 key 不应再解析到分组")
	}
}
