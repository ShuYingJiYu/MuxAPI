package forward

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 回归：上游返回长中文错误体时，按字节硬切会产生非法 UTF-8，
// 使审计 INSERT 被 Postgres 拒收、整条请求记录丢失。
func TestClipErrKeepsValidUTF8(t *testing.T) {
	payload := `{"error":{"message":"` + strings.Repeat("上游渠道当前不可用请稍后重试", 50) + `"}}`
	clipped := clipErr(payload)
	if !utf8.ValidString(clipped) {
		t.Fatalf("clipped error must stay valid UTF-8, got tail % x", clipped[max(0, len(clipped)-6):])
	}
	if len(clipped) > 500 {
		t.Fatalf("clipped error must respect the byte budget, got %d", len(clipped))
	}
	if len(clipped) < 490 {
		t.Fatalf("clip should only back off to a rune boundary, got %d bytes", len(clipped))
	}
}

// 上游可能返回二进制或被截断的响应体，非法字节必须在入库前清掉。
func TestClipErrDropsInvalidBytes(t *testing.T) {
	clipped := clipErr("upstream said \xff\xfe boom")
	if !utf8.ValidString(clipped) {
		t.Fatalf("invalid bytes must be removed, got %q", clipped)
	}
	if !strings.Contains(clipped, "boom") {
		t.Fatalf("readable text must survive, got %q", clipped)
	}
}

func TestClipLabelKeepsValidUTF8(t *testing.T) {
	clipped := clipLabel(strings.Repeat("事件", 60))
	if !utf8.ValidString(clipped) {
		t.Fatalf("clipped label must stay valid UTF-8, got %q", clipped)
	}
	if len(clipped) > 80 {
		t.Fatalf("clipped label must respect the byte budget, got %d", len(clipped))
	}
}

// 短文本与 ASCII 不应被改动。
func TestClipUTF8PassesShortValues(t *testing.T) {
	if got := clipUTF8("plain error", 500); got != "plain error" {
		t.Fatalf("short value must pass through, got %q", got)
	}
}
