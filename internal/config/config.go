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
	WebSearchSupportInjected WebSearchSupport = "injected"
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
	Protocol          string // "anthropic" (default) or "openai"
	WebSearchSupport  WebSearchSupport
	WebSearchMaxUses  int
	TavilyAPIKey      string
	FirecrawlAPIKey   string
	SearchMaxRounds   int
	DefaultMaxTokens  int
	ModelMap          map[string]string
	ProviderModels    map[string]ProviderModelConfig
	ProviderDefs      map[string]ProviderDef
	Cache             CacheConfig
	ResponseProxy     ResponseProxyConfig
	AnthropicProxy    AnthropicProxyConfig
	DeepSeekV4        bool
}

// ProviderDef defines a single upstream provider for multi-provider mode.
type ProviderDef struct {
	BaseURL   string
	APIKey    string
	Version   string
	UserAgent string
	Protocol  string // "anthropic" (default) or "openai"
	WebSearchSupport  WebSearchSupport
	WebSearchMaxUses  int
	TavilyAPIKey      string
	FirecrawlAPIKey   string
	SearchMaxRounds   int
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
	Provider        string // provider key (empty = "default")
	ContextWindow   int
	MaxOutputTokens int
	InputPrice      float64
	OutputPrice     float64
	CacheWritePrice float64
	CacheReadPrice  float64
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
	if err := cfg.validateSearchConfig(); err != nil {
		return err
	}
	// Multi-provider mode: ProviderDefs is non-empty.
	if len(cfg.ProviderDefs) > 0 {
		if len(cfg.ProviderModels) == 0 {
			return errors.New("provider.models must contain at least one model mapping")
		}
		for key, def := range cfg.ProviderDefs {
			if key == "" {
				return errors.New("provider.providers cannot contain empty provider keys")
			}
			if def.BaseURL == "" {
				return fmt.Errorf("provider.providers.%s.base_url is required", key)
			}
			if def.APIKey == "" {
				return fmt.Errorf("provider.providers.%s.api_key is required", key)
			}
			switch def.Protocol {
			case "", "anthropic", "openai":
			default:
				return fmt.Errorf("provider.providers.%s.protocol must be \"anthropic\" or \"openai\"", key)
			}
		}
		for alias, model := range cfg.ProviderModels {
			if alias == "" || model.Name == "" {
				return errors.New("provider.models cannot contain empty aliases or models")
			}
			providerKey := model.Provider
			if providerKey == "" {
				providerKey = "default"
			}
			if _, ok := cfg.ProviderDefs[providerKey]; !ok {
				return fmt.Errorf("provider.models.%s.provider references unknown provider %q", alias, providerKey)
			}
		}
		return nil
	}
	// Legacy single-provider mode.
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

func (cfg Config) validateSearchConfig() error {
	if cfg.WebSearchSupport == WebSearchSupportInjected {
		if cfg.TavilyAPIKey == "" {
			return errors.New("provider.tavily_api_key is required when web_search.support is 'injected'")
		}
		if cfg.SearchMaxRounds <= 0 {
			return errors.New("provider.search_max_rounds must be > 0 when web_search.support is 'injected'")
		}
	}
	// Validate per-provider injected configs.
	for key, def := range cfg.ProviderDefs {
		if def.WebSearchSupport == WebSearchSupportInjected {
			tavilyKey := def.TavilyAPIKey
			if tavilyKey == "" {
				tavilyKey = cfg.TavilyAPIKey
			}
			if tavilyKey == "" {
				return fmt.Errorf("provider.providers.%s.web_search.tavily_api_key is required when web_search.support is 'injected'", key)
			}
			maxRounds := def.SearchMaxRounds
			if maxRounds <= 0 {
				maxRounds = cfg.SearchMaxRounds
			}
			if maxRounds <= 0 {
				return fmt.Errorf("provider.providers.%s.web_search.search_max_rounds must be > 0 when web_search.support is 'injected'", key)
			}
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

func (cfg Config) WebSearchInjected() bool {
	return cfg.WebSearchSupport == WebSearchSupportInjected
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

// WebSearchForProvider returns the resolved web search support for a given provider key.
// It checks the provider-level override first, then falls back to the global setting.
func (cfg Config) WebSearchForProvider(providerKey string) WebSearchSupport {
	if def, ok := cfg.ProviderDefs[providerKey]; ok && def.WebSearchSupport != "" {
		return def.WebSearchSupport
	}
	return cfg.WebSearchSupport
}

// WebSearchForModel returns the resolved web search support for a given model alias.
func (cfg Config) WebSearchForModel(modelAlias string) WebSearchSupport {
	if pm, ok := cfg.ProviderModels[modelAlias]; ok && pm.Provider != "" {
		return cfg.WebSearchForProvider(pm.Provider)
	}
	// No explicit provider; try "default" key, then fall back to global.
	if _, ok := cfg.ProviderDefs["default"]; ok {
		return cfg.WebSearchForProvider("default")
	}
	return cfg.WebSearchSupport
}

// WebSearchMaxUsesForProvider returns the max uses for a given provider key.
func (cfg Config) WebSearchMaxUsesForProvider(providerKey string) int {
	if def, ok := cfg.ProviderDefs[providerKey]; ok && def.WebSearchMaxUses > 0 {
		return def.WebSearchMaxUses
	}
	if cfg.WebSearchMaxUses > 0 {
		return cfg.WebSearchMaxUses
	}
	return 8
}

// WebSearchTavilyKeyForProvider returns the Tavily API key for a given provider key.
func (cfg Config) WebSearchTavilyKeyForProvider(providerKey string) string {
	if def, ok := cfg.ProviderDefs[providerKey]; ok && def.TavilyAPIKey != "" {
		return def.TavilyAPIKey
	}
	return cfg.TavilyAPIKey
}

// WebSearchFirecrawlKeyForProvider returns the Firecrawl API key for a given provider key.
func (cfg Config) WebSearchFirecrawlKeyForProvider(providerKey string) string {
	if def, ok := cfg.ProviderDefs[providerKey]; ok && def.FirecrawlAPIKey != "" {
		return def.FirecrawlAPIKey
	}
	return cfg.FirecrawlAPIKey
}

// WebSearchMaxRoundsForProvider returns the search max rounds for a given provider key.
func (cfg Config) WebSearchMaxRoundsForProvider(providerKey string) int {
	if def, ok := cfg.ProviderDefs[providerKey]; ok && def.SearchMaxRounds > 0 {
		return def.SearchMaxRounds
	}
	if cfg.SearchMaxRounds > 0 {
		return cfg.SearchMaxRounds
	}
	return 5
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

func (cfg Config) DeepSeekV4Enabled() bool {
	return cfg.DeepSeekV4
}
