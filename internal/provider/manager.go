// Package provider manages multiple upstream LLM providers and routes
// requests to the correct provider based on the requested model.
package provider

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"moonbridge/internal/anthropic"
)

// HTTPConfig controls the HTTP connection pool for a provider.
type HTTPConfig struct {
	MaxIdleConnsPerHost int    `yaml:"max_idle_conns_per_host"`
	IdleConnTimeout     string `yaml:"idle_conn_timeout"`
}

// ProviderConfig defines how to connect to a single upstream provider.
type ProviderConfig struct {
	BaseURL   string     `yaml:"base_url"`
	APIKey    string     `yaml:"api_key"`
	Version   string     `yaml:"version"`
	UserAgent string     `yaml:"user_agent"`
	HTTP      HTTPConfig `yaml:"http"`
}

// ModelRoute maps a model alias to a provider and an upstream model name.
type ModelRoute struct {
	Provider string `yaml:"provider"` // key in ProviderConfig map; empty means "default"
	Name     string `yaml:"name"`     // upstream model name
}

// ProviderClient wraps an anthropic.Client (or equivalent) with its key.
type ProviderClient struct {
	Client   *anthropic.Client
	Provider string // provider key
}

// ProviderManager manages multiple upstream provider clients and routes
// model aliases to the appropriate provider.
type ProviderManager struct {
	clients   map[string]*anthropic.Client // provider key -> anthropic.Client
	providers map[string]ProviderConfig    // provider key -> config (for inspection)
	routes    map[string]ModelRoute        // model alias -> route
	defaultK  string                       // default provider key
}

// NewProviderManager creates a ProviderManager from provider configs and model routes.
// providerCfgs: provider key -> ProviderConfig
// routes: model alias -> ModelRoute
func NewProviderManager(providerCfgs map[string]ProviderConfig, routes map[string]ModelRoute) (*ProviderManager, error) {
	pm := &ProviderManager{
		clients:   make(map[string]*anthropic.Client, len(providerCfgs)),
		providers: providerCfgs,
		routes:    routes,
	}

	// Build clients for each provider config.
	for key, cfg := range providerCfgs {
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: base_url is required", key)
		}
		httpClient := newHTTPClient(cfg.HTTP)
		pm.clients[key] = anthropic.NewClient(anthropic.ClientConfig{
			BaseURL:   cfg.BaseURL,
			APIKey:    cfg.APIKey,
			Version:   cfg.Version,
			UserAgent: cfg.UserAgent,
			Client:    httpClient,
		})
	}

	// Pick the default key.
	if _, hasDefault := providerCfgs["default"]; hasDefault {
		pm.defaultK = "default"
	} else if len(providerCfgs) == 1 {
		for k := range providerCfgs {
			pm.defaultK = k
		}
	}

	if len(pm.clients) == 0 {
		return nil, fmt.Errorf("at least one provider must be configured")
	}
	return pm, nil
}

// ClientFor returns the anthropic.Client and upstream model name for a given model alias.
// It returns the default provider if the alias is not explicitly routed.
func (pm *ProviderManager) ClientFor(modelAlias string) (string, *anthropic.Client, error) {
	route, ok := pm.routes[modelAlias]
	if !ok {
		// No explicit route: use default provider with the model name as-is.
		client, ok := pm.clients[pm.defaultK]
		if !ok {
			return "", nil, fmt.Errorf("no route for model %q and no default provider", modelAlias)
		}
		return modelAlias, client, nil
	}

	providerKey := route.Provider
	if providerKey == "" {
		providerKey = pm.defaultK
	}
	client, ok := pm.clients[providerKey]
	if !ok {
		return "", nil, fmt.Errorf("provider %q (referenced by model %q) not configured", providerKey, modelAlias)
	}
	return route.Name, client, nil
}

// ProbeWebSearch probes a specific model's provider for web_search support.
func (pm *ProviderManager) ProbeWebSearch(ctx interface{ DeepSeekV4Enabled() bool }, modelAlias string) bool {
	_, client, err := pm.ClientFor(modelAlias)
	if err != nil {
		return false
	}
	// Build a minimal Anthropic request from the model info.
	supported, probeErr := client.ProbeWebSearch(nil, modelAlias)
	if probeErr != nil {
		return false
	}
	return supported
}

// ProviderKeys returns all configured provider keys.
func (pm *ProviderManager) ProviderKeys() []string {
	keys := make([]string, 0, len(pm.clients))
	for k := range pm.clients {
		keys = append(keys, k)
	}
	return keys
}

// DefaultKey returns the default provider key.
func (pm *ProviderManager) DefaultKey() string {
	return pm.defaultK
}

// newHTTPClient creates an *http.Client with connection pooling configured.
func newHTTPClient(cfg HTTPConfig) *http.Client {
	maxIdle := cfg.MaxIdleConnsPerHost
	if maxIdle <= 0 {
		maxIdle = 4
	}

	idleTimeout := 90 * time.Second
	if cfg.IdleConnTimeout != "" {
		if d, err := time.ParseDuration(cfg.IdleConnTimeout); err == nil {
			idleTimeout = d
		}
	}

	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: maxIdle,
			IdleConnTimeout:     idleTimeout,
			DisableCompression:  false,
		},
	}
}

// BuildProviderConfigs converts legacy single-provider config and new multi-provider
// config into a unified provider config map.
// If new-style providers are present, they take precedence.
// Otherwise a "default" provider is built from the legacy fields.
func BuildProviderConfigs(
	legacyBaseURL, legacyAPIKey, legacyVersion, legacyUserAgent string,
	newProviders map[string]ProviderConfig,
) map[string]ProviderConfig {
	if len(newProviders) > 0 {
		// Normalise: ensure every provider has a base URL.
		normalised := make(map[string]ProviderConfig, len(newProviders))
		for k, v := range newProviders {
			v.BaseURL = strings.TrimRight(strings.TrimSpace(v.BaseURL), "/")
			v.APIKey = strings.TrimSpace(v.APIKey)
			v.Version = strings.TrimSpace(v.Version)
			v.UserAgent = strings.TrimSpace(v.UserAgent)
			normalised[k] = v
		}
		return normalised
	}

	// Legacy single-provider mode: build a "default" entry.
	cfg := ProviderConfig{
		BaseURL:   strings.TrimRight(strings.TrimSpace(legacyBaseURL), "/"),
		APIKey:    strings.TrimSpace(legacyAPIKey),
		Version:   valueOrDefault(legacyVersion, "2023-06-01"),
		UserAgent: strings.TrimSpace(legacyUserAgent),
	}
	if cfg.BaseURL == "" {
		return nil
	}
	return map[string]ProviderConfig{"default": cfg}
}

// BuildModelRoutes converts the old ProviderModels map (alias -> ProviderModelConfig)
// and an optional provider key into ModelRoute entries. If ProviderModelConfig has
// no provider field, it defaults to "default".
func BuildModelRoutes(models map[string]struct {
	Name     string
	Provider string
}) map[string]ModelRoute {
	if len(models) == 0 {
		return nil
	}
	routes := make(map[string]ModelRoute, len(models))
	for alias, m := range models {
		providerKey := m.Provider
		if providerKey == "" {
			providerKey = "default"
		}
		routes[strings.TrimSpace(alias)] = ModelRoute{
			Provider: strings.TrimSpace(providerKey),
			Name:     strings.TrimSpace(m.Name),
		}
	}
	return routes
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// ClientForKey returns the anthropic.Client for a given provider key.
func (pm *ProviderManager) ClientForKey(key string) (*anthropic.Client, error) {
	client, ok := pm.clients[key]
	if !ok {
		return nil, fmt.Errorf("provider %q not found", key)
	}
	return client, nil
}
