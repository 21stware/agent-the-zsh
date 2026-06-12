// Package config resolves the LLM provider configuration for flowd. flow speaks
// the Anthropic Messages wire protocol, but the endpoint behind it may be the
// first-party API or any Anthropic-compatible proxy (GLM, DeepSeek, a gateway
// like worldbase.ai, etc.). The provider is described by: a base URL, an auth
// credential (Bearer auth token OR x-api-key), and model names.
//
// Resolution precedence (first non-empty wins), matching Claude Code's own env
// convention so an existing ~/.claude/settings.json just works:
//  1. process environment
//  2. the "env" block of ~/.claude/settings.json
//
// Recognized keys (env var name == settings.json env key):
//
//	ANTHROPIC_BASE_URL          endpoint root (default https://api.anthropic.com)
//	ANTHROPIC_AUTH_TOKEN        Bearer token (compatible proxies); takes auth precedence
//	ANTHROPIC_API_KEY           x-api-key (first-party API)
//	ANTHROPIC_MODEL             model for mode B (capable)
//	ANTHROPIC_SMALL_FAST_MODEL  model for mode A (fast translation)
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the resolved provider configuration.
type Config struct {
	BaseURL   string
	AuthToken string // Authorization: Bearer <token>; preferred when set
	APIKey    string // x-api-key: <key>; first-party API
	Model     string // mode B / capable
	FastModel string // mode A / fast translation
	// Source notes where the credential came from, for non-secret logging.
	Source string
}

// Enabled reports whether a usable credential is present.
func (c *Config) Enabled() bool {
	return c.AuthToken != "" || c.APIKey != ""
}

const (
	envBaseURL   = "ANTHROPIC_BASE_URL"
	envAuthToken = "ANTHROPIC_AUTH_TOKEN"
	envAPIKey    = "ANTHROPIC_API_KEY"
	envModel     = "ANTHROPIC_MODEL"
	envFastModel = "ANTHROPIC_SMALL_FAST_MODEL"

	// DefaultBaseURL is the first-party API endpoint.
	DefaultBaseURL = "https://api.anthropic.com"
)

// Load resolves the configuration from the process environment, falling back to
// ~/.claude/settings.json's env block for any key not set in the environment.
func Load() *Config {
	settings := loadSettingsEnv()

	get := func(key string) (val, src string) {
		if v := os.Getenv(key); v != "" {
			return v, "env"
		}
		if v, ok := settings[key]; ok && v != "" {
			return v, "settings.json"
		}
		return "", ""
	}

	c := &Config{}
	c.BaseURL, _ = get(envBaseURL)
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	var tokSrc, keySrc string
	c.AuthToken, tokSrc = get(envAuthToken)
	c.APIKey, keySrc = get(envAPIKey)
	c.Model, _ = get(envModel)
	c.FastModel, _ = get(envFastModel)

	switch {
	case c.AuthToken != "":
		c.Source = "ANTHROPIC_AUTH_TOKEN (" + tokSrc + ")"
	case c.APIKey != "":
		c.Source = "ANTHROPIC_API_KEY (" + keySrc + ")"
	}
	return c
}

// loadSettingsEnv reads the "env" map from ~/.claude/settings.json. Returns an
// empty map if the file is absent or unparseable — config resolution must never
// fail hard (the daemon degrades gracefully when no provider is configured).
func loadSettingsEnv() map[string]string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	return doc.Env
}
