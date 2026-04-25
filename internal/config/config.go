package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
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

type Config struct {
	Mode              Mode
	Addr              string
	TraceRequests     bool
	DefaultModel      string
	ProviderBaseURL   string
	ProviderAPIKey    string
	ProviderVersion   string
	ProviderUserAgent string
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

type FileConfig struct {
	Mode          string              `yaml:"mode"`
	TraceRequests bool                `yaml:"trace_requests"`
	Server        ServerFileConfig    `yaml:"server"`
	Provider      ProviderFileConfig  `yaml:"provider"`
	Cache         CacheFileConfig     `yaml:"cache"`
	Developer     DeveloperFileConfig `yaml:"developer"`
}

type ServerFileConfig struct {
	Addr string `yaml:"addr"`
}

type ProviderFileConfig struct {
	BaseURL          string                             `yaml:"base_url"`
	APIKey           string                             `yaml:"api_key"`
	Version          string                             `yaml:"version"`
	UserAgent        string                             `yaml:"user_agent"`
	WebSearch        WebSearchFileConfig                `yaml:"web_search"`
	DefaultMaxTokens int                                `yaml:"default_max_tokens"`
	DefaultModel     string                             `yaml:"default_model"`
	Models           map[string]ProviderModelFileConfig `yaml:"models"`
}

type CacheFileConfig struct {
	Mode                     string `yaml:"mode"`
	TTL                      string `yaml:"ttl"`
	PromptCaching            *bool  `yaml:"prompt_caching"`
	AutomaticPromptCache     *bool  `yaml:"automatic_prompt_cache"`
	ExplicitCacheBreakpoints *bool  `yaml:"explicit_cache_breakpoints"`
	AllowRetentionDowngrade  *bool  `yaml:"allow_retention_downgrade"`
	MaxBreakpoints           int    `yaml:"max_breakpoints"`
	MinCacheTokens           int    `yaml:"min_cache_tokens"`
	ExpectedReuse            int    `yaml:"expected_reuse"`
	MinimumValueScore        int    `yaml:"minimum_value_score"`
}

type ProviderModelFileConfig struct {
	Name            string `yaml:"name"`
	ContextWindow   int    `yaml:"context_window"`
	MaxOutputTokens int    `yaml:"max_output_tokens"`
}

type WebSearchFileConfig struct {
	MaxUses int `yaml:"max_uses"`
}

type DeveloperFileConfig struct {
	Proxy DeveloperProxyFileConfig `yaml:"proxy"`
}

type DeveloperProxyFileConfig struct {
	Response  ProxyFileConfig `yaml:"response"`
	Anthropic ProxyFileConfig `yaml:"anthropic"`
}

type ProxyFileConfig struct {
	Model    string                  `yaml:"model"`
	Provider ProxyProviderFileConfig `yaml:"provider"`
}

type ProxyProviderFileConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Version string `yaml:"version"`
}

func LoadFromEnv() (Config, error) {
	path := os.Getenv("MOONBRIDGE_CONFIG")
	if path == "" {
		path = DefaultConfigPath
	}
	return LoadFromFile(path)
}

func LoadFromFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	return LoadFromYAML(data)
}

func LoadFromYAML(data []byte) (Config, error) {
	var fileConfig FileConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fileConfig); err != nil {
		return Config{}, err
	}
	return FromFileConfig(fileConfig)
}

func FromFileConfig(fileConfig FileConfig) (Config, error) {
	mode, err := parseMode(fileConfig.Mode)
	if err != nil {
		return Config{}, err
	}
	providerModels := FromProviderModelFileConfig(fileConfig.Provider.Models)
	cfg := Config{
		Mode:              mode,
		Addr:              valueOrDefault(strings.TrimSpace(fileConfig.Server.Addr), DefaultAddr),
		TraceRequests:     fileConfig.TraceRequests,
		DefaultModel:      strings.TrimSpace(fileConfig.Provider.DefaultModel),
		ProviderBaseURL:   strings.TrimRight(strings.TrimSpace(fileConfig.Provider.BaseURL), "/"),
		ProviderAPIKey:    strings.TrimSpace(fileConfig.Provider.APIKey),
		ProviderVersion:   valueOrDefault(strings.TrimSpace(fileConfig.Provider.Version), "2023-06-01"),
		ProviderUserAgent: strings.TrimSpace(fileConfig.Provider.UserAgent),
		WebSearchMaxUses:  intOrDefault(fileConfig.Provider.WebSearch.MaxUses, 8),
		DefaultMaxTokens:  intOrDefault(fileConfig.Provider.DefaultMaxTokens, 1024),
		ModelMap:          providerModelMap(providerModels),
		ProviderModels:    providerModels,
		Cache:             fromCacheFileConfig(fileConfig.Cache),
		ResponseProxy:     FromResponseProxyFileConfig(fileConfig.Developer.Proxy.Response),
		AnthropicProxy:    FromAnthropicProxyFileConfig(fileConfig.Developer.Proxy.Anthropic),
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseMode(value string) (Mode, error) {
	switch mode := Mode(strings.TrimSpace(value)); mode {
	case ModeCaptureAnthropic, ModeCaptureResponse, ModeTransform:
		return mode, nil
	case "":
		return "", errors.New("mode is required")
	default:
		return "", fmt.Errorf("invalid mode %q", value)
	}
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

func FromResponseProxyFileConfig(fileConfig ProxyFileConfig) ResponseProxyConfig {
	return ResponseProxyConfig{
		Model:           strings.TrimSpace(fileConfig.Model),
		ProviderBaseURL: strings.TrimRight(strings.TrimSpace(fileConfig.Provider.BaseURL), "/"),
		ProviderAPIKey:  strings.TrimSpace(fileConfig.Provider.APIKey),
	}
}

func FromAnthropicProxyFileConfig(fileConfig ProxyFileConfig) AnthropicProxyConfig {
	return AnthropicProxyConfig{
		Model:           strings.TrimSpace(fileConfig.Model),
		ProviderBaseURL: strings.TrimRight(strings.TrimSpace(fileConfig.Provider.BaseURL), "/"),
		ProviderAPIKey:  strings.TrimSpace(fileConfig.Provider.APIKey),
		ProviderVersion: valueOrDefault(strings.TrimSpace(fileConfig.Provider.Version), "2023-06-01"),
	}
}

func FromProviderModelFileConfig(fileConfig map[string]ProviderModelFileConfig) map[string]ProviderModelConfig {
	models := make(map[string]ProviderModelConfig, len(fileConfig))
	for alias, model := range fileConfig {
		trimmedAlias := strings.TrimSpace(alias)
		if trimmedAlias == "" {
			continue
		}
		models[trimmedAlias] = ProviderModelConfig{
			Name:            strings.TrimSpace(model.Name),
			ContextWindow:   model.ContextWindow,
			MaxOutputTokens: model.MaxOutputTokens,
		}
	}
	return models
}

func fromCacheFileConfig(fileConfig CacheFileConfig) CacheConfig {
	return CacheConfig{
		Mode:                     valueOrDefault(strings.TrimSpace(fileConfig.Mode), "automatic"),
		TTL:                      valueOrDefault(strings.TrimSpace(fileConfig.TTL), "5m"),
		PromptCaching:            boolOrDefault(fileConfig.PromptCaching, true),
		AutomaticPromptCache:     boolOrDefault(fileConfig.AutomaticPromptCache, true),
		ExplicitCacheBreakpoints: boolOrDefault(fileConfig.ExplicitCacheBreakpoints, true),
		AllowRetentionDowngrade:  boolOrDefault(fileConfig.AllowRetentionDowngrade, false),
		MaxBreakpoints:           intOrDefault(fileConfig.MaxBreakpoints, 4),
		MinCacheTokens:           intOrDefault(fileConfig.MinCacheTokens, 1024),
		ExpectedReuse:            intOrDefault(fileConfig.ExpectedReuse, 2),
		MinimumValueScore:        intOrDefault(fileConfig.MinimumValueScore, 2048),
	}
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
