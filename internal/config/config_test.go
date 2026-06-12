package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvPrecedence(t *testing.T) {
	// Point HOME at a temp dir with a settings.json that sets a base URL and a
	// token, then verify env overrides settings and missing keys fall back.
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := `{"env":{
		"ANTHROPIC_BASE_URL":"https://settings.example",
		"ANTHROPIC_AUTH_TOKEN":"tok-from-settings",
		"ANTHROPIC_SMALL_FAST_MODEL":"fast-from-settings"
	}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	// env overrides the base URL but not the token/model.
	t.Setenv("ANTHROPIC_BASE_URL", "https://env.example")
	// Ensure these are unset so settings.json wins.
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("ANTHROPIC_SMALL_FAST_MODEL")
	os.Unsetenv("ANTHROPIC_MODEL")

	c := Load()
	if c.BaseURL != "https://env.example" {
		t.Errorf("BaseURL = %q, want env override", c.BaseURL)
	}
	if c.AuthToken != "tok-from-settings" {
		t.Errorf("AuthToken = %q, want settings fallback", c.AuthToken)
	}
	if c.FastModel != "fast-from-settings" {
		t.Errorf("FastModel = %q, want settings fallback", c.FastModel)
	}
	if !c.Enabled() {
		t.Error("Enabled() = false, want true (token present)")
	}
}

func TestLoadDefaultsAndDisabled(t *testing.T) {
	dir := t.TempDir() // no settings.json inside
	t.Setenv("HOME", dir)
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL"} {
		os.Unsetenv(k)
	}
	c := Load()
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want default %q", c.BaseURL, DefaultBaseURL)
	}
	if c.Enabled() {
		t.Error("Enabled() = true with no credentials, want false")
	}
}

func TestLoadAPIKeyAuth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL"} {
		os.Unsetenv(k)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-key")
	c := Load()
	if c.APIKey != "sk-ant-key" {
		t.Errorf("APIKey = %q", c.APIKey)
	}
	if c.AuthToken != "" {
		t.Errorf("AuthToken = %q, want empty", c.AuthToken)
	}
	if !c.Enabled() {
		t.Error("Enabled() = false with API key, want true")
	}
}
