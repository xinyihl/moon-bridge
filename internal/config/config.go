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
	// Routes maps a model alias to "provider/upstream_model".
	Routes         map[string]RouteEntry
	ProviderDefs   map[string]ProviderDef
	Cache          CacheConfig
	ResponseProxy  ResponseProxyConfig
	AnthropicProxy AnthropicProxyConfig
}

// RouteEntry is a resolved route: alias -> provider key + upstream model name + metadata.
type RouteEntry struct {
	Provider        string
	Model           string // upstream model name
	ContextWindow   int
	MaxOutputTokens int
	InputPrice      float64
	OutputPrice     float64
	CacheWritePrice float64
	CacheReadPrice  float64
}

// ProviderDef defines a single upstream provider.
type ProviderDef struct {
	BaseURL          string
	APIKey           string
	Version          string
	UserAgent        string
	Protocol         string // "anthropic" (default) or "openai"
	DeepSeekV4       bool
	WebSearchSupport WebSearchSupport
	WebSearchMaxUses int
	TavilyAPIKey     string
	FirecrawlAPIKey  string
	SearchMaxRounds  int
	// Models is the provider's model catalog: upstream model name -> metadata.
	Models map[string]ModelMeta
}

// ModelMeta holds metadata for a model offered by a provider.
type ModelMeta struct {
	ContextWindow   int
	MaxOutputTokens int
	InputPrice      float64
	OutputPrice     float64
	CacheWritePrice float64
	CacheReadPrice  float64
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
	MinBreakpointTokens      int `yaml:"min_breakpoint_tokens"`
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
	if len(cfg.ProviderDefs) > 0 {
		if len(cfg.Routes) == 0 {
			return errors.New("routes must contain at least one model mapping")
		}
		for key, def := range cfg.ProviderDefs {
			if key == "" {
				return errors.New("providers cannot contain empty provider keys")
			}
			if def.BaseURL == "" {
				return fmt.Errorf("providers.%s.base_url is required", key)
			}
			if def.APIKey == "" {
				return fmt.Errorf("providers.%s.api_key is required", key)
			}
			switch def.Protocol {
			case "", "anthropic", "openai":
			default:
				return fmt.Errorf("providers.%s.protocol must be \"anthropic\" or \"openai\"", key)
			}
			if def.DeepSeekV4 && def.Protocol == "openai" {
				return fmt.Errorf("providers.%s.deepseek_v4 requires anthropic protocol", key)
			}
		}
		for alias, route := range cfg.Routes {
			if alias == "" || route.Model == "" {
				return errors.New("routes cannot contain empty aliases or models")
			}
			if _, ok := cfg.ProviderDefs[route.Provider]; !ok {
				return fmt.Errorf("routes.%s references unknown provider %q", alias, route.Provider)
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
	if len(cfg.Routes) == 0 {
		return errors.New("routes must contain at least one model mapping")
	}
	for alias, route := range cfg.Routes {
		if alias == "" || route.Model == "" {
			return errors.New("routes cannot contain empty aliases or models")
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
	for key, def := range cfg.ProviderDefs {
		if def.WebSearchSupport == WebSearchSupportInjected {
			tavilyKey := def.TavilyAPIKey
			if tavilyKey == "" {
				tavilyKey = cfg.TavilyAPIKey
			}
			if tavilyKey == "" {
				return fmt.Errorf("providers.%s.web_search.tavily_api_key is required when web_search.support is 'injected'", key)
			}
			maxRounds := def.SearchMaxRounds
			if maxRounds <= 0 {
				maxRounds = cfg.SearchMaxRounds
			}
			if maxRounds <= 0 {
				return fmt.Errorf("providers.%s.web_search.search_max_rounds must be > 0 when web_search.support is 'injected'", key)
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

// ModelFor resolves a model alias to the upstream model name via Routes.
// Supports "provider/model" direct reference.
func (cfg Config) ModelFor(model string) string {
	// Direct provider/model reference.
	if provider, upstream := ParseModelRef(model); provider != "" {
		if _, ok := cfg.ProviderDefs[provider]; ok {
			return upstream
		}
	}
	if cfg.Routes == nil {
		return model
	}
	if route, ok := cfg.Routes[model]; ok && route.Model != "" {
		return route.Model
	}
	return model
}

// RouteFor returns the full RouteEntry for a model alias.
// Supports "provider/model" direct reference.
func (cfg Config) RouteFor(model string) RouteEntry {
	// Direct provider/model reference.
	if provider, upstream := ParseModelRef(model); provider != "" {
		if def, ok := cfg.ProviderDefs[provider]; ok {
			entry := RouteEntry{Provider: provider, Model: upstream}
			if meta, ok := def.Models[upstream]; ok {
				entry.ContextWindow = meta.ContextWindow
				entry.MaxOutputTokens = meta.MaxOutputTokens
				entry.InputPrice = meta.InputPrice
				entry.OutputPrice = meta.OutputPrice
				entry.CacheWritePrice = meta.CacheWritePrice
				entry.CacheReadPrice = meta.CacheReadPrice
			}
			return entry
		}
	}
	if cfg.Routes == nil {
		return RouteEntry{}
	}
	return cfg.Routes[model]
}

func (cfg Config) DefaultModelAlias() string {
	if cfg.DefaultModel != "" {
		return cfg.DefaultModel
	}
	if _, ok := cfg.Routes["moonbridge"]; ok {
		return "moonbridge"
	}
	if len(cfg.Routes) == 1 {
		for alias := range cfg.Routes {
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
func (cfg Config) WebSearchForProvider(providerKey string) WebSearchSupport {
	if def, ok := cfg.ProviderDefs[providerKey]; ok && def.WebSearchSupport != "" {
		return def.WebSearchSupport
	}
	return cfg.WebSearchSupport
}

// WebSearchForModel returns the resolved web search support for a given model alias.
func (cfg Config) WebSearchForModel(modelAlias string) WebSearchSupport {
	// Direct provider/model reference.
	if provider, _ := ParseModelRef(modelAlias); provider != "" {
		return cfg.WebSearchForProvider(provider)
	}
	if route, ok := cfg.Routes[modelAlias]; ok && route.Provider != "" {
		return cfg.WebSearchForProvider(route.Provider)
	}
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

func (cfg Config) DeepSeekV4ForProvider(providerKey string) bool {
	if def, ok := cfg.ProviderDefs[providerKey]; ok {
		return def.DeepSeekV4
	}
	return false
}

func (cfg Config) DeepSeekV4ForModel(modelAlias string) bool {
	// Direct provider/model reference.
	if provider, _ := ParseModelRef(modelAlias); provider != "" {
		return cfg.DeepSeekV4ForProvider(provider)
	}
	if route, ok := cfg.Routes[modelAlias]; ok && route.Provider != "" {
		return cfg.DeepSeekV4ForProvider(route.Provider)
	}
	if _, ok := cfg.ProviderDefs["default"]; ok {
		return cfg.DeepSeekV4ForProvider("default")
	}
	return false
}

// ProviderFor returns the provider key that serves the given model alias.
// Supports "provider/model" direct reference.
func (cfg Config) ProviderFor(modelAlias string) string {
	// Direct provider/model reference.
	if provider, _ := ParseModelRef(modelAlias); provider != "" {
		if _, ok := cfg.ProviderDefs[provider]; ok {
			return provider
		}
	}
	if route, ok := cfg.Routes[modelAlias]; ok {
		return route.Provider
	}
	return ""
}
// ParseModelRef parses a model reference that may be in "provider/model" format.
// Returns (providerKey, modelName). If no slash, providerKey is "" and modelName is the input.
func ParseModelRef(ref string) (provider, model string) {
	before, after, found := strings.Cut(strings.TrimSpace(ref), "/")
	if !found {
		return "", ref
	}
	return strings.TrimSpace(before), strings.TrimSpace(after)
}
