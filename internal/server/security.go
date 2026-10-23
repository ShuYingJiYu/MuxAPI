package server

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/store"
)

// SetTrustedProxies 仅在 HTTP 服务启动前配置，不默认信任 loopback 转发头。
func (s *Server) SetTrustedProxies(value string) error {
	var prefixes []netip.Prefix
	if strings.TrimSpace(value) != "" {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			prefix, err := netip.ParsePrefix(item)
			if err != nil {
				addr, addrErr := netip.ParseAddr(item)
				if addrErr != nil || addr.Zone() != "" {
					return fmt.Errorf("invalid trusted proxy IP/CIDR")
				}
				addr = addr.Unmap()
				prefix = netip.PrefixFrom(addr, addr.BitLen())
			}
			if prefix.Bits() == 0 {
				return fmt.Errorf("trusted proxy CIDR must not include every address")
			}
			prefixes = append(prefixes, prefix.Masked())
		}
	}
	s.trustedProxies = prefixes
	return nil
}

func (s *Server) trustedProxy(ip netip.Addr) bool {
	for _, prefix := range s.trustedProxies {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) securityClientIP(r *http.Request) (client, peer string, forwarded bool) {
	addr, valid := parseRequestIP(r.RemoteAddr)
	if !valid {
		return "", "", false
	}
	addr = addr.Unmap()
	peer = addr.String()
	if !s.trustedProxy(addr) {
		return peer, peer, false
	}
	value := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if value == "" || len(value) > 4096 {
		return peer, peer, false
	}
	chain := strings.Split(value, ",")
	if len(chain) > 32 {
		return peer, peer, false
	}
	// 整条链必须合法。取右侧第一个非可信节点，不采用攻击者附加的最左侧地址。
	addresses := make([]netip.Addr, 0, len(chain))
	for _, part := range chain {
		ip, err := netip.ParseAddr(strings.TrimSpace(part))
		if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
			return peer, peer, false
		}
		addresses = append(addresses, ip.Unmap())
	}
	for i := len(addresses) - 1; i >= 0; i-- {
		addr = addresses[i]
		if !s.trustedProxy(addr) {
			break
		}
	}
	return addr.String(), peer, true
}

func (s *Server) recordSecurity(r *http.Request, success bool) {
	if s.readOnly || s.adminToken == "" || s.store == nil {
		return
	}
	kind := "access"
	if r.URL.Path == "/admin/auth" && r.Method == http.MethodPost {
		kind = "login"
	}
	client, peer, forwarded := s.securityClientIP(r)
	// 使用已注册的路由模板，不复制任意路径参数中的潜在凭据。
	endpoint := r.Pattern
	if endpoint == "" {
		endpoint = "/admin/*"
	}
	if strings.HasSuffix(endpoint, "/") {
		endpoint += "{item}"
	}
	s.store.EnqueueSecurity(store.SecurityLog{
		FirstAt: time.Now().Unix(), Kind: kind, Success: success,
		ClientIP: client, PeerIP: peer, Forwarded: forwarded,
		UserAgent: r.UserAgent(), Method: r.Method, Endpoint: endpoint,
	})
}

func (s *Server) adminAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]bool{"authenticated": s.adminToken != "", "read_only": s.readOnly})
}

func (s *Server) adminSecurityLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	filter := store.SecurityFilter{IP: strings.TrimSpace(q.Get("ip")), Result: q.Get("result"),
		Kind: q.Get("kind"), View: q.Get("view"), Page: 1, Size: 20}
	if (filter.Kind != "" && filter.Kind != "login" && filter.Kind != "access") ||
		(filter.View != "" && filter.View != "events" && filter.View != "all") {
		http.Error(w, "invalid event filter", http.StatusBadRequest)
		return
	}
	if filter.IP != "" {
		ip, err := netip.ParseAddr(filter.IP)
		if err != nil || ip.Zone() != "" {
			http.Error(w, "invalid IP", http.StatusBadRequest)
			return
		}
		filter.IP = ip.Unmap().String()
	}
	switch filter.Result {
	case "", "success", "failure", "attention":
	default:
		http.Error(w, "invalid result", http.StatusBadRequest)
		return
	}
	for name, target := range map[string]*int{"page": &filter.Page, "size": &filter.Size} {
		if raw, ok := q[name]; ok {
			n, err := strconv.Atoi(raw[0])
			limit := 100
			if name == "page" {
				limit = 1000000
			}
			if err != nil || n < 1 || n > limit {
				http.Error(w, "invalid pagination", http.StatusBadRequest)
				return
			}
			*target = n
		}
	}
	for name, target := range map[string]*int64{"from": &filter.From, "to": &filter.To} {
		if raw, ok := q[name]; ok {
			n, err := strconv.ParseInt(raw[0], 10, 64)
			if err != nil || n < 0 || n > time.Now().Unix() {
				http.Error(w, "invalid time range", http.StatusBadRequest)
				return
			}
			*target = n
		}
	}
	if raw, ok := q["range"]; ok {
		// 第一次查询由服务器确定时间；之后翻页复用返回的 from/to。
		durations := map[string]int64{"1h": 3600, "24h": 86400, "7d": 7 * 86400, "15d": 15 * 86400}
		seconds, valid := durations[raw[0]]
		if !valid || q.Has("from") || q.Has("to") {
			http.Error(w, "invalid time range", http.StatusBadRequest)
			return
		}
		filter.To = time.Now().Unix()
		filter.From = filter.To - seconds
	}
	if filter.To != 0 && filter.From > filter.To {
		http.Error(w, "invalid time range", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	page, err := s.store.SecurityLogs(ctx, filter)
	if err != nil {
		http.Error(w, "security logs unavailable", http.StatusServiceUnavailable)
		return
	}
	drops, failed := s.store.SecurityLogStatus()
	writeJSON(w, map[string]any{
		"entries": page.Entries, "total": page.Total, "summary": page.Summary,
		"page": page.Page, "size": page.Size, "from": page.From, "to": page.To,
		"retention_days":      store.SecurityRetentionDays,
		"dropped_since_start": drops, "write_failed": failed,
		"authentication_enabled": s.adminToken != "", "recording_enabled": !s.readOnly && s.adminToken != "",
		"trusted_proxy_configured": len(s.trustedProxies) > 0,
	})
}
