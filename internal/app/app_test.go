package app

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"moonbridge/internal/config"
	"moonbridge/internal/provider"
	mbtrace "moonbridge/internal/trace"
)

func TestWelcomeMessage(t *testing.T) {
	want := "Welcome to Moon Bridge!"

	if got := WelcomeMessage(); got != want {
		t.Fatalf("WelcomeMessage() = %q, want %q", got, want)
	}
}

func TestRunWritesWelcomeMessage(t *testing.T) {
	var output bytes.Buffer

	Run(&output)

	want := "Welcome to Moon Bridge!\n"
	if got := output.String(); got != want {
		t.Fatalf("Run() wrote %q, want %q", got, want)
	}
}

func TestCaptureTraceDirectoriesUseSession(t *testing.T) {
	responseTracer := mbtrace.New(captureResponseTraceConfig(true))
	if got, want := responseTracer.Directory(), filepath.Join("trace", "Capture", "Response", responseTracer.SessionID()); got != want {
		t.Fatalf("response trace directory = %q, want %q", got, want)
	}

	anthropicTracer := mbtrace.New(captureAnthropicTraceConfig(true))
	if got, want := anthropicTracer.Directory(), filepath.Join("trace", "Capture", "Anthropic", anthropicTracer.SessionID()); got != want {
		t.Fatalf("anthropic trace directory = %q, want %q", got, want)
	}
}

func TestResolvePerProviderWebSearchDisabledByConfig(t *testing.T) {
	cfg := config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"default": {WebSearchSupport: config.WebSearchSupportDisabled},
		},
	}
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"default": {BaseURL: "https://test.example.test", APIKey: "test-key"},
		},
		map[string]provider.ModelRoute{
			"moonbridge": {Name: "claude-test", Provider: "default"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	resolvePerProviderWebSearch(context.Background(), cfg, pm, &bytes.Buffer{})
	if got := pm.ResolvedWebSearch("default"); got != "disabled" {
		t.Fatalf("ResolvedWebSearch(default) = %q, want disabled", got)
	}
}

func TestResolvePerProviderWebSearchEnabledByConfig(t *testing.T) {
	cfg := config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"default": {WebSearchSupport: config.WebSearchSupportEnabled},
		},
	}
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"default": {BaseURL: "https://test.example.test", APIKey: "test-key"},
		},
		map[string]provider.ModelRoute{
			"moonbridge": {Name: "claude-test", Provider: "default"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	resolvePerProviderWebSearch(context.Background(), cfg, pm, &bytes.Buffer{})
	if got := pm.ResolvedWebSearch("default"); got != "enabled" {
		t.Fatalf("ResolvedWebSearch(default) = %q, want enabled", got)
	}
}

func TestResolvePerProviderWebSearchNonAnthropicDisabled(t *testing.T) {
	cfg := config.Config{
		WebSearchSupport: config.WebSearchSupportEnabled,
		ProviderDefs: map[string]config.ProviderDef{
			"openai": {},
		},
	}
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"openai": {BaseURL: "https://openai.example.test", APIKey: "key", Protocol: "openai"},
		},
		map[string]provider.ModelRoute{},
	)
	if err != nil {
		t.Fatal(err)
	}

	resolvePerProviderWebSearch(context.Background(), cfg, pm, &bytes.Buffer{})
	if got := pm.ResolvedWebSearch("openai"); got != "disabled" {
		t.Fatalf("ResolvedWebSearch(openai) = %q, want disabled for non-anthropic", got)
	}
}

func TestResolvePerProviderWebSearchFallsBackToGlobal(t *testing.T) {
	cfg := config.Config{
		WebSearchSupport: config.WebSearchSupportDisabled,
		ProviderDefs: map[string]config.ProviderDef{
			"default": {}, // no per-provider override
		},
	}
	pm, err := provider.NewProviderManager(
		map[string]provider.ProviderConfig{
			"default": {BaseURL: "https://test.example.test", APIKey: "test-key"},
		},
		map[string]provider.ModelRoute{
			"moonbridge": {Name: "claude-test", Provider: "default"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	resolvePerProviderWebSearch(context.Background(), cfg, pm, &bytes.Buffer{})
	if got := pm.ResolvedWebSearch("default"); got != "disabled" {
		t.Fatalf("ResolvedWebSearch(default) = %q, want disabled (from global fallback)", got)
	}
}

func TestBuildProviderDefsFromConfigKeepsMultiProviderDefinitions(t *testing.T) {
	cfg := config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"deepseek": {
				BaseURL: "https://deepseek.example.test",
				APIKey:  "deepseek-key",
			},
			"openai": {
				BaseURL:  "https://openai.example.test",
				APIKey:   "openai-key",
				Protocol: "openai",
			},
		},
	}

	defs := buildProviderDefsFromConfig(cfg)
	if len(defs) != 2 {
		t.Fatalf("defs = %+v", defs)
	}
	if defs["openai"].BaseURL != "https://openai.example.test" || defs["openai"].Protocol != "openai" {
		t.Fatalf("openai def = %+v", defs["openai"])
	}
	if defs["deepseek"].BaseURL != "https://deepseek.example.test" {
		t.Fatalf("deepseek def = %+v", defs["deepseek"])
	}
}
