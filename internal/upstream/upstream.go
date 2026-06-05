package upstream

import (
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Upstream 上游抽象。MVP 只做 relay 中转透传一种实现。
// 上游是全局凭证池成员；Priority/Weight 是「组内视图」字段——
// 由 store 从 group_upstreams 中间表 JOIN 填充，表示该上游在某分组内的调度策略。
type Upstream struct {
	ID       int64
	Name     string
	BaseURL  string // 中转站地址
	APIKey   string // 透传凭证
	Proxy    string // 转发/探测走的代理出口(空=按环境变量或直连)
	Priority int    // 组内视图：越小越优先
	Weight   int    // 组内视图：同优先级层分流权重
	Enabled  bool
}

// ProxyTransport 按代理 URL 构建 Transport：空则回退到环境变量(HTTPS_PROXY)，解析失败也回退。
func ProxyTransport(proxy string) *http.Transport {
	t := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if proxy != "" {
		if p, err := url.Parse(proxy); err == nil {
			t.Proxy = http.ProxyURL(p)
		}
	}
	return t
}

// NewTransport 该上游专属 Transport（含其代理出口）。
func (u *Upstream) NewTransport() *http.Transport { return ProxyTransport(u.Proxy) }

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
