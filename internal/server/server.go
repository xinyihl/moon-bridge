package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/bridge"
	"moonbridge/internal/stats"
	"moonbridge/internal/openai"
	"moonbridge/internal/logger"
	mbtrace "moonbridge/internal/trace"
)

type Provider interface {
	CreateMessage(context.Context, anthropic.MessageRequest) (anthropic.MessageResponse, error)
	StreamMessage(context.Context, anthropic.MessageRequest) (anthropic.Stream, error)
}

type Config struct {
	Bridge      *bridge.Bridge
	Provider    Provider
	Tracer      *mbtrace.Tracer
	TraceErrors io.Writer
	Stats       *stats.SessionStats
}

type Server struct {
	bridge      *bridge.Bridge
	provider    Provider
	tracer      *mbtrace.Tracer
	traceErrors io.Writer
	stats       *stats.SessionStats
	mux         *http.ServeMux
}

func New(cfg Config) *Server {
	server := &Server{
		bridge:      cfg.Bridge,
		provider:    cfg.Provider,
		tracer:      cfg.Tracer,
		traceErrors: cfg.TraceErrors,
		stats:       cfg.Stats,
		mux:         http.NewServeMux(),
	}
	server.mux.HandleFunc("/v1/responses", server.handleResponses)
	server.mux.HandleFunc("/responses", server.handleResponses)
	return server
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mux.ServeHTTP(writer, request)
}

func (server *Server) handleResponses(writer http.ResponseWriter, request *http.Request) {
	logger := logger.L().With("path", request.URL.Path, "method", request.Method, "remote", request.RemoteAddr)
	logger.Debug("request received")
	if request.Method != http.MethodPost {
		logger.Warn("method not allowed", "method", request.Method)
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "method not allowed",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}

	body, err := io.ReadAll(request.Body)
	record := mbtrace.Record{HTTPRequest: mbtrace.NewHTTPRequest(request), OpenAIRequest: mbtrace.RawJSONOrString(body)}
	if err != nil {
		logger.Error("failed to read request body", "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "failed to read request body",
			Type:    "invalid_request_error",
			Code:    "invalid_request_body",
		}}
		record.Error = traceError("read_openai_request", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadRequest, payload)
		return
	}

	var responsesRequest openai.ResponsesRequest
	if err := json.Unmarshal(body, &responsesRequest); err != nil {
		logger.Warn("invalid JSON request body", "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "invalid JSON request body",
			Type:    "invalid_request_error",
			Code:    "invalid_json",
		}}
		record.Error = traceError("decode_openai_request", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadRequest, payload)
		return
	}

	anthropicRequest, plan, err := server.bridge.ToAnthropic(responsesRequest)
	conversionContext := server.bridge.ConversionContext(responsesRequest)
	record.AnthropicRequest = anthropicRequest
	if err != nil {
		logger.Warn("failed to convert to anthropic", "error", err)
		status, payload := server.bridge.ErrorResponse(err)
		record.Error = traceError("convert_to_anthropic", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, status, payload)
		return
	}

	if responsesRequest.Stream {
		logger.Debug("handling streaming request", "model", responsesRequest.Model)
		server.handleStream(writer, request, responsesRequest, anthropicRequest, record)
		return
	}

	logger.Debug("sending non-streaming request to provider", "model", anthropicRequest.Model)
	anthropicResponse, err := server.provider.CreateMessage(request.Context(), anthropicRequest)
	if err != nil {
		logger.Error("provider create message failed", "error", err)
		status, payload := server.bridge.ErrorResponse(err)
		record.Error = traceError("provider_create_message", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, status, payload)
		return
	}

	openAIResponse := server.bridge.FromAnthropicWithPlanAndContext(anthropicResponse, responsesRequest.Model, plan, conversionContext)
	usage := anthropicResponse.Usage
	if server.stats != nil {
		server.stats.Record(responsesRequest.Model, stats.Usage{
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
		})
	}
	logger.Info("request completed",
		"model", responsesRequest.Model,
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"cache_creation", usage.CacheCreationInputTokens,
		"cache_read", usage.CacheReadInputTokens,
		"session_cache_hit_rate", server.stats.CacheHitRate(),
	)
	record.AnthropicResponse = anthropicResponse
	record.OpenAIResponse = openAIResponse
	server.writeTrace(record)
	writeJSON(writer, http.StatusOK, openAIResponse)
}

func (server *Server) handleStream(writer http.ResponseWriter, request *http.Request, responsesRequest openai.ResponsesRequest, anthropicRequest anthropic.MessageRequest, record mbtrace.Record) {
	logger := logger.L().With("model", responsesRequest.Model)
	logger.Debug("starting stream")
	stream, err := server.provider.StreamMessage(request.Context(), anthropicRequest)
	if err != nil {
		logger.Error("provider stream message failed", "error", err)
		status, payload := server.bridge.ErrorResponse(err)
		record.Error = traceError("provider_stream_message", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
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
			record.Error = traceError("provider_stream_next", err)
			logger.Error("stream read error", "error", err)
			break
		}
		events = append(events, event)
	}

	openAIEvents := server.bridge.ConvertStreamEventsWithContext(events, responsesRequest.Model, server.bridge.ConversionContext(responsesRequest))
	record.AnthropicStreamEvents = events
	record.OpenAIStreamEvents = openAIEvents
	server.writeTrace(record)

	for _, event := range openAIEvents {
		writeSSE(writer, event)
	}
	// Extract usage from message_delta event
	var usage anthropic.Usage
	for _, event := range events {
		if event.Type == "message_delta" && event.Usage != nil {
			usage = *event.Usage
		}
	}
	if server.stats != nil {
		server.stats.Record(responsesRequest.Model, stats.Usage{
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
		})
	}
	logger.Info("stream completed",
		"model", responsesRequest.Model,
		"events", len(openAIEvents),
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"cache_creation", usage.CacheCreationInputTokens,
		"cache_read", usage.CacheReadInputTokens,
		"session_cache_hit_rate", server.stats.CacheHitRate(),
	)
}

func (server *Server) writeTrace(record mbtrace.Record) {
	if server.tracer == nil || !server.tracer.Enabled() {
		return
	}
	requestNumber := server.tracer.NextRequestNumber()
	if shouldWriteResponseTrace(record) {
		server.writeTraceCategory("Response", requestNumber, mbtrace.Record{
			HTTPRequest:        record.HTTPRequest,
			OpenAIRequest:      record.OpenAIRequest,
			OpenAIResponse:     record.OpenAIResponse,
			OpenAIStreamEvents: record.OpenAIStreamEvents,
			Error:              record.Error,
		})
	}
	if shouldWriteAnthropicTrace(record) {
		server.writeTraceCategory("Anthropic", requestNumber, mbtrace.Record{
			HTTPRequest:           record.HTTPRequest,
			AnthropicRequest:      record.AnthropicRequest,
			AnthropicResponse:     record.AnthropicResponse,
			AnthropicStreamEvents: record.AnthropicStreamEvents,
			Error:                 record.Error,
		})
	}
}

func (server *Server) writeTraceCategory(category string, requestNumber uint64, record mbtrace.Record) {
	if _, err := server.tracer.WriteNumbered(category, requestNumber, record); err != nil && server.traceErrors != nil {
		fmt.Fprintf(server.traceErrors, "trace %s write failed: %v\n", category, err)
	}
}

func shouldWriteResponseTrace(record mbtrace.Record) bool {
	return record.OpenAIRequest != nil || record.OpenAIResponse != nil || record.OpenAIStreamEvents != nil
}

func shouldWriteAnthropicTrace(record mbtrace.Record) bool {
	return record.AnthropicRequest != nil || record.AnthropicResponse != nil || record.AnthropicStreamEvents != nil
}

func traceError(stage string, err error) map[string]string {
	return map[string]string{"stage": stage, "message": err.Error()}
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
