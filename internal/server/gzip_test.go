package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestAdminGzipCompresses 验证客户端声明 Accept-Encoding: gzip 时，
// admin 端点返回 Content-Encoding: gzip 且解压后结构合法。
func TestAdminGzipCompresses(t *testing.T) {
	ts, _, tok := newAdminTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/upstreams", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept-Encoding", "gzip")

	// http.DefaultClient 默认会自动加 gzip 并透明解压，禁掉让我们能看到原始 header。
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q, want gzip", got)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary=%q, want to include Accept-Encoding", got)
	}
	// 注：net/http 会为小响应缓冲后重算 Content-Length，值合法(=压缩后字节数)就行。
	// 关键是我们要 del 掉 handler 提供的原始 Content-Length（避免与压缩后长度矛盾）。

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	defer gr.Close()
	raw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gzip decode: %v", err)
	}

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decompressed body is not JSON: %v; body=%q", err, raw)
	}
}

// TestAdminGzipSkippedWithoutAcceptEncoding 客户端不声明 gzip 时，服务端不能主动 gzip。
func TestAdminGzipSkippedWithoutAcceptEncoding(t *testing.T) {
	ts, _, tok := newAdminTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/upstreams", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	// 显式清空 Accept-Encoding，防止 http.Transport 自动加 gzip
	req.Header.Set("Accept-Encoding", "identity")

	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding=%q, want empty (no compression)", got)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("plain body not JSON: %v; body=%q", err, body)
	}
}

// TestAdminGzipErrorResponse http.Error 走的 unauthorized 响应也应正确 gzip
// （避免中间件在错误路径下把 header 写乱）。
func TestAdminGzipErrorResponse(t *testing.T) {
	ts, _, _ := newAdminTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/upstreams", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("Accept-Encoding", "gzip")

	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("error response not gzipped: Content-Encoding=%q", resp.Header.Get("Content-Encoding"))
	}
	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("error body is not gzip: %v", err)
	}
	raw, _ := io.ReadAll(gr)
	if !strings.Contains(string(raw), "unauthorized") {
		t.Fatalf("decompressed error body=%q, want 'unauthorized'", raw)
	}
}

// TestAdminGzipCompressionRatio 冒烟测:确认 gzip 对典型 JSON payload 至少压到 1/2 以下。
// admin/upstreams 空库返回 []，无法体现压缩效果，用手工大 payload 直测中间件。
func TestAdminGzipCompressionRatio(t *testing.T) {
	// 构造一个和 routing/decisions 差不多形状的 payload
	payload := map[string]any{"items": make([]map[string]any, 50)}
	for i := 0; i < 50; i++ {
		payload["items"].([]map[string]any)[i] = map[string]any{
			"id": i, "reason": strings.Repeat("selected_upstream_priority_forecast_win ", 3),
			"candidates": []string{"aws0", "aws0ex", "awsqaex", "aws0sushua"},
		}
	}
	raw, _ := json.Marshal(payload)

	handler := gzipMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	})

	req, _ := http.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	rec := &captureWriter{header: make(http.Header), body: new(bytes.Buffer)}
	handler(rec, req)

	if rec.header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding=%q, want gzip", rec.header.Get("Content-Encoding"))
	}
	compressed := rec.body.Len()
	if compressed >= len(raw)/2 {
		t.Fatalf("compressed size %d not < half of raw %d", compressed, len(raw))
	}
	t.Logf("raw=%d compressed=%d ratio=%.2f", len(raw), compressed, float64(compressed)/float64(len(raw)))
}

// captureWriter 用于本包内单测中间件时抓取字节；http.ResponseWriter 最小实现。
type captureWriter struct {
	header http.Header
	body   *bytes.Buffer
	status int
}

func (c *captureWriter) Header() http.Header       { return c.header }
func (c *captureWriter) WriteHeader(code int)      { c.status = code }
func (c *captureWriter) Write(p []byte) (int, error) { return c.body.Write(p) }
