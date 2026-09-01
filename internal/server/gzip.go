package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzipWriterPool 复用 gzip.Writer，避免每次请求分配 ~64KB 编码缓冲。
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// gzipResponseWriter 在客户端支持 gzip 时把响应体透明压缩。
// 首次 Write 才写 Content-Encoding/Vary 头，保证 http.Error 早退时同样带上。
// 只支持 admin 类 JSON 响应；不能用于 SSE/事件流（那些不能 buffer 或延迟到 Close）。
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	statusCode  int
}

func (g *gzipResponseWriter) prepare() {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	h := g.ResponseWriter.Header()
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length") // 压缩后长度会变，让 net/http 走 chunked
	if g.statusCode == 0 {
		g.statusCode = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(g.statusCode)
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.statusCode = code
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	g.prepare()
	return g.gz.Write(p)
}

// Flush 让 chunked JSON 分片实时可见；先 flush gzip 再 flush 底层。
func (g *gzipResponseWriter) Flush() {
	if !g.wroteHeader {
		g.prepare()
	}
	_ = g.gz.Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// clientAcceptsGzip 只认 gzip；忽略 q=0 显式禁用。
func clientAcceptsGzip(h http.Header) bool {
	for _, value := range h.Values("Accept-Encoding") {
		for _, part := range strings.Split(value, ",") {
			token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			if !strings.EqualFold(token, "gzip") && token != "*" {
				continue
			}
			if strings.Contains(strings.ToLower(params), "q=0") {
				continue
			}
			return true
		}
	}
	return false
}

// gzipMiddleware 包住 admin 路由。客户端不接受 gzip 时直通。
// handler 完全没写任何字节时不启用压缩，让空响应保持 0-byte。
func gzipMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !clientAcceptsGzip(r.Header) {
			next(w, r)
			return
		}
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w)
		grw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		defer func() {
			if grw.wroteHeader {
				_ = gz.Close()
			}
			gz.Reset(io.Discard)
			gzipWriterPool.Put(gz)
		}()
		next(grw, r)
		// handler 只 WriteHeader 不写 body 的场景：statusCode 已缓存但 prepare() 还没走过
		if !grw.wroteHeader && grw.statusCode != 0 {
			w.WriteHeader(grw.statusCode)
		}
	}
}
