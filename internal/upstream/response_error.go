package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
)

type FailureKind string

const (
	FailureClient      FailureKind = "client_request"
	FailureUnsupported FailureKind = "model_unsupported"
	FailureRateLimit   FailureKind = "upstream_rate_limit"
	FailureChannel     FailureKind = "upstream_http"
	FailureTransient   FailureKind = "upstream_temporary_failure"
)

// ResponseError preserves the provider's actual error for audit and health
// feedback, including errors carried inside HTTP 200 JSON or SSE responses.
type ResponseError struct {
	Kind    FailureKind
	Status  int
	Code    string
	Message string
}

func (e *ResponseError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return string(e.Kind)
}

func ClassifyResponseError(status int, payload []byte) *ResponseError {
	var root map[string]json.RawMessage
	_ = json.Unmarshal(payload, &root)
	errorObject := root
	detected := false
	if raw, ok := root["response"]; ok {
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) == nil {
			if nested, ok := response["error"]; ok && meaningfulError(nested) {
				errorObject = response
				detected = true
			}
		}
	}
	if raw, ok := errorObject["error"]; ok && meaningfulError(raw) {
		detected = true
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil && nested != nil {
			errorObject = nested
		}
	}
	var typ, code, message string
	_ = json.Unmarshal(errorObject["type"], &typ)
	_ = json.Unmarshal(errorObject["code"], &code)
	_ = json.Unmarshal(errorObject["message"], &message)
	var rootType string
	_ = json.Unmarshal(root["type"], &rootType)
	detected = detected || rootType == "error" || rootType == "response.failed"
	if status < 400 && !detected {
		return nil
	}
	if message == "" {
		message = strings.TrimSpace(string(payload))
	}
	result := &ResponseError{Status: status, Code: code, Message: message}
	// New API reports a group with no route for one model as HTTP 503 plus a
	// structured model_not_found code ("No available channel for model …").
	// That is a capability gap of this credential, not an outage of the whole
	// upstream: counting it as a channel failure opens the breaker and takes
	// every other model on that upstream offline. Bare 503s stay failures.
	if status == http.StatusServiceUnavailable &&
		(strings.EqualFold(strings.TrimSpace(code), "model_not_found") || strings.EqualFold(strings.TrimSpace(typ), "model_not_found")) {
		result.Kind = FailureUnsupported
		return result
	}
	// Some gateways deny a single model with a plain-text body and no error
	// code at all: 403 "This token has no access to model X" or 503 "No
	// available channel for model X under group …". Both are capability gaps
	// of this credential for one model. Counting them as channel failures
	// opens the breaker and takes every other model on that upstream offline,
	// which is exactly the collateral 403s are known to cause here.
	if isModelScopedDenial(status, code, typ, message) {
		result.Kind, result.Status = FailureUnsupported, http.StatusNotFound
		return result
	}
	if IsFailureStatus(status) {
		result.Kind = FailureChannel
		return result
	}
	signals := []string{strings.ToLower(code), strings.ToLower(typ)}
	for _, signal := range signals {
		switch signal {
		case "context_length_exceeded", "invalid_request_error", "invalid_argument", "invalid_parameter", "invalid_value", "content_policy_violation":
			result.Kind, result.Status = FailureClient, http.StatusBadRequest
			return result
		case "model_not_found":
			// A 503 is temporary lack of capacity, even if a gateway reuses
			// model_not_found for its own unavailable-channel response.
			if status != http.StatusServiceUnavailable {
				result.Kind, result.Status = FailureUnsupported, http.StatusNotFound
				return result
			}
		case "invalid_api_key", "authentication_error", "permission_error", "permission_denied", "insufficient_quota", "billing_hard_limit_reached":
			result.Kind = FailureChannel
			if status < 400 {
				result.Status = http.StatusUnauthorized
			}
			return result
		case "rate_limit_exceeded", "rate_limit_error", "rate_limit", "too_many_requests":
			result.Kind, result.Status = FailureRateLimit, http.StatusTooManyRequests
			return result
		}
	}
	switch {
	case status == http.StatusTooManyRequests:
		result.Kind = FailureRateLimit
	case IsFailureStatus(status):
		result.Kind = FailureChannel
	case status >= 400:
		result.Kind = FailureClient
	default:
		result.Kind = FailureTransient
	}
	return result
}

func meaningfulError(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "false", `""`, "{}", "[]":
		return false
	default:
		return true
	}
}

// modelScopedDenialPatterns are the plain-text phrasings New API and similar
// gateways use when one credential lacks access to one model. They must match
// a model reference so a channel-wide outage is never downgraded by accident.
var modelScopedDenialPatterns = []string{
	"has no access to model",
	"no access to model",
	"no available channel for model",
	"does not have access to model",
	"model is not allowed",
	"model not allowed for this token",
}

// isModelScopedDenial reports whether this failure denies one model rather
// than breaking the whole upstream. Only 4xx capability statuses qualify: a
// 500-level body that happens to mention a model stays a channel failure.
func isModelScopedDenial(status int, code, typ, message string) bool {
	if status != http.StatusForbidden && status != http.StatusBadRequest && status != http.StatusNotFound && status != http.StatusServiceUnavailable {
		return false
	}
	// A structured auth/quota/billing code means the credential itself is
	// broken, which affects every model, not just the one in the message.
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_api_key", "authentication_error", "permission_error", "permission_denied", "insufficient_quota", "billing_hard_limit_reached":
		return false
	}
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "invalid_api_key", "authentication_error", "insufficient_quota", "billing_hard_limit_reached":
		return false
	}
	text := strings.ToLower(message)
	if !strings.Contains(text, "model") {
		return false
	}
	for _, pattern := range modelScopedDenialPatterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

// InspectSSEEvent is shared by forwarding and active monitoring. Error wins
// over completion; a following [DONE] cannot turn a failed stream into success.
func InspectSSEEvent(event string, payload []byte) (completed bool, failure *ResponseError) {
	text := strings.TrimSpace(string(payload))
	if text == "[DONE]" {
		return true, nil
	}
	failure = ClassifyResponseError(http.StatusOK, payload)
	if failure == nil && (event == "error" || event == "response.failed") {
		failure = &ResponseError{Kind: FailureTransient, Status: http.StatusOK, Message: text}
	}
	if failure != nil {
		return false, failure
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(payload, &root)
	var typ string
	_ = json.Unmarshal(root["type"], &typ)
	for _, value := range []string{event, typ} {
		switch value {
		case "response.completed", "message_stop", "message.completed", "done", "[done]":
			return true, nil
		}
	}
	var candidates []struct {
		FinishReason string `json:"finishReason"`
	}
	_ = json.Unmarshal(root["candidates"], &candidates)
	for _, candidate := range candidates {
		if candidate.FinishReason != "" {
			return true, nil
		}
	}
	return false, nil
}

func ParseSSEBlock(block []byte) (event string, data []byte) {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(string(block), "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return event, []byte(strings.Join(lines, "\n"))
}

func InspectSSE(payload []byte) (completed bool, failure *ResponseError) {
	stream := strings.ReplaceAll(string(payload), "\r\n", "\n")
	for _, block := range strings.Split(stream, "\n\n") {
		event, data := ParseSSEBlock([]byte(block))
		done, failed := InspectSSEEvent(event, data)
		if failed != nil {
			return false, failed
		}
		completed = completed || done
	}
	return completed, nil
}
