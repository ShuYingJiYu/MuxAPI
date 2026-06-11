package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Upstream 上游抽象。MVP 只做 relay 中转透传一种实现。
// 上游是全局凭证池成员；Priority/Weight 是「组内视图」字段——
// 由 store 从 group_upstreams 中间表 JOIN 填充，表示该上游在某分组内的调度策略。
type Upstream struct {
	ID           int64
	Name         string
	BaseURL      string // 中转站地址
	APIKey       string // 透传凭证
	Proxy        string // 转发/探测走的代理出口(空=按环境变量或直连)
	Priority     int    // 组内视图：越小越优先
	Weight       int    // 组内视图：同优先级层分流权重
	Enabled      bool
	ChannelProbe bool // 渠道级探测：探任一模型成功即视整渠道可用（探测复活连带 + 运行时列收起模型徽章）
}

// ProxyTransport 按代理 URL 构建 Transport：空则回退到环境变量(HTTPS_PROXY)，解析失败也回退。
// 每次新建一个独立 Transport，供探测/拉模型这类一次性短连接使用。
func ProxyTransport(proxy string) *http.Transport {
	t := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if proxy != "" {
		if p, err := url.Parse(proxy); err == nil {
			t.Proxy = http.ProxyURL(p)
		}
	}
	return t
}

// sharedTransports 按代理出口缓存复用的 Transport（key=proxy 字符串）。
// 转发热路径每请求新建 Transport 会泄漏连接/fd（旧实现），改为按出口共享一份、
// 带空闲连接回收，long-run 不再耗尽资源。
var sharedTransports sync.Map // proxy string -> *http.Transport

// SharedTransport 取（或惰性建）该代理出口共享的 Transport，供转发热路径复用。
// 设 IdleConnTimeout=90s、MaxIdleConnsPerHost=100 让空闲连接及时回收又能复用；
// 注意：不在此设 ResponseHeaderTimeout（它随容忍线动态变化），由调用方在 context/client 层控制。
func SharedTransport(proxy string) *http.Transport {
	if v, ok := sharedTransports.Load(proxy); ok {
		return v.(*http.Transport)
	}
	t := ProxyTransport(proxy)
	t.IdleConnTimeout = 90 * time.Second
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 100
	// LoadOrStore 防并发重复建：多个 goroutine 同时 miss 时只留一份。
	actual, _ := sharedTransports.LoadOrStore(proxy, t)
	return actual.(*http.Transport)
}

// NewTransport 该上游专属共享 Transport（含其代理出口），转发热路径复用以免连接泄漏。
func (u *Upstream) NewTransport() *http.Transport { return SharedTransport(u.Proxy) }

// IsFailureStatus 判断上游响应码是否表示「该上游此刻不可用」，需触发故障切换/熔断摘除。
// 涵盖：5xx 服务端错误、429 限流，以及 401/402/403/408 这类凭证/余额/超时问题。
// MuxAPI 用自存 api_key 转发（客户端只提供接入 key），故 401/402/403 必为上游侧
// 凭证或账户余额问题，切换到下一上游是安全且正确的。
// 400/404 视为请求内容本身的问题（畸形参数/模型不存在），透传给客户端，
// 不触发切换——避免无意义重试，也避免把客户端错误误判成上游故障而熔断健康上游。
func IsFailureStatus(code int) bool {
	if code >= 500 {
		return true
	}
	switch code {
	case http.StatusUnauthorized, // 401
		http.StatusPaymentRequired, // 402
		http.StatusForbidden,       // 403
		http.StatusRequestTimeout,  // 408
		http.StatusTooManyRequests: // 429
		return true
	}
	return false
}

// FailIsUpstreamLevel 判断「失败」是否属于上游级（应熔断整个上游、所有模型连坐），
// 而非仅当前模型。两类如此：
//   - 凭证/余额类 401/402/403：与具体模型无关，是渠道凭证本身坏了；
//   - 网关类 502/503/504：中转站整体回源故障/过载，并非单模型问题，连坐避免请求
//     在同一坏渠道上逐模型空转。
//
// 其余失败（429 限流、408 超时、网络错误）一律视为模型级局部故障，只熔断 (上游,模型)，
// 避免单个模型抖动误伤整个上游的其他模型。
func FailIsUpstreamLevel(code int) bool {
	switch code {
	case http.StatusUnauthorized, // 401
		http.StatusPaymentRequired,    // 402
		http.StatusForbidden,          // 403
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout:     // 504
		return true
	}
	return false
}

// BuildRequest 构建发往上游的请求：黑名单透传客户端请求头（保留 User-Agent/x-stainless-* 等），
// 仅覆盖凭证为上游 key、重算 Content-Length。URL = TrimSuffix(base_url,"/") + 客户端原始路径(path)。
func (u *Upstream) BuildRequest(method, path string, body io.Reader, clientHeader http.Header) (*http.Request, error) {
	url := strings.TrimSuffix(u.BaseURL, "/") + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range clientHeader { // 全量透传客户端请求头
		req.Header[k] = vs
	}
	req.Header.Del("Content-Length") // 由 body 重新计算，避免与原始长度冲突
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// 覆盖凭证为上游自己的 key（客户端那个是 sub2api 发的，上游不认）
	req.Header.Set("Authorization", "Bearer "+u.APIKey)
	req.Header.Set("x-api-key", u.APIKey)
	return req, nil
}

// FetchModels 实时拉该上游的 /v1/models，解析 OpenAI 风格 {"data":[{"id":...}]}
// 返回模型 ID 列表。既用于上游连通测试，也用于下游 /v1/models 汇总。
// 返回 (models, status, error)：status 是上游 HTTP 状态码（网络错误为 0）。
func (u *Upstream) FetchModels(timeout time.Duration) ([]string, int, error) {
	req, err := u.BuildRequest(http.MethodGet, "/v1/models", nil, http.Header{})
	if err != nil {
		return nil, 0, err
	}
	client := &http.Client{Timeout: timeout, Transport: ProxyTransport(u.Proxy)}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(body, &parsed)
	models := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, resp.StatusCode, nil
}

// HTTPError 上游返回非 2xx 时携带状态码与响应体片段。
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return e.Body }
