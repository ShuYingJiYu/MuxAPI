package modelmapping

import "testing"

func TestDeriveFallback(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Date suffix removal
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-sonnet-4-20250514", "claude-sonnet-4"},
		{"gpt-4o-20240806", "gpt-4o"},
		// Hyphenated dates don't match (not 8 consecutive digits)
		{"gpt-4o-2024-08-06", ""},
		// Thinking suffix removal
		{"claude-opus-4-6-thinking", "claude-opus-4-6"},
		{"o1-thinking", "o1"},
		// Latest suffix removal
		{"gpt-4-latest", "gpt-4"},
		{"claude-sonnet-latest", "claude-sonnet"},
		// No derivable fallback
		{"gpt-4o", ""},
		{"claude-haiku-4-5", ""},
		{"o1", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := deriveFallback(tt.input)
		if got != tt.want {
			t.Errorf("deriveFallback(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsAllDigits(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"20251001", true},
		{"12345678", true},
		{"1234567a", false},
		{"", false},
		{"abcdefgh", false},
	}
	for _, tt := range tests {
		got := isAllDigits(tt.input)
		if got != tt.want {
			t.Errorf("isAllDigits(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
