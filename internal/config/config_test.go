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

// TestStaleEnvTokenDoesNotShadowSettingsProvider is the regression guard for the
// real UAT failure: settings.json configures a full provider (BASE_URL +
// AUTH_TOKEN), but the shell exports a *different*, stale ANTHROPIC_AUTH_TOKEN
// (same var name) for some other service, with no BASE_URL in env. Plain
// env-first precedence picked the stale env token and 401'd against the proxy.
// Endpoint-aware resolution must keep the settings.json token with its endpoint.
func TestStaleEnvTokenDoesNotShadowSettingsProvider(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := `{"env":{
		"ANTHROPIC_BASE_URL":"https://worldbase.example",
		"ANTHROPIC_AUTH_TOKEN":"sk-good-proxy-token"
	}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	// No BASE_URL in env; a stale token from another service is exported.
	os.Unsetenv("ANTHROPIC_BASE_URL")
	os.Unsetenv("ANTHROPIC_API_KEY")
	os.Unsetenv("ANTHROPIC_MODEL")
	os.Unsetenv("ANTHROPIC_SMALL_FAST_MODEL")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "1a1322-stale-foreign-token-3kOn")

	c := Load()
	if c.BaseURL != "https://worldbase.example" {
		t.Errorf("BaseURL = %q, want proxy from settings", c.BaseURL)
	}
	if c.AuthToken != "sk-good-proxy-token" {
		t.Errorf("AuthToken = %q, want the settings.json token (its endpoint's source)", c.AuthToken)
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

// TestStrayEnvAPIKeyDoesNotShadowProxyToken is the regression guard for the UAT
// bug: settings.json configures a proxy (BASE_URL + AUTH_TOKEN), but the shell
// has a leftover ANTHROPIC_API_KEY from another tool. That key doesn't belong to
// the proxy and caused a 401. The proxy's Bearer token (same source as the base
// URL) must win, and the stray key must be dropped.
func TestStrayEnvAPIKeyDoesNotShadowProxyToken(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := `{"env":{
		"ANTHROPIC_BASE_URL":"https://proxy.example",
		"ANTHROPIC_AUTH_TOKEN":"sk-proxy-token"
	}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	os.Unsetenv("ANTHROPIC_BASE_URL")
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	os.Unsetenv("ANTHROPIC_MODEL")
	os.Unsetenv("ANTHROPIC_SMALL_FAST_MODEL")
	// The trap: a stray key in the env from some other tool.
	t.Setenv("ANTHROPIC_API_KEY", "sk-stray-foreign-key")

	c := Load()
	if c.BaseURL != "https://proxy.example" {
		t.Errorf("BaseURL = %q, want proxy from settings", c.BaseURL)
	}
	if c.AuthToken != "sk-proxy-token" {
		t.Errorf("AuthToken = %q, want the proxy's Bearer token", c.AuthToken)
	}
	if c.APIKey != "" {
		t.Errorf("APIKey = %q, want it dropped (stray, doesn't match proxy)", c.APIKey)
	}
}

// TestEnvKeyMatchingEnvBaseURLWins: when both base URL and API key come from the
// env (a coherent first-party/proxy setup) and a token lingers in settings.json,
// the env key should win because it matches the endpoint's source.
func TestEnvKeyMatchingEnvBaseURLWins(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	os.MkdirAll(claudeDir, 0o700)
	os.WriteFile(filepath.Join(claudeDir, "settings.json"),
		[]byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-old-settings-token"}}`), 0o600)
	t.Setenv("HOME", dir)
	os.Unsetenv("ANTHROPIC_AUTH_TOKEN")
	os.Unsetenv("ANTHROPIC_MODEL")
	os.Unsetenv("ANTHROPIC_SMALL_FAST_MODEL")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
	t.Setenv("ANTHROPIC_API_KEY", "sk-env-key")

	c := Load()
	if c.APIKey != "sk-env-key" {
		t.Errorf("APIKey = %q, want env key (matches env base URL)", c.APIKey)
	}
	if c.AuthToken != "" {
		t.Errorf("AuthToken = %q, want dropped", c.AuthToken)
	}
}
