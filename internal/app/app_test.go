package app

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"moonbridge/internal/config"
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

type fakeWebSearchProber struct {
	supported bool
	err       error
	called    bool
	model     string
}

func (prober *fakeWebSearchProber) ProbeWebSearch(context.Context, string) (bool, error) {
	prober.called = true
	return prober.supported, prober.err
}

func TestResolveWebSearchSupportAutoDisablesUnsupportedProvider(t *testing.T) {
	prober := &fakeWebSearchProber{supported: false}
	cfg := config.Config{
		WebSearchSupport: config.WebSearchSupportAuto,
		DefaultModel:     "moonbridge",
		ModelMap:         map[string]string{"moonbridge": "claude-test"},
	}

	resolved := resolveWebSearchSupport(context.Background(), cfg, prober, &bytes.Buffer{})
	if !prober.called {
		t.Fatal("ProbeWebSearch was not called")
	}
	if resolved.WebSearchSupport != config.WebSearchSupportDisabled {
		t.Fatalf("WebSearchSupport = %q, want disabled", resolved.WebSearchSupport)
	}
	if resolved.WebSearchEnabled() {
		t.Fatal("WebSearchEnabled() = true, want false")
	}
}

func TestResolveWebSearchSupportDisabledSkipsProbe(t *testing.T) {
	prober := &fakeWebSearchProber{supported: true}
	cfg := config.Config{WebSearchSupport: config.WebSearchSupportDisabled}

	resolved := resolveWebSearchSupport(context.Background(), cfg, prober, &bytes.Buffer{})
	if prober.called {
		t.Fatal("ProbeWebSearch was called for disabled web_search")
	}
	if resolved.WebSearchEnabled() {
		t.Fatal("WebSearchEnabled() = true, want false")
	}
}

func TestResolveWebSearchSupportDisablesWhenProbeModelIsMissing(t *testing.T) {
	prober := &fakeWebSearchProber{supported: true}
	cfg := config.Config{
		WebSearchSupport: config.WebSearchSupportAuto,
		ProviderModels: map[string]config.ProviderModelConfig{
			"slow": {Name: "claude-slow"},
			"fast": {Name: "claude-fast"},
		},
	}

	resolved := resolveWebSearchSupport(context.Background(), cfg, prober, &bytes.Buffer{})
	if prober.called {
		t.Fatal("ProbeWebSearch was called without a deterministic model")
	}
	if resolved.WebSearchEnabled() {
		t.Fatal("WebSearchEnabled() = true, want false without a probe model")
	}
}

func TestResolveWebSearchSupportDisablesOnProbeInfrastructureError(t *testing.T) {
	prober := &fakeWebSearchProber{err: errors.New("network down")}
	cfg := config.Config{
		WebSearchSupport: config.WebSearchSupportAuto,
		DefaultModel:     "moonbridge",
		ModelMap:         map[string]string{"moonbridge": "claude-test"},
	}

	resolved := resolveWebSearchSupport(context.Background(), cfg, prober, &bytes.Buffer{})
	if resolved.WebSearchEnabled() {
		t.Fatal("WebSearchEnabled() = true, want false when auto probe cannot prove support")
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
