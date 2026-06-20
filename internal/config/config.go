// Package config resolves the LLM provider configuration for flowd. flow speaks
// the Anthropic Messages wire protocol, but the endpoint behind it may be the
// first-party API or any Anthropic-compatible proxy (GLM, DeepSeek, a gateway
// like worldbase.ai, etc.). The provider is described by: a base URL, an auth
// credential (Bearer auth token OR x-api-key), and model names.
//
// Resolution precedence (first non-empty wins):
//  1. process environment (ad-hoc overrides)
//  2. ~/.flow/settings.json — structured config supporting both Anthropic and
//     OpenAI-compatible providers (highest-priority config file)
//  3. the "env" block of ~/.claude/settings.json (fallback, Claude Code compat)
//
// Recognized env keys (env var name == ~/.claude/settings.json env key):
//
//	ANTHROPIC_BASE_URL          endpoint root (default https://api.anthropic.com)
//	ANTHROPIC_AUTH_TOKEN        Bearer token (compatible proxies); takes auth precedence
//	ANTHROPIC_API_KEY           x-api-key (first-party API)
//	ANTHROPIC_MODEL             model for mode B (capable)
//	ANTHROPIC_SMALL_FAST_MODEL  model for mode A (fast translation)
//
// ~/.flow/settings.json uses a flat structured format:
//
//	{
//	  "provider":   "anthropic",     // or "openai"
//	  "base_url":   "https://...",
//	  "api_key":    "sk-...",
//	  "auth_token": "...",
//	  "model":      "...",
//	  "fast_model": "..."
//	}
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Provider types supported by ~/.flow/settings.json.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// Config is the resolved provider configuration.
type Config struct {
	Provider  string // ProviderAnthropic (default) or ProviderOpenAI
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

// Load resolves the configuration. It tries ~/.flow/settings.json first
// (structured, supports both Anthropic and OpenAI-compatible providers), then
// falls back to the process environment + ~/.claude/settings.json env block.
func Load() *Config {
	// Highest-priority config file: ~/.flow/settings.json
	if flow := loadFlowSettings(); flow != nil && (flow.APIKey != "" || flow.AuthToken != "") {
		return loadFromFlow(flow)
	}
	// Fallback: env + ~/.claude/settings.json
	return loadFromClaude()
}

// loadFromFlow builds a Config from ~/.flow/settings.json, with env overrides
// for individual fields (ad-hoc testing/debugging without editing the file).
func loadFromFlow(flow *flowSettings) *Config {
	c := &Config{
		Provider:  flow.Provider,
		BaseURL:   flow.BaseURL,
		AuthToken: flow.AuthToken,
		APIKey:    flow.APIKey,
		Model:     flow.Model,
		FastModel: flow.FastModel,
		Source:    "~/.flow/settings.json",
	}
	if c.Provider == "" {
		c.Provider = ProviderAnthropic
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	// Env overrides for individual fields.
	if v := os.Getenv(envBaseURL); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv(envAuthToken); v != "" {
		c.AuthToken = v
	}
	if v := os.Getenv(envAPIKey); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv(envModel); v != "" {
		c.Model = v
	}
	if v := os.Getenv(envFastModel); v != "" {
		c.FastModel = v
	}
	// Bearer token takes precedence over API key when both are present.
	if c.AuthToken != "" && c.APIKey != "" {
		c.APIKey = ""
	}
	switch {
	case c.AuthToken != "":
		c.Source = "auth_token (~/.flow/settings.json)"
	case c.APIKey != "":
		c.Source = "api_key (~/.flow/settings.json)"
	}
	return c
}

// loadFromClaude is the legacy resolution path: env-first, then
// ~/.claude/settings.json's env block.
func loadFromClaude() *Config {
	settings := loadSettingsEnv()

	// getFrom returns a key's value from a specific source ("env" or
	// "settings.json"), or "" if absent there.
	getFrom := func(key, src string) string {
		switch src {
		case "env":
			return os.Getenv(key)
		case "settings.json":
			return settings[key]
		}
		return ""
	}
	// get resolves a key with env-first precedence, returning its source.
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
	var baseSrc string
	c.BaseURL, baseSrc = get(envBaseURL)
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
		baseSrc = "default"
	}

	c.Model, _ = get(envModel)
	c.FastModel, _ = get(envFastModel)

	// Auth resolution is endpoint-aware: a credential belongs to the endpoint
	// from the SAME source. When the base URL comes from a real source (env or
	// settings.json — i.e. an explicitly chosen provider), look for the auth
	// credential in that source first, and only fall back to the other source if
	// that source has none.
	//
	// This avoids the trap that bit a real user: ANTHROPIC_BASE_URL +
	// ANTHROPIC_AUTH_TOKEN configured together in ~/.claude/settings.json for a
	// proxy, but a *different*, stale ANTHROPIC_AUTH_TOKEN exported in the shell
	// for some other service. Plain env-first precedence would pick the stale
	// env token and 401 against the proxy. Endpoint-aware resolution keeps the
	// token with its endpoint.
	var tokSrc, keySrc string
	if baseSrc == "env" || baseSrc == "settings.json" {
		// Prefer the base URL's own source for the credential.
		if v := getFrom(envAuthToken, baseSrc); v != "" {
			c.AuthToken, tokSrc = v, baseSrc
		} else if v := getFrom(envAPIKey, baseSrc); v != "" {
			c.APIKey, keySrc = v, baseSrc
		} else {
			// That source has no credential; fall back to normal resolution.
			c.AuthToken, tokSrc = get(envAuthToken)
			c.APIKey, keySrc = get(envAPIKey)
		}
	} else {
		// Default endpoint: normal env-first resolution.
		c.AuthToken, tokSrc = get(envAuthToken)
		c.APIKey, keySrc = get(envAPIKey)
	}

	// AUTH_TOKEN (Bearer) and API_KEY (x-api-key) are mutually exclusive on the
	// wire. If both still resolved, prefer the one matching the base URL's
	// source, else Bearer; null out the loser so exactly one header is sent.
	if c.AuthToken != "" && c.APIKey != "" {
		switch {
		case baseSrc != "default" && keySrc == baseSrc && tokSrc != baseSrc:
			c.AuthToken = ""
		case baseSrc != "default" && tokSrc == baseSrc && keySrc != baseSrc:
			c.APIKey = ""
		default:
			c.APIKey = ""
		}
	}

	c.Provider = ProviderAnthropic
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

// flowSettings is the structured config from ~/.flow/settings.json.
type flowSettings struct {
	Provider  string `json:"provider"`   // "anthropic" (default) or "openai"
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	AuthToken string `json:"auth_token"`
	Model     string `json:"model"`
	FastModel string `json:"fast_model"`
}

// loadFlowSettings reads ~/.flow/settings.json. Returns nil if the file is
// absent or unparseable — config resolution degrades gracefully to the
// ~/.claude/settings.json fallback.
func loadFlowSettings() *flowSettings {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := filepath.Join(home, ".flow", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s flowSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil
	}
	return &s
}
