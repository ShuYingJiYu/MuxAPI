package routing

import (
	"net/http"
	"strings"
	"testing"
)

func TestExtractClaudeFeaturesPreservesSessionScope(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"system":"You are a coding assistant.",
		"messages":[
			{"role":"user","content":"Inspect this repository and explain the scheduler."},
			{"role":"assistant","content":"I found a priority scheduler."},
			{"role":"user","content":"Now optimize it."}
		],
		"max_tokens":800,
		"stream":true
	}`)
	features, err := ExtractRequestFeatures(body, FeatureOptions{
		Protocol: "anthropic",
		Headers:  http.Header{"X-Claude-Code-Session-Id": []string{"session-42"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if features.Protocol != "claude" || features.Model != "claude-sonnet-4-5" {
		t.Fatalf("unexpected protocol/model: %+v", features)
	}
	if features.SessionID != "session-42" {
		t.Fatalf("session ID = %q", features.SessionID)
	}
	if features.InputTokens <= 0 || features.ReusableInputTokens <= 0 || features.ReusableInputTokens >= features.InputTokens {
		t.Fatalf("bad token estimates: %+v", features)
	}
	if features.EstimatedOutputTokens != 200 || !features.Stream {
		t.Fatalf("bad output/stream inference: %+v", features)
	}
}

func TestExtractGeminiFeatures(t *testing.T) {
	body := []byte(`{
		"contents":[
			{"role":"user","parts":[{"text":"Write a Go function."}]},
			{"role":"model","parts":[{"text":"Here is the function."}]},
			{"role":"user","parts":[{"text":"Add tests."}]}
		],
		"generationConfig":{"maxOutputTokens":400}
	}`)
	features, err := ExtractRequestFeatures(body, FeatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if features.Protocol != "gemini" || features.InputTokens <= 0 || features.ReusableInputTokens <= 0 {
		t.Fatalf("Gemini request was not extracted: %+v", features)
	}
	if features.MaxOutputTokens != 400 || features.EstimatedOutputTokens != 100 {
		t.Fatalf("Gemini generation config was not extracted: %+v", features)
	}
	if !strings.HasPrefix(features.CacheKey, "mux_") || len(features.CacheKey) != 68 {
		t.Fatalf("invalid derived cache key %q", features.CacheKey)
	}
}

func TestDerivedCacheKeyTracksStablePrefix(t *testing.T) {
	first := []byte(`{"model":"gpt-5","messages":[{"role":"system","content":"same rules"},{"role":"user","content":"first turn"}]}`)
	second := []byte(`{"model":"gpt-5","messages":[{"role":"system","content":"same rules"},{"role":"user","content":"different turn"}]}`)
	a, err := ExtractRequestFeatures(first, FeatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ExtractRequestFeatures(second, FeatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.CacheKey != b.CacheKey {
		t.Fatalf("same reusable prefix should share key: %s != %s", a.CacheKey, b.CacheKey)
	}
	if a.ReusableInputTokens <= 0 || b.ReusableInputTokens <= 0 {
		t.Fatalf("stable prefix not detected: %+v / %+v", a, b)
	}
}

func TestSingleTurnDerivedKeysDoNotAlias(t *testing.T) {
	a, _ := ExtractRequestFeatures([]byte(`{"messages":[{"role":"user","content":"alpha"}]}`), FeatureOptions{})
	b, _ := ExtractRequestFeatures([]byte(`{"messages":[{"role":"user","content":"beta"}]}`), FeatureOptions{})
	if a.CacheKey == b.CacheKey {
		t.Fatalf("unrelated one-shot requests share cache key %q", a.CacheKey)
	}
	if a.ReusableInputTokens != 0 || b.ReusableInputTokens != 0 {
		t.Fatalf("one-shot requests should not predict a reusable prefix: %+v / %+v", a, b)
	}
}
