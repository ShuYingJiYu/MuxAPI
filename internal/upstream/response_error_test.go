package upstream

import "testing"

func TestResponseErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		payload string
		kind    FailureKind
	}{
		{"successful JSON", 200, `{"id":"response","error":null}`, ""},
		{"parameter in JSON", 200, `{"error":{"type":"invalid_request_error","message":"bad temperature"}}`, FailureClient},
		{"context length", 200, `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too many tokens"}}}`, FailureClient},
		{"model missing", 200, `{"error":{"code":"model_not_found","type":"invalid_request_error"}}`, FailureUnsupported},
		{"temporary server", 200, `{"type":"response.failed","response":{"error":{"code":"server_error","message":"overloaded"}}}`, FailureTransient},
		{"rate limit", 200, `{"error":{"type":"rate_limit_error"}}`, FailureRateLimit},
		{"invalid key", 200, `{"error":{"code":"invalid_api_key"}}`, FailureChannel},
		{"quota", 429, `{"error":{"code":"insufficient_quota"}}`, FailureChannel},
		{"temporary capacity", 503, `{"error":{"code":"model_not_found","type":"invalid_request_error"}}`, FailureChannel},
		{"normal invalid request", 422, `{"message":"invalid input"}`, FailureClient},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := ClassifyResponseError(test.status, []byte(test.payload))
			if test.kind == "" {
				if failure != nil {
					t.Fatalf("success classified as failure: %v", failure)
				}
				return
			}
			if failure == nil || failure.Kind != test.kind {
				t.Fatalf("failure=%+v want=%s", failure, test.kind)
			}
		})
	}
}

func TestSSEErrorWinsOverCompletionAcrossProtocols(t *testing.T) {
	for _, payload := range []string{
		"data: {\"error\":{\"message\":\"overloaded\"}}\n\ndata: [DONE]\n\n",
		"event: error\r\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\r\n\r\ndata: [DONE]\r\n\r\n",
		"event: response.failed\ndata: {\"response\":{\"error\":{\"code\":\"server_error\"}}}\n\ndata: [DONE]\n\n",
		"event: error\ndata: {\n" + "data: \"error\":{\"message\":\"failed\"}\n" + "data: }\n\n",
	} {
		completed, failure := InspectSSE([]byte(payload))
		if completed || failure == nil {
			t.Fatalf("error was accepted as successful completion: %q", payload)
		}
	}
}
