package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"moonbridge/internal/config"
)

func TestLoadFromYAMLParsesTransformConfig(t *testing.T) {
	cfg, err := config.LoadFromYAML([]byte(`
mode: Transform
provider:
  base_url: https://provider.example.test
  api_key: upstream-key
  user_agent: Bun/1.3.13
  web_search:
    max_uses: 12
  default_model: gpt-test
  models:
    gpt-test:
      name: claude-test
      context_window: 200000
      max_output_tokens: 100000
    gpt-fast:
      name: claude-fast
cache:
  mode: explicit
  ttl: 1h
trace_requests: true
`))
	if err != nil {
		t.Fatalf("LoadFromYAML() error = %v", err)
	}

	if cfg.Mode != config.ModeTransform {
		t.Fatalf("Mode = %q", cfg.Mode)
	}
	if cfg.Addr != "127.0.0.1:38440" {
		t.Fatalf("Addr = %q, want 127.0.0.1:38440", cfg.Addr)
	}
	if cfg.ProviderVersion != "2023-06-01" {
		t.Fatalf("ProviderVersion = %q", cfg.ProviderVersion)
	}
	if cfg.ProviderUserAgent != "Bun/1.3.13" {
		t.Fatalf("ProviderUserAgent = %q", cfg.ProviderUserAgent)
	}
	if cfg.WebSearchMaxUses != 12 {
		t.Fatalf("WebSearchMaxUses = %d", cfg.WebSearchMaxUses)
	}
	if cfg.DefaultMaxTokens != 1024 {
		t.Fatalf("DefaultMaxTokens = %d", cfg.DefaultMaxTokens)
	}
	if got := cfg.ModelFor("gpt-test"); got != "claude-test" {
		t.Fatalf("ModelFor(gpt-test) = %q", got)
	}
	if got := cfg.DefaultModelAlias(); got != "gpt-test" {
		t.Fatalf("DefaultModelAlias() = %q", got)
	}
	if cfg.Cache.Mode != "explicit" || cfg.Cache.TTL != "1h" {
		t.Fatalf("Cache = %+v", cfg.Cache)
	}
	if !cfg.TraceRequests {
		t.Fatal("TraceRequests = false, want true")
	}
	providerModel := cfg.ProviderModelFor("gpt-test")
	if providerModel.Name != "claude-test" || providerModel.ContextWindow != 200000 || providerModel.MaxOutputTokens != 100000 {
		t.Fatalf("ProviderModelFor(gpt-test) = %+v", providerModel)
	}
}

func TestLoadFromYAMLRequiresMode(t *testing.T) {
	_, err := config.LoadFromYAML([]byte(`{}`))
	if err == nil {
		t.Fatal("LoadFromYAML() error = nil, want missing mode error")
	}
}

func TestLoadFromYAMLRejectsInvalidMode(t *testing.T) {
	_, err := config.LoadFromYAML([]byte(`mode: Proxy`))
	if err == nil {
		t.Fatal("LoadFromYAML() error = nil, want invalid mode error")
	}
}

func TestLoadFromYAMLRequiresTransformProviderSettings(t *testing.T) {
	_, err := config.LoadFromYAML([]byte(`mode: Transform`))
	if err == nil {
		t.Fatal("LoadFromYAML() error = nil, want missing provider settings error")
	}
}

func TestLoadFromYAMLRejectsInvalidCacheTTL(t *testing.T) {
	_, err := config.LoadFromYAML([]byte(`
mode: Transform
provider:
  base_url: https://provider.example.test
  api_key: upstream-key
  models:
    gpt-test:
      name: claude-test
cache:
  ttl: 24h
`))
	if err == nil {
		t.Fatal("LoadFromYAML() error = nil, want invalid cache TTL error")
	}
}

func TestLoadFromYAMLRejectsEmptyModelMapping(t *testing.T) {
	_, err := config.LoadFromYAML([]byte(`
mode: Transform
provider:
  base_url: https://provider.example.test
  api_key: upstream-key
  models:
    moonbridge:
      name: ""
`))
	if err == nil {
		t.Fatal("LoadFromYAML() error = nil, want empty model mapping error")
	}
}

func TestLoadFromEnvUsesMoonBridgeConfigPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(`
mode: Transform
server:
  addr: 127.0.0.1:9999
provider:
  base_url: https://provider.example.test
  api_key: upstream-key
  models:
    moonbridge:
      name: claude-test
cache:
  mode: off
`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("MOONBRIDGE_CONFIG", path)
	cfg, err := config.LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.Addr != "127.0.0.1:9999" {
		t.Fatalf("Addr = %q", cfg.Addr)
	}
	if cfg.Cache.Mode != "off" {
		t.Fatalf("Cache.Mode = %q", cfg.Cache.Mode)
	}
}

func TestLoadFromYAMLParsesCaptureResponseConfig(t *testing.T) {
	cfg, err := config.LoadFromYAML([]byte(`
mode: CaptureResponse
trace_requests: true
developer:
  proxy:
    response:
      model: gpt-capture
      provider:
        base_url: https://api.openai.example.test
        api_key: upstream-openai-key
`))
	if err != nil {
		t.Fatalf("LoadFromYAML() error = %v", err)
	}
	if !cfg.TraceRequests {
		t.Fatal("TraceRequests = false, want true")
	}
	if cfg.ResponseProxy.Model != "gpt-capture" {
		t.Fatalf("Model = %q", cfg.ResponseProxy.Model)
	}
	if cfg.ResponseProxy.ProviderBaseURL != "https://api.openai.example.test" {
		t.Fatalf("ProviderBaseURL = %q", cfg.ResponseProxy.ProviderBaseURL)
	}
	if cfg.ResponseProxy.ProviderAPIKey != "upstream-openai-key" {
		t.Fatalf("ProviderAPIKey = %q", cfg.ResponseProxy.ProviderAPIKey)
	}
}

func TestLoadFromYAMLParsesCaptureAnthropicConfig(t *testing.T) {
	cfg, err := config.LoadFromYAML([]byte(`
mode: CaptureAnthropic
trace_requests: true
developer:
  proxy:
    anthropic:
      model: claude-test
      provider:
        base_url: https://provider.example.test
        api_key: upstream-key
        version: 2023-06-01
`))
	if err != nil {
		t.Fatalf("LoadFromYAML() error = %v", err)
	}
	if cfg.AnthropicProxy.Model != "claude-test" {
		t.Fatalf("Model = %q", cfg.AnthropicProxy.Model)
	}
	if !cfg.TraceRequests {
		t.Fatal("TraceRequests = false, want true")
	}
	if cfg.AnthropicProxy.ProviderBaseURL != "https://provider.example.test" {
		t.Fatalf("ProviderBaseURL = %q", cfg.AnthropicProxy.ProviderBaseURL)
	}
	if cfg.AnthropicProxy.ProviderAPIKey != "upstream-key" {
		t.Fatalf("ProviderAPIKey = %q", cfg.AnthropicProxy.ProviderAPIKey)
	}
	if cfg.AnthropicProxy.ProviderVersion != "2023-06-01" {
		t.Fatalf("ProviderVersion = %q", cfg.AnthropicProxy.ProviderVersion)
	}
}

func TestDefaultModelAliasFallsBackToMoonbridge(t *testing.T) {
	cfg := config.Config{ProviderModels: map[string]config.ProviderModelConfig{
		"moonbridge": {Name: "claude-test"},
		"other":      {Name: "claude-other"},
	}}
	if got := cfg.DefaultModelAlias(); got != "moonbridge" {
		t.Fatalf("DefaultModelAlias() = %q", got)
	}
}

func TestCodexModelUsesResponseProxyModelInCaptureResponse(t *testing.T) {
	cfg := config.Config{
		Mode:         config.ModeCaptureResponse,
		DefaultModel: "moonbridge",
		ResponseProxy: config.ResponseProxyConfig{
			Model: "gpt-capture",
		},
	}
	if got := cfg.CodexModel(); got != "gpt-capture" {
		t.Fatalf("CodexModel() = %q", got)
	}
}

func TestCodexModelUsesDefaultModelInTransform(t *testing.T) {
	cfg := config.Config{
		Mode:         config.ModeTransform,
		DefaultModel: "moonbridge",
		ResponseProxy: config.ResponseProxyConfig{
			Model: "gpt-capture",
		},
	}
	if got := cfg.CodexModel(); got != "moonbridge" {
		t.Fatalf("CodexModel() = %q", got)
	}
}

func TestLoadFromYAMLRequiresCaptureProvider(t *testing.T) {
	for name, input := range map[string]string{
		"response":  `mode: CaptureResponse`,
		"anthropic": `mode: CaptureAnthropic`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := config.LoadFromYAML([]byte(input)); err == nil {
				t.Fatal("LoadFromYAML() error = nil, want missing proxy provider error")
			}
		})
	}
}

func TestOverrideAddrUsesSharedServerAddr(t *testing.T) {
	for _, mode := range []config.Mode{config.ModeTransform, config.ModeCaptureResponse, config.ModeCaptureAnthropic} {
		cfg := config.Config{Mode: mode}
		cfg.OverrideAddr("127.0.0.1:19999")
		if cfg.Addr != "127.0.0.1:19999" {
			t.Fatalf("OverrideAddr(%s) = %q", mode, cfg.Addr)
		}
	}
}

func TestLoadFromYAMLRejectsProxyAddr(t *testing.T) {
	_, err := config.LoadFromYAML([]byte(`
mode: CaptureResponse
developer:
  proxy:
    response:
      addr: 127.0.0.1:19180
      provider:
        base_url: https://api.openai.example.test
        api_key: upstream-openai-key
`))
	if err == nil {
		t.Fatal("LoadFromYAML() error = nil, want unknown proxy addr error")
	}
}
