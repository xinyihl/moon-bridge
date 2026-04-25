package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/bridge"
	"moonbridge/internal/cache"
	"moonbridge/internal/config"
	"moonbridge/internal/proxy"
	"moonbridge/internal/server"
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
		return runTransform(ctx, cfg, errors)
	case config.ModeCaptureResponse:
		return runCaptureResponse(ctx, cfg, errors)
	case config.ModeCaptureAnthropic:
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
	tracer := mbtrace.New(mbtrace.Config{
		Enabled: cfg.TraceRequests,
		Root:    transformTraceRoot(),
	})
	logTrace(errors, "transform", tracer)
	handler := server.New(server.Config{
		Bridge:      bridge.New(cfg, cache.NewMemoryRegistry()),
		Provider:    anthropicClientWrapper{client: anthropicClient},
		Tracer:      tracer,
		TraceErrors: errors,
	})

	return runHTTPServer(ctx, cfg.Addr, handler, errors)
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
	return runHTTPServer(ctx, cfg.Addr, handler, errors)
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
	return runHTTPServer(ctx, cfg.Addr, handler, errors)
}

func logTrace(errors io.Writer, label string, tracer *mbtrace.Tracer) {
	if !tracer.Enabled() {
		fmt.Fprintf(errors, "%s trace disabled\n", label)
		return
	}
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

func runHTTPServer(ctx context.Context, addr string, handler http.Handler, errors io.Writer) error {
	httpServer := &http.Server{Addr: addr, Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(errors, "%s listening on %s\n", Name, addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
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
