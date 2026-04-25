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
	"moonbridge/internal/extensions/websearchinjected"
	"moonbridge/internal/config"
	"moonbridge/internal/logger"
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
	anthropicClient := anthropic.NewClient(anthropic.ClientConfig{
		BaseURL:   cfg.ProviderBaseURL,
		APIKey:    cfg.ProviderAPIKey,
		Version:   cfg.ProviderVersion,
		UserAgent: cfg.ProviderUserAgent,
	})
	cfg = resolveWebSearchSupport(ctx, cfg, anthropicClient, errors)
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
	// Wrap provider with injected search orchestrator extension when configured.
	var provider server.Provider = anthropicClientWrapper{client: anthropicClient}
	if websearchinjected.IsEnabled(cfg) {
		provider = websearchinjected.WrapProvider(anthropicClient, cfg.TavilyAPIKey, cfg.FirecrawlAPIKey, cfg.SearchMaxRounds)
		logger.Info("injected web search enabled", "tavily", cfg.TavilyAPIKey != "", "firecrawl", cfg.FirecrawlAPIKey != "")
	}
	handler := server.New(server.Config{
		Bridge:      bridge.New(cfg, cache.NewMemoryRegistry()),
		Provider:    provider,
		Tracer:      tracer,
		TraceErrors: errors,
		Stats:       sessionStats,
	})

	return runHTTPServer(ctx, cfg.Addr, handler, errors, sessionStats)
}

type webSearchProber interface {
	ProbeWebSearch(context.Context, string) (bool, error)
}

func resolveWebSearchSupport(ctx context.Context, cfg config.Config, prober webSearchProber, errors io.Writer) config.Config {
	switch cfg.WebSearchSupport {
	case config.WebSearchSupportDisabled:
		logger.Info("web_search disabled by config")
		fmt.Fprintln(errors, "web_search disabled by config")
		return cfg
	case config.WebSearchSupportEnabled:
		logger.Info("web_search forced enabled by config")
		return cfg
	case config.WebSearchSupportInjected:
		logger.Info("web_search injected mode enabled, search executed server-side via Tavily/Firecrawl")
		return cfg
	}

	model := cfg.WebSearchProbeModel()
	if model == "" {
		cfg.DisableWebSearch()
		logger.Warn("web_search auto probe skipped: no default model alias; tool injection disabled")
		return cfg
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	supported, err := prober.ProbeWebSearch(probeCtx, model)
	if err != nil {
		cfg.DisableWebSearch()
		logger.Warn("web_search auto probe failed; tool injection disabled", "error", err)
		fmt.Fprintf(errors, "web_search auto probe failed; tool injection disabled: %v\n", err)
		return cfg
	}
	if !supported {
		cfg.DisableWebSearch()
		logger.Warn("web_search unsupported by provider; tool injection disabled", "model", model)
		fmt.Fprintln(errors, "web_search unsupported by provider; tool injection disabled")
		return cfg
	}
	logger.Info("web_search supported by provider", "model", model)
	return cfg
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
			logger.Info("session summary", "stats", summary)
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

type anthropicClientWrapper struct {
	client *anthropic.Client
}

func (wrapper anthropicClientWrapper) CreateMessage(ctx context.Context, request anthropic.MessageRequest) (anthropic.MessageResponse, error) {
	return wrapper.client.CreateMessage(ctx, request)
}

func (wrapper anthropicClientWrapper) StreamMessage(ctx context.Context, request anthropic.MessageRequest) (anthropic.Stream, error) {
	return wrapper.client.StreamMessage(ctx, request)
}
