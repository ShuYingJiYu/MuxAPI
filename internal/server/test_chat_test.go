package server

import (
	"strings"
	"testing"
	"time"
)

// 验证 relayChatStream 正确把上游 OpenAI SSE 的 delta 拼成 content 事件，并以 test_complete 收尾。
func TestRelayChatStream(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{}}]}`, // 无内容块，跳过
		`data: [DONE]`,
	}, "\n")

	var events []testEvent
	(&Server{}).relayChatStream(strings.NewReader(upstream), func(e testEvent) { events = append(events, e) }, time.Now())

	var text, last string
	for _, e := range events {
		if e.Type == "content" {
			text += e.Text
		}
		last = e.Type
	}
	if text != "Hello" {
		t.Fatalf("应拼出 Hello，实际 %q", text)
	}
	if last != "test_complete" || !events[len(events)-1].Success {
		t.Fatalf("应以 test_complete success 收尾，实际 %+v", events[len(events)-1])
	}
}

// 验证上游 SSE 里夹带 error 块时立即转成 error 事件并停止。
func TestRelayChatStreamError(t *testing.T) {
	upstream := `data: {"error":{"message":"rate limited"}}`
	var events []testEvent
	(&Server{}).relayChatStream(strings.NewReader(upstream), func(e testEvent) { events = append(events, e) }, time.Now())
	if events[0].Type != "error" || events[0].Error != "rate limited" {
		t.Fatalf("应转成 error 事件，实际 %+v", events)
	}
}
