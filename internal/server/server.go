package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/bridge"
	"moonbridge/internal/provider"
	"moonbridge/internal/session"
	"moonbridge/internal/stats"
	"moonbridge/internal/openai"
	"moonbridge/internal/logger"
	mbtrace "moonbridge/internal/trace"
)

// Provider defines the upstream interface for creating messages.
type Provider interface {
	CreateMessage(context.Context, anthropic.MessageRequest) (anthropic.MessageResponse, error)
	StreamMessage(context.Context, anthropic.MessageRequest) (anthropic.Stream, error)
}

type Config struct {
	Bridge       *bridge.Bridge
	Provider     Provider
	ProviderMgr  *provider.ProviderManager // optional; used for multi-provider routing
	Tracer       *mbtrace.Tracer
	TraceErrors  io.Writer
	Stats        *stats.SessionStats
}

type Server struct {
	bridge      *bridge.Bridge
	provider    Provider
	providerMgr *provider.ProviderManager
	tracer      *mbtrace.Tracer
	traceErrors io.Writer
	stats       *stats.SessionStats
	mux         *http.ServeMux
}

func New(cfg Config) *Server {
	server := &Server{
		bridge:      cfg.Bridge,
		provider:    cfg.Provider,
		providerMgr: cfg.ProviderMgr,
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
	log := logger.L().With("path", request.URL.Path, "method", request.Method, "remote", request.RemoteAddr)
	log.Debug("request received")
	if request.Method != http.MethodPost {
		log.Warn("method not allowed", "method", request.Method)
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "method not allowed",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}

	// Create a per-request session for state isolation (e.g., DeepSeek thinking cache).
	sess := session.New()

	body, err := io.ReadAll(request.Body)
	record := mbtrace.Record{HTTPRequest: mbtrace.NewHTTPRequest(request), OpenAIRequest: mbtrace.RawJSONOrString(body)}
	if err != nil {
		log.Error("failed to read request body", "error", err)
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
		log.Warn("invalid JSON request body", "error", err)
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

	anthropicRequest, plan, err := server.bridge.ToAnthropic(responsesRequest, sess)
	conversionContext := server.bridge.ConversionContext(responsesRequest)
	record.AnthropicRequest = anthropicRequest
	if err != nil {
		log.Warn("failed to convert to anthropic", "error", err)
		status, payload := server.bridge.ErrorResponse(err)
		record.Error = traceError("convert_to_anthropic", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, status, payload)
		return
	}

	// Resolve the provider for this request.
	effectiveProvider := server.resolveProvider(responsesRequest.Model, server.bridge.ProviderFor(responsesRequest.Model))

	if responsesRequest.Stream {
		log.Debug("handling streaming request", "model", responsesRequest.Model)
		server.handleStream(writer, request, responsesRequest, anthropicRequest, record, conversionContext, sess, effectiveProvider)
		return
	}

	log.Debug("sending non-streaming request to provider", "model", anthropicRequest.Model)
	anthropicResponse, err := effectiveProvider.CreateMessage(request.Context(), anthropicRequest)
	if err != nil {
		log.Error("provider create message failed", "error", err)
		status, payload := server.bridge.ErrorResponse(err)
		record.Error = traceError("provider_create_message", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, status, payload)
		return
	}

	openAIResponse := server.bridge.FromAnthropicWithPlanAndContext(anthropicResponse, responsesRequest.Model, plan, conversionContext, sess)
	usage := anthropicResponse.Usage
	if server.stats != nil {
		server.stats.Record(responsesRequest.Model, stats.Usage{
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
		})
	}
	log.Info("request completed",
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

func (server *Server) handleStream(writer http.ResponseWriter, request *http.Request, responsesRequest openai.ResponsesRequest, anthropicRequest anthropic.MessageRequest, record mbtrace.Record, context bridge.ConversionContext, sess *session.Session, provider Provider) {
	log := logger.L().With("model", responsesRequest.Model)
	log.Debug("starting stream")
	stream, err := provider.StreamMessage(request.Context(), anthropicRequest)
	if err != nil {
		log.Error("provider stream message failed", "error", err)
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
			log.Error("stream read error", "error", err)
			break
		}
		events = append(events, event)
	}

	openAIEvents := server.bridge.ConvertStreamEventsWithContext(events, responsesRequest.Model, context, sess)
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
	log.Info("stream completed",
		"model", responsesRequest.Model,
		"events", len(openAIEvents),
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"cache_creation", usage.CacheCreationInputTokens,
		"cache_read", usage.CacheReadInputTokens,
		"session_cache_hit_rate", server.stats.CacheHitRate(),
	)
}

// resolveProvider selects the correct Provider for a given model alias.
// If a ProviderManager is configured, it uses it for routing.
// Otherwise it falls back to the single default provider.
func (server *Server) resolveProvider(modelAlias string, providerKey string) Provider {
	if server.providerMgr != nil {
		client, err := server.providerMgr.ClientForKey(providerKey)
		if err == nil && client != nil {
			return &anthropicClientWrapper{client: client}
		}
		// Fallback: try the first available client.
		for _, k := range server.providerMgr.ProviderKeys() {
			c, err := server.providerMgr.ClientForKey(k)
			if err == nil && c != nil {
				return &anthropicClientWrapper{client: c}
			}
		}
	}
	if server.provider != nil {
		return server.provider
	}
	return nil
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

// anthropicClientWrapper adapts *anthropic.Client to the Provider interface.
type anthropicClientWrapper struct {
	client *anthropic.Client
}

func (w *anthropicClientWrapper) CreateMessage(ctx context.Context, request anthropic.MessageRequest) (anthropic.MessageResponse, error) {
	return w.client.CreateMessage(ctx, request)
}

func (w *anthropicClientWrapper) StreamMessage(ctx context.Context, request anthropic.MessageRequest) (anthropic.Stream, error) {
	return w.client.StreamMessage(ctx, request)
}
