package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/bridge"
	"moonbridge/internal/openai"
)

type Provider interface {
	CreateMessage(context.Context, anthropic.MessageRequest) (anthropic.MessageResponse, error)
	StreamMessage(context.Context, anthropic.MessageRequest) (anthropic.Stream, error)
}

type Config struct {
	Bridge   *bridge.Bridge
	Provider Provider
}

type Server struct {
	bridge   *bridge.Bridge
	provider Provider
	mux      *http.ServeMux
}

func New(cfg Config) *Server {
	server := &Server{bridge: cfg.Bridge, provider: cfg.Provider, mux: http.NewServeMux()}
	server.mux.HandleFunc("/v1/responses", server.handleResponses)
	server.mux.HandleFunc("/responses", server.handleResponses)
	return server
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mux.ServeHTTP(writer, request)
}

func (server *Server) handleResponses(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "method not allowed",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}

	var responsesRequest openai.ResponsesRequest
	if err := json.NewDecoder(request.Body).Decode(&responsesRequest); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "invalid JSON request body",
			Type:    "invalid_request_error",
			Code:    "invalid_json",
		}})
		return
	}

	anthropicRequest, plan, err := server.bridge.ToAnthropic(responsesRequest)
	if err != nil {
		status, payload := server.bridge.ErrorResponse(err)
		writeOpenAIError(writer, status, payload)
		return
	}

	if responsesRequest.Stream {
		server.handleStream(writer, request, responsesRequest, anthropicRequest)
		return
	}

	anthropicResponse, err := server.provider.CreateMessage(request.Context(), anthropicRequest)
	if err != nil {
		status, payload := server.bridge.ErrorResponse(err)
		writeOpenAIError(writer, status, payload)
		return
	}

	writeJSON(writer, http.StatusOK, server.bridge.FromAnthropicWithPlan(anthropicResponse, responsesRequest.Model, plan))
}

func (server *Server) handleStream(writer http.ResponseWriter, request *http.Request, responsesRequest openai.ResponsesRequest, anthropicRequest anthropic.MessageRequest) {
	stream, err := server.provider.StreamMessage(request.Context(), anthropicRequest)
	if err != nil {
		status, payload := server.bridge.ErrorResponse(err)
		writeOpenAIError(writer, status, payload)
		return
	}
	defer stream.Close()

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)

	var events []anthropic.StreamEvent
	for {
		event, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			events = append(events, anthropic.StreamEvent{Type: "error", Error: &anthropic.ErrorObject{Type: "provider_stream_error", Message: err.Error()}})
			break
		}
		events = append(events, event)
	}

	for _, event := range server.bridge.ConvertStreamEvents(events, responsesRequest.Model) {
		writeSSE(writer, event)
	}
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func writeOpenAIError(writer http.ResponseWriter, status int, payload openai.ErrorResponse) {
	writeJSON(writer, status, payload)
}

func writeSSE(writer http.ResponseWriter, event openai.StreamEvent) {
	data, _ := json.Marshal(event.Data)
	_, _ = writer.Write([]byte("event: " + event.Event + "\n"))
	_, _ = writer.Write([]byte("data: " + string(data) + "\n\n"))
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}
