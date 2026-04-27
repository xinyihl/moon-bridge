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
	deepseekv4 "moonbridge/internal/extensions/deepseek_v4"
	"moonbridge/internal/logger"
	"moonbridge/internal/plugin"
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

func (provider *fakeProvider) StreamMessage(_ context.Context, request anthropic.MessageRequest) (anthropic.Stream, error) {
	provider.request = request
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
			Routes:           map[string]config.RouteEntry{"gpt-test": {Provider: "default", Model: "claude-test"}},
			Cache:            config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry(), nil),
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
	logStr := logOutput.String()
	if !strings.Contains(logStr, "模型: gpt-test ➡️ claude-test") {
		t.Fatalf("log should contain model routing, got: %s", logStr)
	}
}

func TestResponsesHandlerWritesTraceFile(t *testing.T) {
	traceRoot := t.TempDir()
	provider := &fakeProvider{}
	handler := server.New(server.Config{
		Bridge: bridge.New(config.Config{
			DefaultMaxTokens: 1024,
			Routes:           map[string]config.RouteEntry{"gpt-test": {Provider: "default", Model: "claude-test"}},
			Cache:            config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry(), nil),
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
			Routes:           map[string]config.RouteEntry{"gpt-test": {Provider: "default", Model: "claude-test"}},
			Cache:            config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry(), nil),
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

func TestBuildModelInfoFromRouteEnablesApplyPatchFreeform(t *testing.T) {
	info := server.BuildModelInfoFromRoute("gpt-test", "default", config.RouteEntry{
		DisplayName: "GPT Test",
	})

	if info.ApplyPatchToolType == nil || *info.ApplyPatchToolType != "freeform" {
		t.Fatalf("apply_patch_tool_type = %v", info.ApplyPatchToolType)
	}
	if info.TruncationPolicy.Mode != "tokens" || info.TruncationPolicy.Limit != server.DefaultCatalogTruncationLimit {
		t.Fatalf("truncation_policy = %+v", info.TruncationPolicy)
	}
}

func TestBuildModelInfoFromRouteUsesTokenTruncationPolicyForGPT52(t *testing.T) {
	info := server.BuildModelInfoFromRoute("gpt-5.2", "default", config.RouteEntry{
		DisplayName: "GPT 5.2",
	})

	if info.TruncationPolicy.Mode != "tokens" || info.TruncationPolicy.Limit != server.DefaultCatalogTruncationLimit {
		t.Fatalf("truncation_policy = %+v", info.TruncationPolicy)
	}
}

func TestBuildModelInfosFromConfigIncludesProviderModelsBeforeRouteFallback(t *testing.T) {
	models := server.BuildModelInfosFromConfig(config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"p1": {
				Models: map[string]config.ModelMeta{
					"model-b": {DisplayName: "Model B", ContextWindow: 2000},
					"model-a": {DisplayName: "Model A", ContextWindow: 1000},
				},
			},
			"p2": {
				Models: map[string]config.ModelMeta{
					"model-c": {DisplayName: "Model C", ContextWindow: 3000},
				},
			},
		},
		Routes: map[string]config.RouteEntry{
			"alias-a":    {Provider: "p1", Model: "model-a", DisplayName: "Alias A"},
			"p1/model-a": {Provider: "p1", Model: "model-a", DisplayName: "Duplicate Direct"},
		},
	})

	var slugs []string
	for _, model := range models {
		slugs = append(slugs, model.Slug)
	}
	want := []string{"p1/model-a", "p1/model-b", "p2/model-c", "alias-a"}
	if strings.Join(slugs, ",") != strings.Join(want, ",") {
		t.Fatalf("slugs = %v, want %v", slugs, want)
	}
	if models[0].DisplayName != "Model A(p1)" || models[0].ContextWindow == nil || *models[0].ContextWindow != 1000 {
		t.Fatalf("provider metadata not preserved: %+v", models[0])
	}
}

func TestBuildModelInfoPreservesReasoningLevelsForDeepSeekV4(t *testing.T) {
	info := server.BuildModelInfoFromProviderModel("deepseek/deepseek-v4-pro", "deepseek", config.ModelMeta{
		DeepSeekV4:            true,
		DefaultReasoningLevel: "high",
		SupportedReasoningLevels: []config.ReasoningLevelPreset{
			{Effort: "high", Description: "High reasoning effort"},
			{Effort: "xhigh", Description: "Extra high reasoning effort"},
		},
	})

	if info.DefaultReasoningLevel != "high" {
		t.Fatalf("DefaultReasoningLevel = %q, want high", info.DefaultReasoningLevel)
	}
	if len(info.SupportedReasoningLevels) != 2 {
		t.Fatalf("SupportedReasoningLevels = %+v, want two levels", info.SupportedReasoningLevels)
	}
	if info.SupportedReasoningLevels[0].Effort != "high" || info.SupportedReasoningLevels[1].Effort != "xhigh" {
		t.Fatalf("SupportedReasoningLevels = %+v", info.SupportedReasoningLevels)
	}
}

func TestResponsesHandlerRejectsUnsupportedToolType(t *testing.T) {
	handler := server.New(server.Config{
		Bridge: bridge.New(config.Config{
			DefaultMaxTokens: 1024,
			Cache:            config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry(), nil),
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
		}, cache.NewMemoryRegistry(), nil),
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
	if bytes.Contains(recorder.Body.Bytes(), []byte("Collecting from upstream")) || bytes.Contains(recorder.Body.Bytes(), []byte(`"phase":"commentary"`)) {
		t.Fatalf("stream body contains synthetic commentary preamble: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("event: response.output_text.delta")) {
		t.Fatalf("stream body = %s", recorder.Body.String())
	}
}

func TestResponsesHandlerReusesCodexSessionForDeepSeekThinking(t *testing.T) {
	provider := &fakeProvider{
		streamEvents: []anthropic.StreamEvent{
			{Type: "message_start", Message: &anthropic.MessageResponse{ID: "msg_1", Type: "message", Role: "assistant"}},
			{Type: "content_block_start", Index: 0, ContentBlock: &anthropic.ContentBlock{Type: "thinking"}},
			{Type: "content_block_delta", Index: 0, Delta: anthropic.StreamDelta{Type: "thinking_delta", Thinking: "inspect before listing"}},
			{Type: "content_block_delta", Index: 0, Delta: anthropic.StreamDelta{Type: "signature_delta", Signature: "sig_1"}},
			{Type: "content_block_stop", Index: 0},
			{Type: "content_block_start", Index: 1, ContentBlock: &anthropic.ContentBlock{Type: "tool_use", ID: "call_ls", Name: "exec_command", Input: json.RawMessage(`{}`)}},
			{Type: "content_block_delta", Index: 1, Delta: anthropic.StreamDelta{Type: "input_json_delta", PartialJSON: `{"cmd":"ls"}`}},
			{Type: "content_block_stop", Index: 1},
			{Type: "message_delta", Delta: anthropic.StreamDelta{StopReason: "tool_use"}},
			{Type: "message_stop"},
		},
	}
	cfg := config.Config{
		DefaultMaxTokens: 1024,
		Routes:           map[string]config.RouteEntry{"gpt-test": {Provider: "default", Model: "deepseek-v4-pro", DeepSeekV4: true}},
		ProviderDefs:     map[string]config.ProviderDef{"default": {}},
		Cache:            config.CacheConfig{Mode: "off"},
	}
	plugins := plugin.NewRegistry(nil)
	plugins.Register(deepseekv4.NewPlugin(cfg.DeepSeekV4ForModel))
	handler := server.New(server.Config{
		Bridge:   bridge.New(cfg, cache.NewMemoryRegistry(), plugins),
		Provider: provider,
		Plugins:  plugins,
	})

	firstRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-test","input":"inspect","stream":true}`))
	firstRequest.Header.Set("Session_id", "codex-session-1")
	firstRecorder := httptest.NewRecorder()

	handler.ServeHTTP(firstRecorder, firstRequest)

	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", firstRecorder.Code, firstRecorder.Body.String())
	}

	secondRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{
		"model":"gpt-test",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"inspect"}],"type":"message"},
			{"arguments":"{\"cmd\":\"ls\"}","call_id":"call_ls","name":"exec_command","type":"function_call"},
			{"call_id":"call_ls","output":"README.md\n","type":"function_call_output"}
		]
	}`))
	secondRequest.Header.Set("Session_id", "codex-session-1")
	secondRecorder := httptest.NewRecorder()

	handler.ServeHTTP(secondRecorder, secondRequest)

	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", secondRecorder.Code, secondRecorder.Body.String())
	}
	if len(provider.request.Messages) != 3 {
		t.Fatalf("provider messages = %+v", provider.request.Messages)
	}
	assistant := provider.request.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 2 {
		t.Fatalf("assistant message = %+v", assistant)
	}
	if assistant.Content[0].Type != "thinking" || assistant.Content[0].Thinking != "inspect before listing" || assistant.Content[0].Signature != "sig_1" {
		t.Fatalf("thinking block = %+v", assistant.Content[0])
	}
	if assistant.Content[1].Type != "tool_use" || assistant.Content[1].ID != "call_ls" {
		t.Fatalf("tool use block = %+v", assistant.Content[1])
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
	sessionStats.Record("image", "", stats.Usage{InputTokens: 1_000_000})
	handler := server.New(server.Config{
		Bridge: bridge.New(config.Config{
			Routes: map[string]config.RouteEntry{
				"image": {Provider: "openai", Model: "gpt-image-1.5"},
			},
			Cache: config.CacheConfig{Mode: "off"},
		}, cache.NewMemoryRegistry(), nil),
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
		"模型: image ➡️ gpt-image-1.5",
		"输出: 0.5000 M",
		"累计 3.0400 元",
	} {
		if !strings.Contains(logOutput.String(), want) {
			t.Fatalf("log missing %q: %s", want, logOutput.String())
		}
	}
}
