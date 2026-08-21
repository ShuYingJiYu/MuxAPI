// Package server 提供客户端 API、管理 API 与内嵌前端的 HTTP 接入层。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mirainya/muxapi/internal/backup"
	"github.com/mirainya/muxapi/internal/billing"
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

// Server 汇集转发、健康、监控和存储依赖，并维护模型列表缓存。
type Server struct {
	fwd             *forward.Forwarder
	adminToken      string
	store           *store.Store
	health          *health.Manager
	mon             *monitor.Manager
	monProber       *monitor.Prober
	billingMgr      *billing.Manager
	backupSvc       *backup.Service
	maxBody         int64 // 请求体字节上限（<=0 表示不限制）
	maxBodyProvider func() int64
	settingsChanged func()

	modelMu     sync.Mutex                // 保护 modelCache 与 modelFlight
	modelCache  map[int64]modelCacheEntry // 按 upstream_id 缓存其 /v1/models 结果，TTL=modelsTTL
	modelFlight map[int64]*modelFetch     // 同一上游正在进行的拉取，用于并发去重
}

// modelFetch 表示一次正在进行的上游模型拉取。缓存过期瞬间的并发请求共享同一次
// 拉取结果，避免 N 个客户端 × M 个上游的连接风暴。
type modelFetch struct {
	done   chan struct{}
	models []string
}

// SetBillingManager enables background and manual provider billing refreshes.
func (s *Server) SetBillingManager(manager *billing.Manager) { s.billingMgr = manager }

// SetBackupService enables the S3 backup feature.
func (s *Server) SetBackupService(svc *backup.Service) { s.backupSvc = svc }

// SetMaxBodyProvider supplies the current request body limit without requiring
// a process restart after a settings update.
func (s *Server) SetMaxBodyProvider(provider func() int64) { s.maxBodyProvider = provider }

// SetSettingsChanged registers the runtime policy refresh hook used after the
// admin settings endpoint persists a new breaker policy.
func (s *Server) SetSettingsChanged(handler func()) { s.settingsChanged = handler }

// New 创建 HTTP 服务；maxBody 控制客户端请求正文上限。
func New(fwd *forward.Forwarder, adminToken string, st *store.Store, hm *health.Manager, mon *monitor.Manager, mp *monitor.Prober, maxBody int64) *Server {
	return &Server{fwd: fwd, adminToken: adminToken, store: st, health: hm, mon: mon, monProber: mp, maxBody: maxBody,
		modelCache: make(map[int64]modelCacheEntry), modelFlight: make(map[int64]*modelFetch)}
}

// Handler 注册客户端、管理、健康检查和静态前端路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/messages", s.messages)         // Claude 格式
	mux.HandleFunc("/v1/chat/completions", s.messages) // OpenAI 格式
	mux.HandleFunc("/v1/responses", s.messages)        // OpenAI Responses API (codex)
	mux.HandleFunc("/v1/models", s.listModels)         // 模型清单：汇总分组内各上游
	s.registerAdmin(mux)
	// 内嵌前端（"/" 兜底，/v1、/admin、/healthz 等更长前缀优先匹配，不冲突）
	if sub, err := fs.Sub(muxweb.Dist, "dist"); err == nil {
		mux.Handle("/", spaFileServer(sub))
	}
	return mux
}

// spaFileServer serves real build assets when they exist and falls back to
// index.html for extensionless client-side routes used by Vue Router history mode.
func spaFileServer(root fs.FS) http.Handler {
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "." {
			name = ""
		}
		if name == "" {
			files.ServeHTTP(w, r)
			return
		}
		if _, err := fs.Stat(root, name); err == nil {
			files.ServeHTTP(w, r)
			return
		}
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		if !isSPAHistoryRoute(name) {
			http.NotFound(w, r)
			return
		}

		indexRequest := r.Clone(r.Context())
		indexRequest.URL.Path = "/"
		files.ServeHTTP(w, indexRequest)
	})
}

// isSPAHistoryRoute lists the admin pages that Vue Router can render directly.
// Backend prefixes must never reach the HTML fallback, otherwise an unknown API
// request would incorrectly receive a successful index.html response.
func isSPAHistoryRoute(name string) bool {
	switch name {
	case "overview", "groups", "upstreams", "monitors", "logs", "settings":
		return true
	default:
		return false
	}
}

// auth 后台管理鉴权（adminToken）。AdminToken 为空时跳过（仅本地调试）。
// 用常量时间比较防 token 计时侧信道；长度不等时 ConstantTimeCompare 返回 0，
// 故两路候选(Authorization / x-api-key)分别比较再 OR。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken != "" {
			want := []byte(s.adminToken)
			bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			okBearer := subtle.ConstantTimeCompare([]byte(bearer), want) == 1
			okKey := subtle.ConstantTimeCompare([]byte(r.Header.Get("x-api-key")), want) == 1
			if !okBearer && !okKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// clientKey 兼容 Bearer 与 x-api-key 两种接入凭证头。
func clientKey(r *http.Request) string {
	if k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); k != "" {
		return k
	}
	return r.Header.Get("x-api-key")
}

// requestClientIP trusts forwarding headers only when the direct peer is a local reverse proxy.
func requestClientIP(r *http.Request) string {
	peer, ok := parseRequestIP(r.RemoteAddr)
	if !ok {
		return ""
	}
	peer = peer.Unmap()
	if peer.IsLoopback() {
		for _, value := range []string{
			r.Header.Get("CF-Connecting-IP"),
			r.Header.Get("X-Real-IP"),
			strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0],
		} {
			if forwarded, valid := parseRequestIP(value); valid && !forwarded.IsUnspecified() {
				return forwarded.Unmap().String()
			}
		}
	}
	return peer.String()
}

func parseRequestIP(value string) (netip.Addr, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Addr{}, false
	}
	if address, err := netip.ParseAddrPort(value); err == nil {
		return address.Addr(), true
	}
	address, err := netip.ParseAddr(strings.Trim(value, "[]"))
	return address, err == nil
}

func requestUserAgent(r *http.Request) string {
	const maxRunes = 512
	value := strings.TrimSpace(r.UserAgent())
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes])
	}
	return value
}

// messages 转发入口：按接入 key 找到分组，在组内调度转发。
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := uuid.NewString()
	w.Header().Set("X-Request-ID", requestID)
	endpoint := r.URL.Path
	clientIP, userAgent := requestClientIP(r), requestUserAgent(r)
	groupID, keyName, ok := s.store.GroupAndKeyByKey(clientKey(r))
	if !ok {
		http.Error(w, "unauthorized: unknown access key", http.StatusUnauthorized)
		s.recordRequest(requestID, started, 0, "unknown", "", endpoint, clientIP, userAgent, false, 0,
			forward.Result{Status: http.StatusUnauthorized, Outcome: forward.OutcomeClientError,
				ErrorKind: "auth", ErrorSource: "client", Error: "unauthorized: unknown access key"})
		return
	}
	// 限制请求体大小，防无上限 io.ReadAll 被超大 body 打爆内存(DoS)。
	maxBody := s.maxBody
	if s.maxBodyProvider != nil {
		if value := s.maxBodyProvider(); value >= 0 {
			maxBody = value
		}
	}
	if maxBody > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			model, stream := parseBodyAudit(body)
			s.recordRequest(requestID, started, groupID, keyName, model, endpoint, clientIP, userAgent, stream, int64(len(body)),
				forward.Result{Status: http.StatusRequestEntityTooLarge, Outcome: forward.OutcomeClientError,
					ErrorKind: "request_too_large", ErrorSource: "client", Error: "request body too large"})
			return
		}
		http.Error(w, "read body failed", http.StatusBadRequest)
		model, stream := parseBodyAudit(body)
		s.recordRequest(requestID, started, groupID, keyName, model, endpoint, clientIP, userAgent, stream, int64(len(body)),
			forward.Result{Status: http.StatusBadRequest, Outcome: forward.OutcomeClientError,
				ErrorKind: "request_read", ErrorSource: "client", Error: "read body failed"})
		return
	}
	model, stream := parseBodyAudit(body)
	result := s.fwd.Forward(w, r, body, groupID, keyName)
	s.recordRequest(requestID, started, groupID, keyName, model, endpoint, clientIP, userAgent, stream, int64(len(body)), result)
}

// recordRequest 将转发结果转换为异步持久化的请求与尝试记录。
func (s *Server) recordRequest(requestID string, started time.Time, groupID int64, keyName, model, endpoint, clientIP, userAgent string, stream bool, requestBytes int64, result forward.Result) {
	completed := time.Now()
	attempts := make([]store.RequestAttemptRecord, len(result.Attempts))
	for i, attempt := range result.Attempts {
		attempts[i] = store.RequestAttemptRecord{
			AttemptNo: attempt.AttemptNo, Protocol: attempt.Protocol,
			UpstreamID: attempt.UpstreamID, Priority: attempt.Priority,
			SelectionReason: attempt.SelectionReason, HealthBefore: attempt.HealthBefore, HealthAfter: attempt.HealthAfter,
			Status: attempt.Status, Outcome: attempt.Outcome, TTFTMs: attempt.TTFTMs, DurationMs: attempt.DurationMs,
			ResponseBytes: attempt.ResponseBytes, Stream: attempt.Stream, StreamCompleted: attempt.StreamCompleted,
			LastEvent: attempt.LastEvent, InputTokens: attempt.InputTokens, OutputTokens: attempt.OutputTokens,
			CachedTokens: attempt.CachedTokens, CacheCreationTokens: attempt.CacheCreationTokens,
			UpstreamRequestID: attempt.UpstreamRequestID,
			ErrorKind:         attempt.ErrorKind, ErrorSource: attempt.ErrorSource,
			CreatedAt: attempt.CreatedAt, CompletedAt: attempt.CompletedAt, Error: attempt.Error,
		}
	}
	s.store.EnqueueRequest(store.RequestRecord{
		RequestID: requestID, GroupID: groupID, FinalUpstreamID: result.FinalUpstreamID,
		Model: model, Endpoint: endpoint, KeyName: keyName, ClientIP: clientIP, UserAgent: userAgent,
		Stream: stream, RequestBytes: requestBytes,
		ResponseBytes: result.ResponseBytes, InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		CachedTokens: result.CachedTokens, CacheCreationTokens: result.CacheCreationTokens,
		StreamCompleted: result.StreamCompleted, LastEvent: result.LastEvent,
		UpstreamRequestID: result.UpstreamRequestID, ErrorKind: result.ErrorKind, ErrorSource: result.ErrorSource,
		Status:  result.Status,
		Outcome: result.Outcome, TTFTMs: result.TTFTMs, DurationMs: completed.Sub(started).Milliseconds(),
		CreatedAt: started, CompletedAt: completed, Error: result.Error, Attempts: attempts,
	})
}

func parseBodyModel(body []byte) string {
	model, _ := parseBodyAudit(body)
	return model
}

func parseBodyAudit(body []byte) (string, bool) {
	var payload struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model, payload.Stream
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
	// 并发拉取各上游：串行时任一上游超时都会线性累加到客户端等待时间。
	perUpstream := make([][]string, len(ups))
	var wg sync.WaitGroup
	for i, u := range ups {
		wg.Add(1)
		go func(index int, item *upstream.Upstream) {
			defer wg.Done()
			perUpstream[index] = s.upstreamModels(r.Context(), item)
		}(i, u)
	}
	wg.Wait()
	seen := make(map[string]bool)
	var ids []string
	for _, models := range perUpstream {
		for _, m := range models {
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

// forgetUpstreamModels 丢弃某上游的模型清单缓存，供上游删除后调用。
func (s *Server) forgetUpstreamModels(id int64) {
	s.modelMu.Lock()
	delete(s.modelCache, id)
	s.modelMu.Unlock()
}

// upstreamModels 取单个上游的模型列表，命中缓存(TTL 内)则直接返回，
// 否则实时拉取并写缓存；拉取失败时回退到上次缓存（可能过期），仍无则空。
// 同一上游同时只允许一次在途拉取，其余调用者等待并复用结果——否则缓存过期瞬间
// 的并发请求会各自打满上游连接。
func (s *Server) upstreamModels(ctx context.Context, u *upstream.Upstream) []string {
	s.modelMu.Lock()
	ent, cached := s.modelCache[u.ID]
	if cached && time.Since(ent.ts) < modelsTTL {
		s.modelMu.Unlock()
		return ent.models
	}
	if flight, ok := s.modelFlight[u.ID]; ok {
		s.modelMu.Unlock()
		select {
		case <-flight.done:
			return flight.models
		case <-ctx.Done():
			return ent.models
		}
	}
	flight := &modelFetch{done: make(chan struct{})}
	s.modelFlight[u.ID] = flight
	s.modelMu.Unlock()

	models, _, err := u.FetchModels(ctx, 10*time.Second)
	if err != nil {
		models = ent.models // 失败回退到旧缓存（无则 nil）
	} else {
		s.health.MarkModelsSupported(u.ID, models)
	}
	s.modelMu.Lock()
	if err == nil {
		s.modelCache[u.ID] = modelCacheEntry{models: models, ts: time.Now()}
	}
	delete(s.modelFlight, u.ID)
	s.modelMu.Unlock()
	flight.models = models
	close(flight.done)
	return models
}
