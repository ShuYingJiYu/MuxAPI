package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvAllowEmpty(t *testing.T) {
	t.Setenv("MUXAPI_TEST_PRESENT", "")
	if got := envAllowEmpty("MUXAPI_TEST_PRESENT", "fallback"); got != "" {
		t.Fatalf("present empty environment value = %q, want empty", got)
	}
	t.Setenv("MUXAPI_TEST_PRESENT", "configured")
	if got := envAllowEmpty("MUXAPI_TEST_PRESENT", "fallback"); got != "configured" {
		t.Fatalf("configured environment value = %q, want configured", got)
	}
	t.Setenv("MUXAPI_TEST_PRESENT", "")
	if got := envAllowEmpty("MUXAPI_TEST_ABSENT", "fallback"); got != "fallback" {
		t.Fatalf("absent environment value = %q, want fallback", got)
	}
}

func TestLoadDotEnvDoesNotOverrideExplicitEmpty(t *testing.T) {
	const key = "MUXAPI_TEST_DOTENV_EMPTY"
	t.Setenv(key, "")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(key+"=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loadDotEnv(path)
	if got := os.Getenv(key); got != "" {
		t.Fatalf("explicit empty environment value = %q, want empty", got)
	}
}
