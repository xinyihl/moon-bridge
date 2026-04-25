package config

import (
	"errors"
	"fmt"
	"strings"
)

const (
	DefaultConfigPath = "config.yml"
	DefaultAddr       = "127.0.0.1:38440"
)

type Mode string

const (
	ModeCaptureAnthropic Mode = "CaptureAnthropic"
	ModeCaptureResponse  Mode = "CaptureResponse"
	ModeTransform        Mode = "Transform"
)

type WebSearchSupport string

const (
	WebSearchSupportAuto     WebSearchSupport = "auto"
	WebSearchSupportEnabled  WebSearchSupport = "enabled"
	WebSearchSupportDisabled WebSearchSupport = "disabled"
)

type Config struct {
	Mode              Mode
	Addr              string
	TraceRequests     bool
	LogLevel          string
	LogFormat         string
	SystemPrompt      string
	DefaultModel      string
	ProviderBaseURL   string
	ProviderAPIKey    string
	ProviderVersion   string
	ProviderUserAgent string
	WebSearchSupport  WebSearchSupport
	WebSearchMaxUses  int
	DefaultMaxTokens  int
	ModelMap          map[string]string
	ProviderModels    map[string]ProviderModelConfig
	Cache             CacheConfig
	ResponseProxy     ResponseProxyConfig
	AnthropicProxy    AnthropicProxyConfig
}

type ResponseProxyConfig struct {
	Model           string
	ProviderBaseURL string
	ProviderAPIKey  string
}

type AnthropicProxyConfig struct {
	Model           string
	ProviderBaseURL string
	ProviderAPIKey  string
	ProviderVersion string
}

type CacheConfig struct {
	Mode                     string
	TTL                      string
	PromptCaching            bool
	AutomaticPromptCache     bool
	ExplicitCacheBreakpoints bool
	AllowRetentionDowngrade  bool
	MaxBreakpoints           int
	MinCacheTokens           int
	ExpectedReuse            int
	MinimumValueScore        int
}

type ProviderModelConfig struct {
	Name            string
	ContextWindow   int
	MaxOutputTokens int
}

func (cfg Config) Validate() error {
	if err := cfg.Cache.Validate(); err != nil {
		return err
	}
	switch cfg.Mode {
	case ModeTransform:
		return cfg.validateTransform()
	case ModeCaptureResponse:
		return cfg.ResponseProxy.Validate("developer.proxy.response")
	case ModeCaptureAnthropic:
		return cfg.AnthropicProxy.Validate("developer.proxy.anthropic")
	default:
		return fmt.Errorf("invalid mode %q", cfg.Mode)
	}
}

func (cfg Config) validateTransform() error {
	if cfg.ProviderBaseURL == "" {
		return errors.New("provider.base_url is required")
	}
	if cfg.ProviderAPIKey == "" {
		return errors.New("provider.api_key is required")
	}
	if len(cfg.ModelMap) == 0 {
		return errors.New("provider.models must contain at least one model mapping")
	}
	for alias, model := range cfg.ModelMap {
		if alias == "" || model == "" {
			return errors.New("provider.models cannot contain empty aliases or models")
		}
	}
	return nil
}

func (cfg ResponseProxyConfig) Validate(prefix string) error {
	if cfg.ProviderBaseURL == "" {
		return fmt.Errorf("%s.provider.base_url is required", prefix)
	}
	if cfg.ProviderAPIKey == "" {
		return fmt.Errorf("%s.provider.api_key is required", prefix)
	}
	return nil
}

func (cfg AnthropicProxyConfig) Validate(prefix string) error {
	if cfg.ProviderBaseURL == "" {
		return fmt.Errorf("%s.provider.base_url is required", prefix)
	}
	if cfg.ProviderAPIKey == "" {
		return fmt.Errorf("%s.provider.api_key is required", prefix)
	}
	return nil
}

func (cfg Config) ModelFor(model string) string {
	if cfg.ModelMap == nil {
		return model
	}
	if mapped, ok := cfg.ModelMap[model]; ok && mapped != "" {
		return mapped
	}
	return model
}

func (cfg Config) ProviderModelFor(model string) ProviderModelConfig {
	if cfg.ProviderModels == nil {
		return ProviderModelConfig{}
	}
	return cfg.ProviderModels[model]
}

func (cfg Config) DefaultModelAlias() string {
	if cfg.DefaultModel != "" {
		return cfg.DefaultModel
	}
	if _, ok := cfg.ProviderModels["moonbridge"]; ok {
		return "moonbridge"
	}
	if len(cfg.ProviderModels) == 1 {
		for alias := range cfg.ProviderModels {
			return alias
		}
	}
	return ""
}

func (cfg Config) CodexModel() string {
	if cfg.Mode == ModeCaptureResponse && cfg.ResponseProxy.Model != "" {
		return cfg.ResponseProxy.Model
	}
	return cfg.DefaultModelAlias()
}

func (cfg Config) WebSearchEnabled() bool {
	return cfg.WebSearchSupport != WebSearchSupportDisabled
}

func (cfg Config) WebSearchProbeModel() string {
	alias := cfg.DefaultModelAlias()
	if alias == "" {
		return ""
	}
	return cfg.ModelFor(alias)
}

func (cfg *Config) DisableWebSearch() {
	cfg.WebSearchSupport = WebSearchSupportDisabled
}

func (cfg *Config) OverrideAddr(addr string) {
	if addr == "" {
		return
	}
	cfg.Addr = strings.TrimSpace(addr)
}

func (cfg CacheConfig) Validate() error {
	switch cfg.Mode {
	case "", "off", "automatic", "explicit", "hybrid":
	default:
		return fmt.Errorf("invalid cache mode %q", cfg.Mode)
	}
	switch cfg.TTL {
	case "", "5m", "1h":
	default:
		return fmt.Errorf("invalid cache ttl %q", cfg.TTL)
	}
	return nil
}

func valueOrDefault(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func intOrDefault(value int, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func boolOrDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func providerModelMap(models map[string]ProviderModelConfig) map[string]string {
	if len(models) == 0 {
		return nil
	}
	normalized := make(map[string]string, len(models))
	for alias, model := range models {
		normalized[strings.TrimSpace(alias)] = strings.TrimSpace(model.Name)
	}
	return normalized
}
