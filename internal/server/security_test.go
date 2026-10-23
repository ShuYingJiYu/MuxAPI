package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/store"
)

func TestSecurityClientIP(t *testing.T) {
	for _, tc := range []struct {
		name, proxies, peer, chain, want string
		forwarded                        bool
	}{
		{"default ignores loopback headers", "", "127.0.0.1:10", "192.0.2.1", "127.0.0.1", false},
		{"untrusted peer", "127.0.0.1", "192.0.2.9:10", "192.0.2.1", "192.0.2.9", false},
		{"trusted loopback", "127.0.0.1", "127.0.0.1:10", "192.0.2.1", "192.0.2.1", true},
		{"spoofed left value", "127.0.0.1", "127.0.0.1:10", "198.51.100.3, 192.0.2.1", "192.0.2.1", true},
		{"trusted chain", "127.0.0.1,10.0.0.0/24", "127.0.0.1:10", "192.0.2.1, 10.0.0.3", "192.0.2.1", true},
		{"invalid chain", "127.0.0.1", "127.0.0.1:10", "invalid,192.0.2.1", "127.0.0.1", false},
		{"ipv6", "::1", "[::1]:10", "2001:db8::1", "2001:db8::1", true},
		{"oversize", "127.0.0.1", "127.0.0.1:10", strings.Repeat("a", 5000), "127.0.0.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			if err := s.SetTrustedProxies(tc.proxies); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "/admin/auth", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("X-Forwarded-For", tc.chain)
			r.Header.Set("CF-Connecting-IP", "203.0.113.99")
			r.Header.Set("X-Real-IP", "203.0.113.99")
			got, _, forwarded := s.securityClientIP(r)
			if got != tc.want || forwarded != tc.forwarded {
				t.Fatalf("got %s %t", got, forwarded)
			}
		})
	}
	for _, value := range []string{"invalid", "0.0.0.0/0", "::/0", "127.0.0.1,", "fe80::1%eth0"} {
		if err := (&Server{}).SetTrustedProxies(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestSecurityAuthAndDirectAccess(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(nil, "test-secret-never-log", st, nil, nil, nil, 0)
	h := s.Handler()
	call := func(method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("User-Agent", "Browser\nTest")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("POST", "/admin/auth", "wrong"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("POST", "/admin/auth", s.adminToken); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "/admin/auth", s.adminToken); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "/admin/groups?token=do-not-log", "wrong"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for i := 0; i < 3; i++ {
		if w := call("GET", "/admin/groups", s.adminToken); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	// 路由子路径可能带任意值，只记录注册的模板。
	call("DELETE", "/admin/keys/private-path-value", "wrong")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.FlushSecurity(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := st.SecurityLogs(ctx, store.SecurityFilter{Size: 100})
	if err != nil {
		t.Fatal(err)
	}
	var loginCount, pollCount int64
	for _, row := range page.Entries {
		if row.Kind == "login" && row.Success {
			loginCount += row.Count
		}
		if row.Endpoint == "/admin/groups" && row.Success {
			pollCount += row.Count
		}
		if strings.ContainsAny(row.UserAgent, "\n\r") {
			t.Fatal("unsafe UA")
		}
	}
	if loginCount != 1 || pollCount != 3 {
		t.Fatalf("login %d poll %d", loginCount, pollCount)
	}
	body, _ := json.Marshal(page)
	for _, sensitive := range []string{s.adminToken, "do-not-log", "private-path-value"} {
		if strings.Contains(string(body), sensitive) {
			t.Fatalf("leaked sensitive value %q", sensitive)
		}
	}
	if w := call("GET", "/admin/security-logs", "wrong"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := call("GET", "/admin/security-logs?result=failure&size=2", s.adminToken)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"retention_days":15`) {
		t.Fatal(w.Body.String())
	}
	for _, query := range []string{"ip=oops", "page=0", "size=101", "from=-1", "result=bad", "from=100&to=99"} {
		if w := call("GET", "/admin/security-logs?"+query, s.adminToken); w.Code != 400 {
			t.Fatalf("accepted %s: %d", query, w.Code)
		}
	}
	if w := call("POST", "/admin/security-logs", s.adminToken); w.Code != 405 {
		t.Fatal(w.Code)
	}
}

func TestSecurityReadOnlyAndNoToken(t *testing.T) {
	for _, token := range []string{"", "test-token"} {
		st, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		s := New(nil, token, st, nil, nil, nil, 0)
		s.SetReadOnly(token != "")
		r := httptest.NewRequest("POST", "/admin/auth", nil)
		r.Header.Set("x-api-key", token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		if err := st.FlushSecurity(context.Background()); err != nil {
			t.Fatal(err)
		}
		page, err := st.SecurityLogs(context.Background(), store.SecurityFilter{Size: 20})
		if err != nil || page.Total != 0 {
			t.Fatalf("audit write in disabled mode: %+v %v", page, err)
		}
		st.Close()
	}
}
