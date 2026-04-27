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
	deepseekv4 "moonbridge/internal/extensions/deepseek_v4"
	"moonbridge/internal/logger"
	"moonbridge/internal/plugin"
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
	return "欢迎使用 " + Name + "!"
}

func RunServer(ctx context.Context, cfg config.Config, errors io.Writer) error {
	switch cfg.Mode {
	case config.ModeTransform:
		logger.Info("启动服务器", "mode", cfg.Mode, "addr", cfg.Addr)
		return runTransform(ctx, cfg, errors)
	case config.ModeCaptureResponse:
		logger.Info("启动服务器", "mode", cfg.Mode, "addr", cfg.Addr)
		return runCaptureResponse(ctx, cfg, errors)
	case config.ModeCaptureAnthropic:
		logger.Info("启动服务器", "mode", cfg.Mode, "addr", cfg.Addr)
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
	// Set per-model pricing from routes and provider/model direct references
	pricing := make(map[string]stats.ModelPricing)
	for alias, route := range cfg.Routes {
		if route.InputPrice > 0 || route.OutputPrice > 0 || route.CacheWritePrice > 0 || route.CacheReadPrice > 0 {
			pricing[alias] = stats.ModelPricing{
				InputPrice:      route.InputPrice,
				OutputPrice:     route.OutputPrice,
				CacheWritePrice: route.CacheWritePrice,
				CacheReadPrice:  route.CacheReadPrice,
			}
		}
	}
	// Also index pricing by provider/model slug for direct references.
	for providerKey, def := range cfg.ProviderDefs {
		for modelName, meta := range def.Models {
			slug := providerKey + "/" + modelName
			if _, exists := pricing[slug]; exists {
				continue // route alias already has pricing (may differ from model meta)
			}
			if meta.InputPrice > 0 || meta.OutputPrice > 0 || meta.CacheWritePrice > 0 || meta.CacheReadPrice > 0 {
				pricing[slug] = stats.ModelPricing{
					InputPrice:      meta.InputPrice,
					OutputPrice:     meta.OutputPrice,
					CacheWritePrice: meta.CacheWritePrice,
					CacheReadPrice:  meta.CacheReadPrice,
				}
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

	// Register plugins.
	plugins := plugin.NewRegistry(nil)
	plugins.Register(deepseekv4.NewPlugin(cfg.DeepSeekV4ForModel))
	if err := plugins.InitAll(nil); err != nil {
		return fmt.Errorf("init plugins: %w", err)
	}
	defer plugins.ShutdownAll()

	handler := server.New(server.Config{
		Bridge:      bridge.New(cfg, cache.NewMemoryRegistry(), plugins),
		Provider:    fallbackProvider,
		ProviderMgr: providerMgr,
		Tracer:      tracer,
		TraceErrors: errors,
		Stats:       sessionStats,
		AppConfig:   cfg,
		Plugins:     plugins,
	})

	return runHTTPServer(ctx, cfg.Addr, handler, errors, sessionStats)
}

// resolveDefaultClient returns the provider client for the default key.
// Returns nil when no default provider is configured (all models use explicit routing).
func resolveDefaultClient(pm *provider.ProviderManager, errors io.Writer) *anthropic.Client {
	if pm.DefaultKey() == "" {
		logger.Warn("未配置默认提供商，禁用网页搜索探测和服务器回退")
		return nil
	}
	client, err := pm.ClientForKey(pm.DefaultKey())
	if err != nil {
		logger.Warn("默认提供商客户端不可用", "error", err)
		return nil
	}
	return client
}

// buildProviderDefsFromConfig converts config into provider definition map.
func buildProviderDefsFromConfig(cfg config.Config) map[string]provider.ProviderConfig {
	if len(cfg.ProviderDefs) > 0 {
		defs := make(map[string]provider.ProviderConfig, len(cfg.ProviderDefs))
		for key, def := range cfg.ProviderDefs {
			modelNames := make([]string, 0, len(def.Models))
			for name := range def.Models {
				modelNames = append(modelNames, name)
			}
			defs[key] = provider.ProviderConfig{
				BaseURL:          def.BaseURL,
				APIKey:           def.APIKey,
				Version:          def.Version,
				UserAgent:        def.UserAgent,
				Protocol:         def.Protocol,
				WebSearchSupport: string(def.WebSearchSupport),
				ModelNames:       modelNames,
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
	routes := make(map[string]provider.ModelRoute, len(cfg.Routes))
	for alias, route := range cfg.Routes {
		routes[alias] = provider.ModelRoute{
			Provider: route.Provider,
			Name:     route.Model,
		}
	}
	return routes
}

type webSearchProber interface {
	ProbeWebSearch(context.Context, string) (bool, error)
}

// resolvePerProviderWebSearch resolves web_search support for each provider and
// each model that has a model-level override.
func resolvePerProviderWebSearch(ctx context.Context, cfg config.Config, pm *provider.ProviderManager, errors io.Writer) {
	if pm == nil {
		return
	}
	// 1. Resolve provider-level defaults.
	for _, key := range pm.ProviderKeys() {
		if pm.ProtocolForKey(key) != "anthropic" {
			pm.SetResolvedWebSearch(key, "disabled")
			logger.Info("非 Anthropic 提供商禁用网页搜索", "provider", key)
			continue
		}
		support := cfg.WebSearchForProvider(key)
		switch support {
		case config.WebSearchSupportDisabled:
			pm.SetResolvedWebSearch(key, "disabled")
			logger.Info("配置禁用网页搜索", "provider", key)
		case config.WebSearchSupportEnabled:
			pm.SetResolvedWebSearch(key, "enabled")
			logger.Info("配置强制启用网页搜索", "provider", key)
		case config.WebSearchSupportInjected:
			pm.SetResolvedWebSearch(key, "injected")
			logger.Info("网页搜索注入模式已启用", "provider", key)
		default:
			resolved := probeProviderWebSearch(ctx, key, pm, errors)
			pm.SetResolvedWebSearch(key, resolved)
		}
	}
	// 2. Resolve model-level overrides for provider catalog slugs and route aliases.
	for providerKey, def := range cfg.ProviderDefs {
		providerWS := cfg.WebSearchForProvider(providerKey)
		for modelName := range def.Models {
			alias := providerKey + "/" + modelName
			resolveModelWebSearch(ctx, alias, cfg.WebSearchForModel(alias), providerWS, pm, errors)
		}
	}
	for alias, route := range cfg.Routes {
		modelWS := cfg.WebSearchForModel(alias)
		providerWS := cfg.WebSearchForProvider(route.Provider)
		resolveModelWebSearch(ctx, alias, modelWS, providerWS, pm, errors)
	}
}

func resolveModelWebSearch(ctx context.Context, alias string, modelWS config.WebSearchSupport, providerWS config.WebSearchSupport, pm *provider.ProviderManager, errors io.Writer) {
	if modelWS == providerWS {
		return // no model-level override, provider resolution applies
	}
	modelKey := "model:" + alias
	switch modelWS {
	case config.WebSearchSupportDisabled:
		pm.SetResolvedWebSearch(modelKey, "disabled")
		logger.Info("模型配置禁用网页搜索", "model", alias)
	case config.WebSearchSupportEnabled:
		pm.SetResolvedWebSearch(modelKey, "enabled")
		logger.Info("模型配置强制启用网页搜索", "model", alias)
	case config.WebSearchSupportInjected:
		pm.SetResolvedWebSearch(modelKey, "injected")
		logger.Info("模型配置启用网页搜索注入模式", "model", alias)
	default:
		// Auto: probe using this model's upstream name.
		resolved := probeModelWebSearch(ctx, alias, pm, errors)
		pm.SetResolvedWebSearch(modelKey, resolved)
	}
}

// probeProviderWebSearch probes a single provider for web_search support.
// Returns "enabled" or "disabled".
func probeProviderWebSearch(ctx context.Context, key string, pm *provider.ProviderManager, errors io.Writer) string {
	client, err := pm.ClientForKey(key)
	if err != nil {
		logger.Warn("网页搜索探测跳过：客户端不可用", "provider", key, "error", err)
		return "disabled"
	}

	upstreamModel := pm.FirstUpstreamModelForKey(key)
	if upstreamModel == "" {
		logger.Warn("网页搜索自动探测跳过：无模型路由到提供商", "provider", key)
		return "disabled"
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	supported, err := client.ProbeWebSearch(probeCtx, upstreamModel)
	if err != nil {
		logger.Warn("网页搜索自动探测失败", "provider", key, "error", err)
		fmt.Fprintf(errors, "网页搜索自动探测失败（提供商 %s）: %v\n", key, err)
		return "disabled"
	}
	if !supported {
		logger.Warn("提供商不支持网页搜索", "provider", key, "model", upstreamModel)
		fmt.Fprintf(errors, "提供商 %s 不支持网页搜索\n", key)
		return "disabled"
	}
	logger.Info("提供商支持网页搜索", "provider", key, "model", upstreamModel)
	return "enabled"
}

// probeModelWebSearch probes a specific model alias for web_search support.
func probeModelWebSearch(ctx context.Context, modelAlias string, pm *provider.ProviderManager, errors io.Writer) string {
	upstreamModel, client, err := pm.ClientFor(modelAlias)
	if err != nil {
		logger.Warn("网页搜索模型探测跳过：客户端不可用", "model", modelAlias, "error", err)
		return "disabled"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	supported, err := client.ProbeWebSearch(probeCtx, upstreamModel)
	if err != nil {
		logger.Warn("网页搜索模型探测失败", "model", modelAlias, "error", err)
		fmt.Fprintf(errors, "网页搜索模型探测失败（%s）: %v\n", modelAlias, err)
		return "disabled"
	}
	if !supported {
		logger.Warn("模型不支持网页搜索", "model", modelAlias)
		fmt.Fprintf(errors, "模型 %s 不支持网页搜索\n", modelAlias)
		return "disabled"
	}
	logger.Info("模型支持网页搜索", "model", modelAlias)
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
	logger.Info("响应代理已初始化", "upstream", cfg.ResponseProxy.ProviderBaseURL)
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
	logger.Info("Anthropic 代理已初始化", "upstream", cfg.AnthropicProxy.ProviderBaseURL)
	return runHTTPServer(ctx, cfg.Addr, handler, errors, nil)
}

func logTrace(errors io.Writer, label string, tracer *mbtrace.Tracer) {
	if !tracer.Enabled() {
		fmt.Fprintf(errors, "%s 跟踪已禁用\n", label)
		return
	}
	logger.Info("跟踪已启用", "label", label, "dir", tracer.Directory())
	fmt.Fprintf(errors, "%s 跟踪已启用于 %s\n", label, tracer.Directory())
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
		fmt.Fprintf(errors, "%s 监听于 %s\n", Name, addr)
		logger.Info("HTTP 服务器监听中", "addr", addr)
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
		logger.Error("HTTP 服务器错误", "error", err)
		return err
	}
}
