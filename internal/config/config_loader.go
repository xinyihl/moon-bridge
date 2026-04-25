package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type FileConfig struct {
	Mode          string              `yaml:"mode"`
	TraceRequests bool                `yaml:"trace_requests"`
	Log           LogFileConfig       `yaml:"log"`
	Server        ServerFileConfig    `yaml:"server"`
	Provider      ProviderFileConfig  `yaml:"provider"`
	Cache         CacheFileConfig     `yaml:"cache"`
	SystemPrompt  string              `yaml:"system_prompt"`
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
	DeepSeekV4       bool                               `yaml:"deepseek_v4"`
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
	Name            string                 `yaml:"name"`
	ContextWindow   int                    `yaml:"context_window"`
	MaxOutputTokens int                    `yaml:"max_output_tokens"`
	Pricing         ModelPricingFileConfig `yaml:"pricing"`
}

// ModelPricingFileConfig holds optional per-model pricing in RMB per M tokens.
type ModelPricingFileConfig struct {
	InputPrice      float64 `yaml:"input_price"`
	OutputPrice     float64 `yaml:"output_price"`
	CacheWritePrice float64 `yaml:"cache_write_price"`
	CacheReadPrice  float64 `yaml:"cache_read_price"`
}

type WebSearchFileConfig struct {
	Support         string `yaml:"support"`
	MaxUses         int    `yaml:"max_uses"`
	TavilyAPIKey    string `yaml:"tavily_api_key"`
	FirecrawlAPIKey string `yaml:"firecrawl_api_key"`
	SearchMaxRounds int    `yaml:"search_max_rounds"`
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

type LogFileConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
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
	webSearchSupport, err := parseWebSearchSupport(fileConfig.Provider.WebSearch.Support)
	if err != nil {
		return Config{}, err
	}
	providerModels := FromProviderModelFileConfig(fileConfig.Provider.Models)
	cfg := Config{
		Mode:              mode,
		Addr:              valueOrDefault(strings.TrimSpace(fileConfig.Server.Addr), DefaultAddr),
		TraceRequests:     fileConfig.TraceRequests,
		LogLevel:          valueOrDefault(strings.TrimSpace(fileConfig.Log.Level), "info"),
		LogFormat:         valueOrDefault(strings.TrimSpace(fileConfig.Log.Format), "text"),
		SystemPrompt:      strings.TrimSpace(fileConfig.SystemPrompt),
		DefaultModel:      strings.TrimSpace(fileConfig.Provider.DefaultModel),
		ProviderBaseURL:   strings.TrimRight(strings.TrimSpace(fileConfig.Provider.BaseURL), "/"),
		ProviderAPIKey:    strings.TrimSpace(fileConfig.Provider.APIKey),
		ProviderVersion:   valueOrDefault(strings.TrimSpace(fileConfig.Provider.Version), "2023-06-01"),
		ProviderUserAgent: strings.TrimSpace(fileConfig.Provider.UserAgent),
		WebSearchSupport:  webSearchSupport,
		WebSearchMaxUses:  intOrDefault(fileConfig.Provider.WebSearch.MaxUses, 8),
		TavilyAPIKey:      strings.TrimSpace(fileConfig.Provider.WebSearch.TavilyAPIKey),
		FirecrawlAPIKey:   strings.TrimSpace(fileConfig.Provider.WebSearch.FirecrawlAPIKey),
		SearchMaxRounds:   intOrDefault(fileConfig.Provider.WebSearch.SearchMaxRounds, 5),
		DefaultMaxTokens:  intOrDefault(fileConfig.Provider.DefaultMaxTokens, 1024),
		ModelMap:          providerModelMap(providerModels),
		ProviderModels:    providerModels,
		Cache:             fromCacheFileConfig(fileConfig.Cache),
		ResponseProxy:     FromResponseProxyFileConfig(fileConfig.Developer.Proxy.Response),
		DeepSeekV4:        fileConfig.Provider.DeepSeekV4,
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

func parseWebSearchSupport(value string) (WebSearchSupport, error) {
	switch support := WebSearchSupport(strings.TrimSpace(value)); support {
	case "":
		return WebSearchSupportAuto, nil
	case WebSearchSupportAuto, WebSearchSupportEnabled, WebSearchSupportDisabled, WebSearchSupportInjected:
		return support, nil
	default:
		return "", fmt.Errorf("invalid provider.web_search.support %q", value)
	}
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
			InputPrice:      model.Pricing.InputPrice,
			OutputPrice:     model.Pricing.OutputPrice,
			CacheWritePrice: model.Pricing.CacheWritePrice,
			CacheReadPrice:  model.Pricing.CacheReadPrice,
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
