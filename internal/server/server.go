package server

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/monitor"
	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
	muxweb "github.com/mirainya/muxapi/web"
)

// modelsTTL 下游 /v1/models 汇总结果的单上游缓存有效期。
const modelsTTL = 60 * time.Second

type modelCacheEntry struct {
	models []string
	ts     time.Time
}

type Server struct {
	fwd        *forward.Forwarder
	adminToken string
	store      *store.Store
	health     *health.Manager
	mon        *monitor.Manager
	monProber  *monitor.Prober

	modelMu    sync.Mutex                 // 保护 modelCache
	modelCache map[int64]modelCacheEntry  // 按 upstream_id 缓存其 /v1/models 结果，TTL=modelsTTL
}

func New(fwd *forward.Forwarder, adminToken string, st *store.Store, hm *health.Manager, mon *monitor.Manager, mp *monitor.Prober) *Server {
	return &Server{fwd: fwd, adminToken: adminToken, store: st, health: hm, mon: mon, monProber: mp,
		modelCache: make(map[int64]modelCacheEntry)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/messages", s.messages)          // Claude 格式
	mux.HandleFunc("/v1/chat/completions", s.messages)  // OpenAI 格式
	mux.HandleFunc("/v1/responses", s.messages)         // OpenAI Responses API (codex)
	mux.HandleFunc("/v1/models", s.listModels)          // 模型清单：汇总分组内各上游
	s.registerAdmin(mux)
	// 内嵌前端（"/" 兜底，/v1、/admin、/healthz 等更长前缀优先匹配，不冲突）
	if sub, err := fs.Sub(muxweb.Dist, "dist"); err == nil {
		mux.Handle("/", http.FileServer(http.FS(sub)))
	}
	return mux
}

// auth 后台管理鉴权（adminToken）。AdminToken 为空时跳过（仅本地调试）。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken != "" {
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if tok != s.adminToken && r.Header.Get("x-api-key") != s.adminToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func clientKey(r *http.Request) string {
	if k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); k != "" {
		return k
	}
	return r.Header.Get("x-api-key")
}

// messages 转发入口：按接入 key 找到分组，在组内调度转发。
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	groupID, keyName, ok := s.store.GroupAndKeyByKey(clientKey(r))
	if !ok {
		http.Error(w, "unauthorized: unknown access key", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	s.fwd.Forward(w, r, body, groupID, keyName)
}

// listModels 下游模型清单：按接入 key 找到分组，实时汇总分组内各启用上游的
// /v1/models 并集去重，输出 OpenAI 兼容格式。单上游拉取失败只跳过该上游，
// 保证部分可用；结果按 upstream 维度缓存 modelsTTL，避免每次请求都打上游。
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	groupID, ok := s.store.GroupByKey(clientKey(r))
	if !ok {
		http.Error(w, "unauthorized: unknown access key", http.StatusUnauthorized)
		return
	}
	ups, err := s.store.ListEnabledByGroup(groupID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	seen := make(map[string]bool)
	var ids []string
	for _, u := range ups {
		for _, m := range s.upstreamModels(u) {
			if !seen[m] {
				seen[m] = true
				ids = append(ids, m)
			}
		}
	}
	sort.Strings(ids)
	now := time.Now().Unix()
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]modelObj, 0, len(ids))
	for _, id := range ids {
		data = append(data, modelObj{ID: id, Object: "model", Created: now, OwnedBy: "muxapi"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// upstreamModels 取单个上游的模型列表，命中缓存(TTL 内)则直接返回，
// 否则实时拉取并写缓存；拉取失败时回退到上次缓存（可能过期），仍无则空。
func (s *Server) upstreamModels(u *upstream.Upstream) []string {
	s.modelMu.Lock()
	ent, ok := s.modelCache[u.ID]
	s.modelMu.Unlock()
	if ok && time.Since(ent.ts) < modelsTTL {
		return ent.models
	}
	models, _, err := u.FetchModels(10 * time.Second)
	if err != nil {
		return ent.models // 失败回退到旧缓存（无则 nil）
	}
	s.modelMu.Lock()
	s.modelCache[u.ID] = modelCacheEntry{models: models, ts: time.Now()}
	s.modelMu.Unlock()
	return models
}
