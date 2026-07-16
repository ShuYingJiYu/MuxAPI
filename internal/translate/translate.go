package translate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/muxbuiltin"
)

// Format is a request/response schema understood by the CPA translator.
type Format string

const (
	Passthrough     Format = "passthrough"
	OpenAI          Format = "openai"
	OpenAIResponses Format = "openai-response"
	Claude          Format = "claude"
	Codex           Format = "codex"
)

var ErrUnsupported = errors.New("protocol translation is not supported")

func NormalizeFormat(value string) (Format, bool) {
	format := Format(strings.ToLower(strings.TrimSpace(value)))
	if format == "" {
		format = Passthrough
	}
	switch format {
	case Passthrough, OpenAI, OpenAIResponses, Claude, Codex:
		return format, true
	default:
		return "", false
	}
}

func SourceFromPath(path string) (Format, bool) {
	switch strings.TrimSuffix(strings.TrimSpace(path), "/") {
	case "/v1/chat/completions":
		return OpenAI, true
	case "/v1/responses":
		return OpenAIResponses, true
	case "/v1/messages":
		return Claude, true
	default:
		return "", false
	}
}

func TargetPath(format Format, originalPath string) (string, error) {
	switch format {
	case Passthrough:
		return originalPath, nil
	case OpenAI:
		return "/v1/chat/completions", nil
	case OpenAIResponses, Codex:
		return "/v1/responses", nil
	case Claude:
		return "/v1/messages", nil
	default:
		return "", fmt.Errorf("%w: target format %q", ErrUnsupported, format)
	}
}

// ConfigureRequestHeaders removes source-protocol headers after translation
// and adds headers required by the target protocol.
func ConfigureRequestHeaders(header http.Header, target Format, translated bool) {
	if target == Passthrough {
		return
	}
	if translated {
		header.Del("Content-Encoding")
		header.Del("Content-Length")
		header.Del("Accept-Encoding")
		header.Set("Content-Type", "application/json")
	}
	if target == Claude {
		if header.Get("anthropic-version") == "" {
			header.Set("anthropic-version", "2023-06-01")
		}
		return
	}
	header.Del("anthropic-version")
	header.Del("anthropic-beta")
}

// ErrorResponse converts an upstream error envelope to the client's protocol.
func ErrorResponse(source Format, status int, body []byte) []byte {
	if source == Passthrough {
		return append([]byte(nil), body...)
	}
	details := parseErrorDetails(body)
	if details.Message == "" {
		details.Message = http.StatusText(status)
	}
	if details.Type == "" {
		details.Type = errorTypeForStatus(status)
	}

	var response any
	if source == Claude {
		response = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    details.Type,
				"message": details.Message,
			},
		}
	} else {
		response = map[string]any{
			"error": map[string]any{
				"message": details.Message,
				"type":    details.Type,
				"param":   nil,
				"code":    details.Code,
			},
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return append([]byte(nil), body...)
	}
	return encoded
}

type errorDetails struct {
	Message string
	Type    string
	Code    any
}

func parseErrorDetails(body []byte) errorDetails {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    any             `json:"code"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return errorDetails{Message: strings.TrimSpace(string(body))}
	}
	details := errorDetails{Message: envelope.Message, Type: envelope.Type, Code: envelope.Code}
	if len(envelope.Error) == 0 || string(envelope.Error) == "null" {
		return details
	}
	var message string
	if json.Unmarshal(envelope.Error, &message) == nil {
		details.Message = message
		return details
	}
	var nested struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	if json.Unmarshal(envelope.Error, &nested) == nil {
		if nested.Message != "" {
			details.Message = nested.Message
		}
		if nested.Type != "" {
			details.Type = nested.Type
		}
		if nested.Code != nil {
			details.Code = nested.Code
		}
	}
	return details
}

func errorTypeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// Exchange owns the translated request and the state used to translate one response stream.
type Exchange struct {
	Source          Format
	Target          Format
	Model           string
	Stream          bool
	UpstreamStream  bool
	OriginalRequest []byte
	UpstreamRequest []byte

	registry *sdktranslator.Registry
	state    any
}

func NewExchange(source, target Format, model string, stream bool, original []byte) (exchange *Exchange, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			exchange = nil
			err = fmt.Errorf("translate request %s -> %s: %v", source, target, recovered)
		}
	}()
	if source == "" {
		return nil, fmt.Errorf("%w: source format is empty", ErrUnsupported)
	}
	if target == "" {
		target = Passthrough
	}
	exchange = &Exchange{
		Source:          source,
		Target:          target,
		Model:           model,
		Stream:          stream,
		OriginalRequest: append([]byte(nil), original...),
		registry:        muxbuiltin.Registry(),
	}
	exchange.UpstreamStream = stream || (exchange.Translated() && (target == Claude || target == Codex))
	if !exchange.Translated() {
		exchange.UpstreamRequest = append([]byte(nil), original...)
		return exchange, nil
	}
	from, to := exchange.sdkFormats()
	if !exchange.registry.HasRequestTransformer(from, to) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrUnsupported, source, target)
	}
	if stream {
		if !exchange.registry.HasStreamResponseTransformer(from, to) {
			return nil, fmt.Errorf("%w: streaming response %s <- %s", ErrUnsupported, source, target)
		}
	} else if !exchange.registry.HasNonStreamResponseTransformer(from, to) {
		return nil, fmt.Errorf("%w: response %s <- %s", ErrUnsupported, source, target)
	}
	translated := exchange.registry.TranslateRequest(from, to, model, original, exchange.UpstreamStream)
	if !json.Valid(translated) {
		return nil, fmt.Errorf("translate request %s -> %s: invalid JSON", source, target)
	}
	exchange.UpstreamRequest = translated
	return exchange, nil
}

func (e *Exchange) Translated() bool {
	return e != nil && e.Target != Passthrough && e.Source != e.Target
}

func (e *Exchange) TranslateNonStream(ctx context.Context, body []byte) (out []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = nil
			err = fmt.Errorf("translate response %s <- %s: %v", e.Source, e.Target, recovered)
		}
	}()
	if e == nil || !e.Translated() {
		return append([]byte(nil), body...), nil
	}
	from, to := e.sdkFormats()
	out = e.registry.TranslateNonStream(ctx, to, from, e.Model, e.OriginalRequest, e.UpstreamRequest, body, &e.state)
	if !json.Valid(out) {
		return nil, fmt.Errorf("translate response %s <- %s: invalid JSON", e.Source, e.Target)
	}
	return out, nil
}

// TranslateStream accepts one upstream SSE line and may emit zero or more client SSE events.
func (e *Exchange) TranslateStream(ctx context.Context, line []byte) (outputs [][]byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			outputs = nil
			err = fmt.Errorf("translate stream %s <- %s: %v", e.Source, e.Target, recovered)
		}
	}()
	if e == nil || !e.Translated() {
		return [][]byte{append([]byte(nil), line...)}, nil
	}
	from, to := e.sdkFormats()
	outputs = e.registry.TranslateStream(ctx, to, from, e.Model, e.OriginalRequest, e.UpstreamRequest, line, &e.state)
	return outputs, nil
}

func (e *Exchange) sdkFormats() (sdktranslator.Format, sdktranslator.Format) {
	return sdktranslator.FromString(string(e.Source)), sdktranslator.FromString(string(e.Target))
}
