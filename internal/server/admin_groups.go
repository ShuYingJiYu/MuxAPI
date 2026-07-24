package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/mirainya/muxapi/internal/health"
)

// --- 分组 ---
func (s *Server) adminGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		gs, err := s.store.ListGroups()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out := make([]groupOut, 0, len(gs))
		for _, g := range gs {
			out = append(out, groupOut{Group: g, Runtime: s.computeGroupRuntime(g.ID)})
		}
		writeJSON(w, out)
	case http.MethodPost:
		var d struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if json.NewDecoder(r.Body).Decode(&d) != nil || d.Name == "" {
			http.Error(w, "bad name", 400)
			return
		}
		if _, err := s.store.CreateGroup(d.Name, d.Description); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(201)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// adminGroupSub 路由 /admin/groups/{id}、/{id}/upstreams、/{id}/upstreams/{uid}、/{id}/keys。
func (s *Server) adminGroupSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/groups/"), "/")
	if rest == "reorder" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.reorderGroups(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	gid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad group id", 400)
		return
	}
	switch {
	case len(parts) == 1: // /{id}
		switch r.Method {
		case http.MethodPut:
			var d struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if json.NewDecoder(r.Body).Decode(&d) != nil || d.Name == "" {
				http.Error(w, "bad name", 400)
				return
			}
			if err := s.store.UpdateGroup(gid, d.Name, d.Description); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
		case http.MethodDelete:
			if err := s.store.DeleteGroup(gid); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
		default:
			http.Error(w, "method not allowed", 405)
			return
		}
	case parts[1] == "upstreams":
		s.adminGroupUpstreams(w, r, gid, parts)
	case parts[1] == "keys":
		s.adminGroupKeys(w, r, gid)
	default:
		http.Error(w, "not found", 404)
	}
}

// reorderGroups 持久化分组卡片顺序。body: {ids:[3,1,2,...]}
func (s *Server) reorderGroups(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.ReorderGroups(in.IDs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adminGroupUpstreams 组成员：GET 列表 / POST 加入(带组内prio/weight) /
// PUT 启停(/upstreams/{uid} 带 {enabled}) / DELETE 移除(/upstreams/{uid})。
func (s *Server) adminGroupUpstreams(w http.ResponseWriter, r *http.Request, gid int64, parts []string) {
	if len(parts) == 3 { // /{id}/upstreams/{uid}
		uid, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			http.Error(w, "bad upstream id", 400)
			return
		}
		switch r.Method {
		case http.MethodPut: // 组内启停
			var d struct {
				Enabled bool `json:"enabled"`
			}
			if json.NewDecoder(r.Body).Decode(&d) != nil {
				http.Error(w, "bad request body", 400)
				return
			}
			if err := s.store.SetMemberEnabled(gid, uid, d.Enabled); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
			return
		case http.MethodDelete: // 移除成员
			if err := s.store.RemoveMember(gid, uid); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
			return
		default:
			http.Error(w, "method not allowed", 405)
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		ms, err := s.store.ListGroupMembers(gid)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// 缓存各成员快照(含 reqs/延迟)与「聚合后对外状态」，避免重复加锁查询
		snaps := make(map[int64]health.Snapshot, len(ms))
		effSt := make(map[int64]string, len(ms))
		for _, m := range ms {
			snaps[m.UpstreamID] = s.health.Snapshot(m.UpstreamID)
			effSt[m.UpstreamID] = s.health.EffectiveState(m.UpstreamID)
		}
		state := func(id int64) string { return effSt[id] }
		best, hasEff := effectivePriority(ms, state)
		out := make([]memberOut, 0, len(ms))
		for _, m := range ms {
			isEff := hasEff && m.Enabled && m.GroupEnabled && m.Priority == best && effSt[m.UpstreamID] != "OPEN"
			out = append(out, memberOut{
				Member:      m,
				Health:      toHealthView(snaps[m.UpstreamID], effSt[m.UpstreamID]),
				ModelHealth: toModelHealthViews(s.health.ModelStates(m.UpstreamID)),
				Effective:   isEff,
			})
		}
		writeJSON(w, out)
	case http.MethodPost, http.MethodPut: // 加入或更新组内策略（UPSERT）
		var d struct {
			UpstreamID int64 `json:"upstream_id"`
			Priority   int   `json:"priority"`
			Weight     int   `json:"weight"`
		}
		if json.NewDecoder(r.Body).Decode(&d) != nil || d.UpstreamID == 0 {
			http.Error(w, "bad upstream_id", 400)
			return
		}
		if err := s.store.AddMember(gid, d.UpstreamID, d.Priority, d.Weight); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(201)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// --- 接入 key（归口到分组）---
func (s *Server) adminGroupKeys(w http.ResponseWriter, r *http.Request, gid int64) {
	switch r.Method {
	case http.MethodGet:
		ks, err := s.store.ListKeys(gid)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		type item struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Key     string `json:"key"`
			Masked  string `json:"masked"`
			Enabled bool   `json:"enabled"`
		}
		out := make([]item, 0, len(ks))
		for _, k := range ks {
			out = append(out, item{k.ID, k.Name, k.Key, mask(k.Key), k.Enabled})
		}
		writeJSON(w, out)
	case http.MethodPost: // 系统生成，返回明文(仅此一次)
		var d struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&d)
		key, err := s.store.CreateKey(d.Name, gid)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		writeJSON(w, map[string]string{"key": key})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) adminKeyItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/admin/keys/"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	switch r.Method {
	case http.MethodPut: // 启停
		var d struct {
			Enabled bool `json:"enabled"`
		}
		if json.NewDecoder(r.Body).Decode(&d) != nil {
			http.Error(w, "bad request body", 400)
			return
		}
		if err := s.store.SetKeyEnabled(id, d.Enabled); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	case http.MethodDelete:
		if err := s.store.DeleteKey(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	default:
		http.Error(w, "method not allowed", 405)
	}
}
