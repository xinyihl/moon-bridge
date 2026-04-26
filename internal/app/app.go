package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/bridge"
	"moonbridge/internal/cache"
	"moonbridge/internal/config"
	"moonbridge/internal/logger"
	"moonbridge/internal/provider"
	"moonbridge/internal/proxy"
	"moonbridge/internal/server"
	"moonbridge/internal/stats"
	mbtrace "moonbridge/internal/trace"
)

const Name = "Moon Bridge"

func Run(output io.Writer) {
	fmt.Fprintln(output, WelcomeMessage())
}

func WelcomeMessage() string {
	return "Welcome to " + Name + "!"
}

func RunServerFromEnv(ctx context.Context, errors io.Writer) error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}
	return RunServer(ctx, cfg, errors)
}

func RunServer(ctx context.Context, cfg config.Config, errors io.Writer) error {
	switch cfg.Mode {
	case config.ModeTransform:
		logger.Info("starting server", "mode", cfg.Mode, "addr", cfg.Addr)
		return runTransform(ctx, cfg, errors)
	case config.ModeCaptureResponse:
		logger.Info("starting server", "mode", cfg.Mode, "addr", cfg.Addr)
		return runCaptureResponse(ctx, cfg, errors)
	case config.ModeCaptureAnthropic:
		logger.Info("starting server", "mode", cfg.Mode, "addr", cfg.Addr)
		return runCaptureAnthropic(ctx, cfg, errors)
	default:
		return fmt.Errorf("unsupported mode %q", cfg.Mode)
	}
}

func runTransform(ctx context.Context, cfg config.Config, errors io.Writer) error {
	// Build multi-provider infrastructure.
	providerDefs := buildProviderDefsFromConfig(cfg)
	modelRoutes := buildModelRoutesFromConfig(cfg)
	providerMgr, err := provider.NewProviderManager(providerDefs, modelRoutes)
	if err != nil {
		return fmt.Errorf("init provider manager: %w", err)
	}

	// Resolve a fallback client for web search probing and server fallback.
	// When no "default" provider is configured, probe and fallback are skipped.
	defaultClient := resolveDefaultClient(providerMgr, errors)
	resolvePerProviderWebSearch(ctx, cfg, providerMgr, errors)

	sessionStats := stats.NewSessionStats()
	// Set per-model pricing if configured
	pricing := make(map[string]stats.ModelPricing)
	for alias, pm := range cfg.ProviderModels {
		if pm.InputPrice > 0 || pm.OutputPrice > 0 || pm.CacheWritePrice > 0 || pm.CacheReadPrice > 0 {
			pricing[alias] = stats.ModelPricing{
				InputPrice:      pm.InputPrice,
				OutputPrice:     pm.OutputPrice,
				CacheWritePrice: pm.CacheWritePrice,
				CacheReadPrice:  pm.CacheReadPrice,
			}
		}
	}
	if len(pricing) > 0 {
		sessionStats.SetPricing(pricing)
	}
	tracer := mbtrace.New(mbtrace.Config{
		Enabled: cfg.TraceRequests,
		Root:    transformTraceRoot(),
	})
	logTrace(errors, "transform", tracer)

	// Determine the default provider to use as the fallback Provider.
	// *anthropic.Client directly implements server.Provider.
	var fallbackProvider server.Provider
	if defaultClient != nil {
		fallbackProvider = defaultClient
	}

	handler := server.New(server.Config{
		Bridge:      bridge.New(cfg, cache.NewMemoryRegistry()),
		Provider:    fallbackProvider,
		ProviderMgr: providerMgr,
		Tracer:      tracer,
		TraceErrors: errors,
		Stats:       sessionStats,
		AppConfig:   cfg,
	})

	return runHTTPServer(ctx, cfg.Addr, handler, errors, sessionStats)
}

// resolveDefaultClient returns the provider client for the default key.
// Returns nil when no default provider is configured (all models use explicit routing).
func resolveDefaultClient(pm *provider.ProviderManager, errors io.Writer) *anthropic.Client {
	if pm.DefaultKey() == "" {
		logger.Warn("no default provider configured; web search probing and server fallback disabled")
		return nil
	}
	client, err := pm.ClientForKey(pm.DefaultKey())
	if err != nil {
		logger.Warn("default provider client not available", "error", err)
		return nil
	}
	return client
}

// buildProviderDefsFromConfig converts config into provider definition map.
func buildProviderDefsFromConfig(cfg config.Config) map[string]provider.ProviderConfig {
	if len(cfg.ProviderDefs) > 0 {
		defs := make(map[string]provider.ProviderConfig, len(cfg.ProviderDefs))
		for key, def := range cfg.ProviderDefs {
			defs[key] = provider.ProviderConfig{
				BaseURL:          def.BaseURL,
				APIKey:           def.APIKey,
				Version:          def.Version,
				UserAgent:        def.UserAgent,
				Protocol:         def.Protocol,
				WebSearchSupport: string(def.WebSearchSupport),
			}
		}
		return defs
	}
	// Legacy single-provider mode.
	return provider.BuildProviderConfigs(
		cfg.ProviderBaseURL,
		cfg.ProviderAPIKey,
		cfg.ProviderVersion,
		cfg.ProviderUserAgent,
		nil,
	)
}

// buildModelRoutesFromConfig converts config model entries into route definitions.
func buildModelRoutesFromConfig(cfg config.Config) map[string]provider.ModelRoute {
	routes := make(map[string]provider.ModelRoute, len(cfg.ProviderModels))
	for alias, pm := range cfg.ProviderModels {
		providerKey := pm.Provider
		if providerKey == "" {
			providerKey = "default"
		}
		routes[alias] = provider.ModelRoute{
			Provider: providerKey,
			Name:     pm.Name,
		}
	}
	return routes
}

type webSearchProber interface {
	ProbeWebSearch(context.Context, string) (bool, error)
}

// resolvePerProviderWebSearch probes each Anthropic-protocol provider for web_search
// support and stores the resolved result in the ProviderManager.
func resolvePerProviderWebSearch(ctx context.Context, cfg config.Config, pm *provider.ProviderManager, errors io.Writer) {
	if pm == nil {
		return
	}
	for _, key := range pm.ProviderKeys() {
		// Skip non-Anthropic providers (e.g. OpenAI protocol).
		if pm.ProtocolForKey(key) != "anthropic" {
			pm.SetResolvedWebSearch(key, "disabled")
			logger.Info("web_search disabled for non-anthropic provider", "provider", key)
			continue
		}

		// Resolve the effective config: per-provider override > global.
		support := cfg.WebSearchForProvider(key)

		switch support {
		case config.WebSearchSupportDisabled:
			pm.SetResolvedWebSearch(key, "disabled")
			logger.Info("web_search disabled by config", "provider", key)
			fmt.Fprintf(errors, "web_search disabled for provider %s\n", key)
		case config.WebSearchSupportEnabled:
			pm.SetResolvedWebSearch(key, "enabled")
			logger.Info("web_search forced enabled by config", "provider", key)
		case config.WebSearchSupportInjected:
			pm.SetResolvedWebSearch(key, "injected")
			logger.Info("web_search injected mode enabled", "provider", key)
		default:
			// Auto: probe the provider.
			resolved := probeProviderWebSearch(ctx, key, pm, errors)
			pm.SetResolvedWebSearch(key, resolved)
		}
	}
}

// probeProviderWebSearch probes a single provider for web_search support.
// Returns "enabled" or "disabled".
func probeProviderWebSearch(ctx context.Context, key string, pm *provider.ProviderManager, errors io.Writer) string {
	client, err := pm.ClientForKey(key)
	if err != nil {
		logger.Warn("web_search probe skipped: client not available", "provider", key, "error", err)
		return "disabled"
	}

	upstreamModel := pm.FirstUpstreamModelForKey(key)
	if upstreamModel == "" {
		logger.Warn("web_search auto probe skipped: no model routes to provider", "provider", key)
		return "disabled"
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	supported, err := client.ProbeWebSearch(probeCtx, upstreamModel)
	if err != nil {
		logger.Warn("web_search auto probe failed", "provider", key, "error", err)
		fmt.Fprintf(errors, "web_search auto probe failed for provider %s: %v\n", key, err)
		return "disabled"
	}
	if !supported {
		logger.Warn("web_search unsupported by provider", "provider", key, "model", upstreamModel)
		fmt.Fprintf(errors, "web_search unsupported by provider %s\n", key)
		return "disabled"
	}
	logger.Info("web_search supported by provider", "provider", key, "model", upstreamModel)
	return "enabled"
}

func runCaptureResponse(ctx context.Context, cfg config.Config, errors io.Writer) error {
	tracer := mbtrace.New(captureResponseTraceConfig(cfg.TraceRequests))
	logTrace(errors, "response proxy", tracer)
	handler, err := proxy.NewResponse(proxy.ResponseConfig{
		UpstreamBaseURL: cfg.ResponseProxy.ProviderBaseURL,
		APIKey:          cfg.ResponseProxy.ProviderAPIKey,
		Tracer:          tracer,
		TraceErrors:     errors,
	})
	if err != nil {
		return err
	}
	logger.Info("response proxy initialized", "upstream", cfg.ResponseProxy.ProviderBaseURL)
	return runHTTPServer(ctx, cfg.Addr, handler, errors, nil)
}

func runCaptureAnthropic(ctx context.Context, cfg config.Config, errors io.Writer) error {
	tracer := mbtrace.New(captureAnthropicTraceConfig(cfg.TraceRequests))
	logTrace(errors, "anthropic proxy", tracer)
	handler, err := proxy.NewAnthropic(proxy.AnthropicConfig{
		UpstreamBaseURL: cfg.AnthropicProxy.ProviderBaseURL,
		APIKey:          cfg.AnthropicProxy.ProviderAPIKey,
		Version:         cfg.AnthropicProxy.ProviderVersion,
		Tracer:          tracer,
		TraceErrors:     errors,
	})
	if err != nil {
		return err
	}
	logger.Info("anthropic proxy initialized", "upstream", cfg.AnthropicProxy.ProviderBaseURL)
	return runHTTPServer(ctx, cfg.Addr, handler, errors, nil)
}

func logTrace(errors io.Writer, label string, tracer *mbtrace.Tracer) {
	if !tracer.Enabled() {
		fmt.Fprintf(errors, "%s trace disabled\n", label)
		return
	}
	logger.Info("trace enabled", "label", label, "dir", tracer.Directory())
	fmt.Fprintf(errors, "%s trace enabled at %s\n", label, tracer.Directory())
}

func transformTraceRoot() string {
	return filepath.Join(mbtrace.DefaultRoot, "Transform")
}

func captureResponseTraceConfig(enabled bool) mbtrace.Config {
	return mbtrace.Config{
		Enabled: enabled,
		Root:    filepath.Join(mbtrace.DefaultRoot, "Capture", "Response"),
	}
}

func captureAnthropicTraceConfig(enabled bool) mbtrace.Config {
	return mbtrace.Config{
		Enabled: enabled,
		Root:    filepath.Join(mbtrace.DefaultRoot, "Capture", "Anthropic"),
	}
}

func runHTTPServer(ctx context.Context, addr string, handler http.Handler, errors io.Writer, sessionStats *stats.SessionStats) error {
	httpServer := &http.Server{Addr: addr, Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(errors, "%s listening on %s\n", Name, addr)
		logger.Info("http server listening", "addr", addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		if sessionStats != nil {
			summary := sessionStats.Summary()
			logger.Info(stats.FormatSummaryLine(summary))
			fmt.Fprintln(errors)
			stats.WriteSummary(errors, summary)
		}
		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		logger.Error("http server error", "error", err)
		return err
	}
}
