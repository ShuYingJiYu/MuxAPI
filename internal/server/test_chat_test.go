package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/translate"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestRelayTranslatedTestStreamOpenAI(t *testing.T) {
	original := []byte(`{"model":"gpt-test","input":"hello","stream":true}`)
	exchange, err := translate.NewExchange(translate.OpenAIResponses, translate.OpenAI, "gpt-test", true, original)
	if err != nil {
		t.Fatal(err)
	}
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: [DONE]`,
	}, "\n\n")

	var events []testEvent
	(&Server{}).relayTranslatedTestStream(context.Background(), strings.NewReader(stream), exchange, func(event testEvent) {
		events = append(events, event)
	}, time.Now())

	var text string
	for _, event := range events {
		if event.Type == "content" {
			text += event.Text
		}
	}
	if text != "Hello" {
		t.Fatalf("translated content = %q", text)
	}
	last := events[len(events)-1]
	if last.Type != "test_complete" || !last.Success {
		t.Fatalf("last event = %+v", last)
	}
}

func TestRelayTranslatedTestStreamRejectsEarlyEOF(t *testing.T) {
	original := []byte(`{"model":"gpt-test","input":"hello","stream":true}`)
	exchange, err := translate.NewExchange(translate.OpenAIResponses, translate.OpenAIResponses, "gpt-test", true, original)
	if err != nil {
		t.Fatal(err)
	}
	stream := `data: {"type":"response.output_text.delta","delta":"partial"}`
	var events []testEvent
	(&Server{}).relayTranslatedTestStream(context.Background(), strings.NewReader(stream), exchange, func(event testEvent) {
		events = append(events, event)
	}, time.Now())
	last := events[len(events)-1]
	if last.Type != "error" || !strings.Contains(last.Error, "response.completed") {
		t.Fatalf("last event = %+v", last)
	}
}

func TestAdminUpstreamTestUsesConfiguredClaudeProtocol(t *testing.T) {
	type observedRequest struct {
		path             string
		anthropicVersion string
		body             string
	}
	observed := make(chan observedRequest, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observed <- observedRequest{r.URL.Path, r.Header.Get("anthropic-version"), string(body)}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":2}}}\n\n",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"translated hello\"}}\n\n",
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n",
			"data: {\"type\":\"message_stop\"}\n\n",
		} {
			io.WriteString(w, event)
		}
	}))
	defer upstreamServer.Close()

	server, store, token := newAdminTestServer(t)
	if err := store.Create(&upstream.Upstream{
		Name: "claude", BaseURL: upstreamServer.URL, APIKey: "key", Protocol: "claude", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	upstreams, err := store.List()
	if err != nil || len(upstreams) != 1 {
		t.Fatalf("upstreams = %+v, err = %v", upstreams, err)
	}
	response := adminReq(t, http.MethodPost, server.URL+"/admin/upstreams/1/test?model=claude-test", token, "")
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "translated hello") || !strings.Contains(string(body), "test_complete") {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}

	request := <-observed
	if request.path != "/v1/messages" || request.anthropicVersion == "" {
		t.Fatalf("request = %+v", request)
	}
	if !strings.Contains(request.body, `"messages"`) || strings.Contains(request.body, `"input"`) {
		t.Fatalf("request was not translated: %s", request.body)
	}
}
