package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/bridge"
	"moonbridge/internal/cache"
	"moonbridge/internal/config"
	"moonbridge/internal/logger"
	"moonbridge/internal/provider"
	"moonbridge/internal/server"
	"moonbridge/internal/stats"
	mbtrace "moonbridge/internal/trace"
)

type fakeProvider struct {
	request      anthropic.MessageRequest
	streamEvents []anthropic.StreamEvent
}

func (provider *fakeProvider) CreateMessage(_ context.Context, request anthropic.MessageRequest) (anthropic.MessageResponse, error) {
	provider.request = request
	return anthropic.MessageResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       "assistant",
		StopReason: "end_turn",
		Content:    []anthropic.ContentBlock{{Type: "text", Text: "Hello from provider"}},
		Usage:      anthropic.Usage{InputTokens: 4, OutputTokens: 3},
	}, nil
}

func (provider *fakeProvider) StreamMessage(context.Context, anthropic.MessageRequest) (anthropic.Stream, error) {
	return &sliceStream{events: provider.streamEvents}, nil
}

type sliceStream struct {
	events []anthropic.StreamEvent
	index  int
}

func (stream *sliceStream) Next() (anthropic.StreamEvent, error) {
	if stream.index >= len(stream.events) {
		return anthropic.StreamEvent{}, io.EOF
	}
	event := stream.events[stream.index]
	stream.index++
	return event, nil
}

func (stream *sliceStream) Close() error {
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestResponsesHandlerReturnsOpenAIResponse(t *testing.T) {
	provider := &fakeProvider{}
	var logOutput bytes.Buffer
	if err := logger.Init(logger.Config{Level: logger.LevelInfo, Format: "text", Output: &logOutput}); err != nil {
		t.Fatalf("logger.Init() error = %v", err)
	}
	t.Cleanup(func() {
		_ = logger.Init(logger.Config{Level: logger.LevelInfo, Format: "text", Output: os.Stderr})
	})
	handler := server.New(server.Config{
		Bridge: bridge.New(config.Config{
			DefaultMaxTokens: 1024,
			ModelMap:         map[string]string{"gpt-test": "claude-test"},
			Cache:            config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry()),
		Provider: provider,
	})

	requestBody := bytes.NewBufferString(`{"model":"gpt-test","input":"Hello"}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", requestBody)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if provider.request.Model != "claude-test" {
		t.Fatalf("provider model = %q", provider.request.Model)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("Unmarshal response error = %v", err)
	}
	if response["object"] != "response" || response["output_text"] != "Hello from provider" {
		t.Fatalf("response = %+v", response)
	}
	if !strings.Contains(logOutput.String(), "claude-test Usage:") {
		t.Fatalf("log should use forwarded model, got: %s", logOutput.String())
	}
}

func TestResponsesHandlerWritesTraceFile(t *testing.T) {
	traceRoot := t.TempDir()
	provider := &fakeProvider{}
	handler := server.New(server.Config{
		Bridge: bridge.New(config.Config{
			DefaultMaxTokens: 1024,
			ModelMap:         map[string]string{"gpt-test": "claude-test"},
			Cache:            config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry()),
		Provider: provider,
		Tracer:   mbtrace.New(mbtrace.Config{Enabled: true, Root: traceRoot, SessionID: "session-test"}),
	})

	requestBody := bytes.NewBufferString(`{"model":"gpt-test","input":"Hello trace debug"}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", requestBody)
	request.Header.Set("Authorization", "Bearer client-api-key")

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	responseData, err := os.ReadFile(filepath.Join(traceRoot, "session-test", "Response", "1.json"))
	if err != nil {
		t.Fatalf("ReadFile(response trace) error = %v", err)
	}
	responseContent := string(responseData)
	if strings.Contains(responseContent, "client-api-key") {
		t.Fatalf("response trace leaked API key: %s", responseContent)
	}
	for _, want := range []string{
		`"request_number": 1`,
		`"openai_request"`,
		"Hello trace debug",
		`"openai_response"`,
		"[REDACTED]",
	} {
		if !strings.Contains(responseContent, want) {
			t.Fatalf("response trace missing %q: %s", want, responseContent)
		}
	}
	for _, notWant := range []string{`"anthropic_request"`, `"anthropic_response"`} {
		if strings.Contains(responseContent, notWant) {
			t.Fatalf("response trace should not contain %q: %s", notWant, responseContent)
		}
	}

	anthropicData, err := os.ReadFile(filepath.Join(traceRoot, "session-test", "Anthropic", "1.json"))
	if err != nil {
		t.Fatalf("ReadFile(anthropic trace) error = %v", err)
	}
	anthropicContent := string(anthropicData)
	for _, want := range []string{
		`"request_number": 1`,
		`"anthropic_request"`,
		"claude-test",
		`"anthropic_response"`,
		"Hello from provider",
	} {
		if !strings.Contains(anthropicContent, want) {
			t.Fatalf("anthropic trace missing %q: %s", want, anthropicContent)
		}
	}
	for _, notWant := range []string{`"openai_request"`, `"openai_response"`} {
		if strings.Contains(anthropicContent, notWant) {
			t.Fatalf("anthropic trace should not contain %q: %s", notWant, anthropicContent)
		}
	}
}

func TestResponsesHandlerAcceptsCodexResponsesPath(t *testing.T) {
	provider := &fakeProvider{}
	handler := server.New(server.Config{
		Bridge: bridge.New(config.Config{
			DefaultMaxTokens: 1024,
			ModelMap:         map[string]string{"gpt-test": "claude-test"},
			Cache:            config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry()),
		Provider: provider,
	})

	requestBody := bytes.NewBufferString(`{"model":"gpt-test","input":"Hello"}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/responses", requestBody)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesHandlerRejectsUnsupportedToolType(t *testing.T) {
	handler := server.New(server.Config{
		Bridge: bridge.New(config.Config{
			DefaultMaxTokens: 1024,
			Cache:            config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry()),
		Provider: &fakeProvider{},
	})

	requestBody := bytes.NewBufferString(`{"model":"gpt-test","input":"Hello","tools":[{"type":"unknown_tool"}]}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", requestBody)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("unsupported_parameter")) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestResponsesHandlerStreamsOpenAIEvents(t *testing.T) {
	provider := &fakeProvider{
		streamEvents: []anthropic.StreamEvent{
			{Type: "message_start", Message: &anthropic.MessageResponse{ID: "msg_1", Type: "message", Role: "assistant"}},
			{Type: "content_block_start", Index: 0, ContentBlock: &anthropic.ContentBlock{Type: "text"}},
			{Type: "content_block_delta", Index: 0, Delta: anthropic.StreamDelta{Type: "text_delta", Text: "Hi"}},
			{Type: "content_block_stop", Index: 0},
			{Type: "message_delta", Delta: anthropic.StreamDelta{StopReason: "end_turn"}},
			{Type: "message_stop"},
		},
	}
	handler := server.New(server.Config{
		Bridge: bridge.New(config.Config{
			DefaultMaxTokens: 1024,
			Cache:            config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry()),
		Provider: provider,
	})

	requestBody := bytes.NewBufferString(`{"model":"gpt-test","input":"Hello","stream":true}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", requestBody)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type = %q", got)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("event: response.output_text.delta")) {
		t.Fatalf("stream body = %s", recorder.Body.String())
	}
}

func TestResponsesHandlerPassesOpenAIProtocolThroughWithUpstreamModel(t *testing.T) {
	var upstreamRequest struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/responses" {
			t.Fatalf("upstream path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer openai-key" {
			t.Fatalf("authorization = %q", got)
		}
		if err := json.NewDecoder(request.Body).Decode(&upstreamRequest); err != nil {
			t.Fatalf("Decode upstream request error = %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_123","object":"response","status":"completed","output":[],"usage":{"input_tokens":1200000,"output_tokens":500000,"input_tokens_details":{"cached_tokens":200000}}}`)),
		}, nil
	})}

	providerMgr, err := provider.NewProviderManager(map[string]provider.ProviderConfig{
		"default": {
			BaseURL: "https://anthropic.example.test",
			APIKey:  "anthropic-key",
		},
		"openai": {
			BaseURL:  "https://openai.example.test",
			APIKey:   "openai-key",
			Protocol: "openai",
		},
	}, map[string]provider.ModelRoute{
		"image": {Provider: "openai", Name: "gpt-image-1.5"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager() error = %v", err)
	}
	var logOutput bytes.Buffer
	if err := logger.Init(logger.Config{Level: logger.LevelInfo, Format: "text", Output: &logOutput}); err != nil {
		t.Fatalf("logger.Init() error = %v", err)
	}
	t.Cleanup(func() {
		_ = logger.Init(logger.Config{Level: logger.LevelInfo, Format: "text", Output: os.Stderr})
	})
	sessionStats := stats.NewSessionStats()
	sessionStats.SetPricing(map[string]stats.ModelPricing{
		"image": {
			InputPrice:     1,
			OutputPrice:    2,
			CacheReadPrice: 0.2,
		},
	})
	sessionStats.Record("image", stats.Usage{InputTokens: 1_000_000})
	handler := server.New(server.Config{
		Bridge: bridge.New(config.Config{
			ProviderModels: map[string]config.ProviderModelConfig{
				"image": {Provider: "openai", Name: "gpt-image-1.5"},
			},
			Cache: config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry()),
		ProviderMgr:      providerMgr,
		OpenAIHTTPClient: httpClient,
		Stats:            sessionStats,
	})

	requestBody := bytes.NewBufferString(`{"model":"image","input":"draw"}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", requestBody)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamRequest.Model != "gpt-image-1.5" {
		t.Fatalf("upstream model = %q", upstreamRequest.Model)
	}
	if upstreamRequest.Input != "draw" {
		t.Fatalf("upstream input = %q", upstreamRequest.Input)
	}
	summary := sessionStats.Summary()
	if summary.Requests != 2 || summary.InputTokens != 2_000_000 || summary.CacheRead != 200_000 || summary.OutputTokens != 500_000 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.TotalCost < 3.039999 || summary.TotalCost > 3.040001 {
		t.Fatalf("TotalCost = %f, want 3.04", summary.TotalCost)
	}
	for _, want := range []string{
		"gpt-image-1.5 Usage:",
		"1.200000 M Input",
		"0.500000 M Output",
		"Billing: 3.04 CNY",
	} {
		if !strings.Contains(logOutput.String(), want) {
			t.Fatalf("log missing %q: %s", want, logOutput.String())
		}
	}
}
