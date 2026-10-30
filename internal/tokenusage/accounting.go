// Package tokenusage keeps reported usage separate from independently estimated input.
package tokenusage

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const Version = 1

type Estimate struct {
	Tokens   int64  `json:"tokens"`
	Model    string `json:"model"`
	Protocol string `json:"protocol"`
	Source   string `json:"source"`
	Covered  bool   `json:"covered"`
	Reason   string `json:"reason,omitempty"`
}

type Report struct {
	Present       bool              `json:"present"`
	Complete      bool              `json:"complete"`
	Quality       string            `json:"quality"`
	Protocol      string            `json:"protocol"`
	ResponseModel string            `json:"response_model,omitempty"`
	InputTotal    *int64            `json:"input_total,omitempty"`
	UncachedInput int64             `json:"uncached_input"`
	CacheRead     int64             `json:"cache_read"`
	CacheWrite    int64             `json:"cache_write"`
	Output        int64             `json:"output"`
	Normalized    bool              `json:"normalized"`
	Raw           map[string]string `json:"raw,omitempty"`
}

type Measurement struct {
	Version  int      `json:"version"`
	Estimate Estimate `json:"estimate"`
	Report   Report   `json:"report"`
	Status   string   `json:"status"`
	Ratio    *float64 `json:"ratio,omitempty"`
}

func New(estimate Estimate, report Report) *Measurement {
	m := &Measurement{Version: Version, Estimate: estimate, Report: report}
	switch {
	case !report.Present:
		m.Status = "missing_usage"
	case report.Quality == "inconsistent":
		m.Status = "invalid_usage"
	case report.Quality != "valid" || report.InputTotal == nil:
		m.Status = "unclassified_usage"
	case !report.Complete:
		m.Status = "incomplete_usage"
	case !estimate.Covered:
		m.Status = estimate.Reason
		if m.Status == "" {
			m.Status = "incomplete_estimate"
		}
	case estimate.Tokens <= 0:
		m.Status = "empty_estimate"
	case report.ResponseModel != "" && !sameModel(estimate.Model, report.ResponseModel):
		m.Status = "response_model_mismatch"
	default:
		m.Status = "comparable"
		ratio := float64(*report.InputTotal) / float64(estimate.Tokens)
		m.Ratio = &ratio
	}
	return m
}

func sameModel(sent, received string) bool {
	sent, received = strings.ToLower(strings.TrimSpace(sent)), strings.ToLower(strings.TrimSpace(received))
	if sent == received {
		return true
	}
	// A dated snapshot of a named model is compatible with its unsuffixed name.
	if strings.HasPrefix(received, sent+"-20") {
		suffix := strings.TrimPrefix(received, sent+"-")
		_, err := time.Parse("2006-01-02", suffix)
		return err == nil
	}
	return false
}

func Encode(m *Measurement) string {
	if m == nil {
		return ""
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(data)
}

func Decode(raw string) *Measurement {
	var m Measurement
	if json.Unmarshal([]byte(raw), &m) != nil || m.Version != Version {
		return nil
	}
	if m.Estimate.Source == "legacy" && m.Report.Quality == "legacy" {
		m.Status, m.Ratio = "legacy", nil
		return &m
	}
	return New(m.Estimate, m.Report)
}

func Protocol(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "anthropic", "messages", "anthropic-messages":
		return "claude"
	case "openai", "chat", "chat_completions", "chat-completions":
		return "openai"
	case "responses", "codex", "openai-response", "openai-responses", "openai_responses":
		return "responses"
	case "gemini", "google", "generativelanguage", "generatecontent":
		return "gemini"
	default:
		return ""
	}
}

// Parse accepts only token fields. Arbitrary upstream usage metadata is not persisted.
func Parse(usage map[string]any, protocol string) Report {
	r := Report{Protocol: Protocol(protocol), Quality: "valid", Raw: map[string]string{}}
	read := func(paths ...string) *int64 {
		var chosen *int64
		for _, path := range paths {
			var current any = usage
			found := true
			for _, key := range strings.Split(path, ".") {
				object, ok := current.(map[string]any)
				if !ok {
					found = false
					break
				}
				current, ok = object[key]
				if !ok {
					found = false
					break
				}
			}
			if !found {
				continue
			}
			r.Present = true
			text := "[invalid]"
			switch current.(type) {
			case json.Number, float64, float32, int, int64, int32, uint, uint64, uint32:
				text = fmt.Sprint(current)
				if len(text) > 32 {
					text = "[invalid]"
				}
			}
			r.Raw[path] = text
			value, err := strconv.ParseInt(text, 10, 64)
			if err != nil || value < 0 {
				r.Quality = "inconsistent"
				continue
			}
			if chosen != nil && *chosen != value {
				r.Quality = "inconsistent"
			}
			if chosen == nil {
				chosen = &value
			}
		}
		return chosen
	}
	for _, key := range []string{"input_tokens_details", "prompt_tokens_details"} {
		if value := usage[key]; value != nil {
			if _, ok := value.(map[string]any); !ok {
				r.Present, r.Quality = true, "inconsistent"
				r.Raw[key] = "[invalid]"
			}
		}
	}
	input := read("input_tokens", "prompt_tokens", "promptTokenCount")
	output := read("output_tokens", "completion_tokens", "candidatesTokenCount")
	cached := read("cached_tokens", "cache_read_input_tokens", "cachedContentTokenCount", "input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens")
	write := read("cache_creation_tokens", "cache_creation_input_tokens", "input_tokens_details.cache_creation_tokens", "input_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_creation_tokens", "prompt_tokens_details.cache_write_tokens")
	thoughts := read("thoughtsTokenCount")
	total := read("total_tokens", "totalTokenCount")
	if r.Protocol == "" {
		switch {
		case usage["promptTokenCount"] != nil:
			r.Protocol = "gemini"
		case usage["cache_read_input_tokens"] != nil || usage["cache_creation_input_tokens"] != nil:
			r.Protocol = "claude"
		case usage["prompt_tokens"] != nil || usage["prompt_tokens_details"] != nil:
			r.Protocol = "openai"
		case usage["input_tokens_details"] != nil:
			r.Protocol = "responses"
		}
	}
	if cached != nil {
		r.CacheRead = *cached
	}
	if write != nil {
		r.CacheWrite = *write
	}
	if output != nil {
		r.Output = *output
	}
	if thoughts != nil && r.Protocol == "gemini" {
		if sum, ok := add(r.Output, *thoughts); ok {
			r.Output = sum
		} else {
			r.Quality = "inconsistent"
		}
	}
	if input == nil {
		if r.Quality == "valid" {
			r.Quality = "unclassified"
		}
		return r
	}
	r.UncachedInput = *input
	switch r.Protocol {
	case "claude":
		if sum, ok := add(*input, r.CacheRead, r.CacheWrite); ok {
			r.InputTotal = &sum
		} else {
			r.Quality = "inconsistent"
		}
		r.Normalized = true
	case "openai", "responses", "gemini":
		// Nested cache-write details declare subset buckets. Flat write
		// extensions on a non-Claude response do not establish their overlap.
		ambiguousWrite := r.CacheWrite > 0 && usage["cache_creation_input_tokens"] != nil || r.CacheWrite > 0 && usage["cache_creation_tokens"] != nil
		cacheTotal, ok := add(r.CacheRead, r.CacheWrite)
		if !ok || cacheTotal > *input {
			r.Quality = "inconsistent"
		}
		if ambiguousWrite && r.Quality == "valid" {
			r.Quality = "unclassified"
		}
		if ambiguousWrite {
			cacheTotal = r.CacheRead
		}
		r.UncachedInput = max(0, *input-cacheTotal)
		r.InputTotal = input
		r.Normalized = true
	default:
		if r.Quality == "valid" {
			r.Quality = "unclassified"
		}
	}
	if total != nil && output != nil && r.InputTotal != nil && r.Quality == "valid" {
		if expected, ok := add(*r.InputTotal, r.Output); !ok || expected != *total {
			r.Quality = "inconsistent"
		}
	}
	return r
}

func add(values ...int64) (int64, bool) {
	var sum int64
	for _, value := range values {
		if value < 0 || value > math.MaxInt64-sum {
			return 0, false
		}
		sum += value
	}
	return sum, true
}

// Legacy never invents an outbound estimate or a completed usage snapshot.
func Legacy(input, cached, write int64, normalized bool, protocol string) *Measurement {
	r := Report{Quality: "legacy", Protocol: Protocol(protocol), Present: input > 0 || cached > 0 || write > 0, UncachedInput: input, CacheRead: cached, CacheWrite: write}
	if normalized || r.Protocol == "claude" {
		r.Normalized = true
		if sum, ok := add(input, cached, write); ok {
			r.InputTotal = &sum
		}
	} else if r.Protocol != "" && input >= 0 {
		r.InputTotal = &input
		if write == 0 && cached >= 0 && cached <= input {
			r.UncachedInput = input - cached
			r.Normalized = true
		}
	}
	return &Measurement{Version: Version, Report: r, Estimate: Estimate{Source: "legacy"}, Status: "legacy"}
}
