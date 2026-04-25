package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/bridge"
	"moonbridge/internal/cache"
	"moonbridge/internal/config"
	"moonbridge/internal/server"
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

func TestResponsesHandlerReturnsOpenAIResponse(t *testing.T) {
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

	requestBody := bytes.NewBufferString(`{"model":"gpt-test","input":"Hello","tools":[{"type":"web_search_preview"}]}`)
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
