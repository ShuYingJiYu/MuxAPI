package server

import (
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/mirainya/muxapi/internal/forward"
	"github.com/mirainya/muxapi/internal/health"
	"github.com/mirainya/muxapi/internal/monitor"
	"github.com/mirainya/muxapi/internal/store"
	muxweb "github.com/mirainya/muxapi/web"
)

type Server struct {
	fwd        *forward.Forwarder
	adminToken string
	store      *store.Store
	health     *health.Manager
	mon        *monitor.Manager
	monProber  *monitor.Prober
}

func New(fwd *forward.Forwarder, adminToken string, st *store.Store, hm *health.Manager, mon *monitor.Manager, mp *monitor.Prober) *Server {
	return &Server{fwd: fwd, adminToken: adminToken, store: st, health: hm, mon: mon, monProber: mp}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/messages", s.messages)          // Claude 格式
	mux.HandleFunc("/v1/chat/completions", s.messages)  // OpenAI 格式
	mux.HandleFunc("/v1/responses", s.messages)         // OpenAI Responses API (codex)
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
	groupID, ok := s.store.GroupByKey(clientKey(r))
	if !ok {
		http.Error(w, "unauthorized: unknown access key", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	s.fwd.Forward(w, r, body, groupID)
}
