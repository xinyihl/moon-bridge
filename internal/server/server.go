package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/bridge"
	"moonbridge/internal/cache"
	"moonbridge/internal/config"
	"moonbridge/internal/extensions/websearchinjected"
	"moonbridge/internal/logger"
	"moonbridge/internal/openai"
	"moonbridge/internal/provider"
	"moonbridge/internal/session"
	"moonbridge/internal/stats"
	mbtrace "moonbridge/internal/trace"
)

// Provider defines the upstream interface for creating messages.
type Provider interface {
	CreateMessage(context.Context, anthropic.MessageRequest) (anthropic.MessageResponse, error)
	StreamMessage(context.Context, anthropic.MessageRequest) (anthropic.Stream, error)
}

type Config struct {
	Bridge           *bridge.Bridge
	Provider         Provider
	ProviderMgr      *provider.ProviderManager // optional; used for multi-provider routing
	OpenAIHTTPClient *http.Client
	Tracer           *mbtrace.Tracer
	TraceErrors      io.Writer
	Stats            *stats.SessionStats
	AppConfig        config.Config // full app config for per-provider resolution
}

type Server struct {
	bridge      *bridge.Bridge
	provider    Provider
	providerMgr *provider.ProviderManager
	openAIHTTP  *http.Client
	tracer      *mbtrace.Tracer
	traceErrors io.Writer
	stats       *stats.SessionStats
	mux         *http.ServeMux
	sessionsMu  sync.Mutex
	sessions    map[string]serverSession
	appConfig   config.Config
}

type serverSession struct {
	sess     *session.Session
	lastUsed time.Time
}

const sessionTTL = 24 * time.Hour

func New(cfg Config) *Server {
	server := &Server{
		bridge:      cfg.Bridge,
		provider:    cfg.Provider,
		providerMgr: cfg.ProviderMgr,
		openAIHTTP:  cfg.OpenAIHTTPClient,
		tracer:      cfg.Tracer,
		traceErrors: cfg.TraceErrors,
		stats:       cfg.Stats,
		mux:         http.NewServeMux(),
		sessions:    map[string]serverSession{},
		appConfig:   cfg.AppConfig,
	}
	server.mux.HandleFunc("/v1/responses", server.handleResponses)
	server.mux.HandleFunc("/responses", server.handleResponses)
	server.mux.HandleFunc("/v1/models", server.handleModels)
	server.mux.HandleFunc("/models", server.handleModels)
	return server
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mux.ServeHTTP(writer, request)
}


// ModelInfo represents a model entry in the OpenAI /v1/models response.
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (server *Server) handleModels(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "only GET requests are supported",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}
	models := server.listModels()
	resp := struct {
		Object string      `json:"object"`
		Data   []ModelInfo `json:"data"`
	}{
		Object: "list",
		Data:   models,
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(resp)
}


func (server *Server) listModels() []ModelInfo {
	seen := make(map[string]bool)
	var models []ModelInfo
	now := time.Now().Unix()

	// Direct "provider/upstream_model" entries from provider definitions first.
	for providerKey, def := range server.appConfig.ProviderDefs {
		for modelName := range def.Models {
			id := providerKey + "/" + modelName
			if seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, ModelInfo{
				ID:      id,
				Object:  "model",
				Created: now,
				OwnedBy: providerKey,
			})
		}
	}

	// Route aliases (processed second so they won't be shadowed by direct entries).
	for alias, route := range server.appConfig.Routes {
		if seen[alias] {
			continue
		}
		seen[alias] = true
		ownedBy := "system"
		if route.Provider != "" {
			ownedBy = route.Provider
		}
		models = append(models, ModelInfo{
			ID:      alias,
			Object:  "model",
			Created: now,
			OwnedBy: ownedBy,
		})
	}

	return models
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

	sess := server.sessionForRequest(request)

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

	// Check if this model routes to an OpenAI-protocol provider (skip Anthropic conversion).
	providerKey := server.bridge.ProviderFor(responsesRequest.Model)
	if server.providerMgr != nil && server.providerMgr.ProtocolForModel(responsesRequest.Model) == "openai" {
		server.handleOpenAIResponse(writer, request, responsesRequest, providerKey, record)
		return
	}

	// Resolve per-provider web search mode.
	reqOpts := server.resolveRequestOptions(responsesRequest.Model, providerKey)

	anthropicRequest, plan, err := server.bridge.ToAnthropic(responsesRequest, sess, reqOpts)
	conversionContext := server.bridge.ConversionContext(responsesRequest)
	record.AnthropicRequest = anthropicRequest
	if err != nil {
		log.Warn("failed to convert to anthropic", "error", err)
		status, payload := server.bridge.ErrorResponseForModel(responsesRequest.Model, err)
		record.Error = traceError("convert_to_anthropic", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, status, payload)
		return
	}

	// Resolve the provider for this request.
	effectiveProvider := server.resolveProvider(responsesRequest.Model, server.bridge.ProviderFor(responsesRequest.Model))
	if effectiveProvider == nil {
		log.Error("no provider available for model", "model", responsesRequest.Model)
		writeOpenAIError(writer, http.StatusBadGateway, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("no upstream provider configured for model %q", responsesRequest.Model),
			Type:    "server_error",
			Code:    "provider_error",
		}})
		return
	}

	if responsesRequest.Stream {
		log.Debug("handling streaming request", "model", responsesRequest.Model)
		server.handleStream(writer, request, responsesRequest, anthropicRequest, plan, record, conversionContext, sess, effectiveProvider)
		return
	}

	log.Debug("sending non-streaming request to provider", "model", anthropicRequest.Model)
	anthropicResponse, err := effectiveProvider.CreateMessage(request.Context(), anthropicRequest)
	if err != nil {
		status, payload := server.bridge.ErrorResponseForModel(responsesRequest.Model, err)
		errLine := stats.FormatErrorLine(stats.ErrorLineParams{
			RequestModel: responsesRequest.Model,
			ActualModel:  anthropicRequest.Model,
			StatusCode:   status,
			Message:      payload.Error.Message,
		})
		fmt.Fprintln(logger.Output(), errLine)
		log.Error("provider request failed", "status", status)
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
	logUsageLine(responsesRequest.Model, anthropicRequest.Model, stats.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CacheCreationInputTokens: usage.CacheCreationInputTokens, CacheReadInputTokens: usage.CacheReadInputTokens}, server.stats)
	record.AnthropicResponse = anthropicResponse
	record.OpenAIResponse = openAIResponse
	server.writeTrace(record)
	writeJSON(writer, http.StatusOK, openAIResponse)
}

func (server *Server) handleStream(writer http.ResponseWriter, request *http.Request, responsesRequest openai.ResponsesRequest, anthropicRequest anthropic.MessageRequest, plan cache.CacheCreationPlan, record mbtrace.Record, context bridge.ConversionContext, sess *session.Session, provider Provider) {
	log := logger.L().With("model", responsesRequest.Model)
	log.Debug("starting stream")
	stream, err := provider.StreamMessage(request.Context(), anthropicRequest)
	if err != nil {
		status, payload := server.bridge.ErrorResponseForModel(responsesRequest.Model, err)
		errLine := stats.FormatErrorLine(stats.ErrorLineParams{
			RequestModel: responsesRequest.Model,
			ActualModel:  anthropicRequest.Model,
			StatusCode:   status,
			Message:      payload.Error.Message,
		})
		fmt.Fprintln(logger.Output(), errLine)
		log.Error("provider stream failed", "status", status)
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

	openAIEvents := server.bridge.ConvertStreamEventsWithContext(events, responsesRequest.Model, context, sess, bridge.StreamOptions{
		PersistFinalTextReasoning: hasToolHistory(anthropicRequest.Messages),
	})
	record.AnthropicStreamEvents = events
	record.OpenAIStreamEvents = openAIEvents
	server.writeTrace(record)

	for _, event := range openAIEvents {
		writeSSE(writer, event)
	}
	// Extract usage from message_delta event
	var usage anthropic.Usage
	for _, ev := range events {
		switch {
		case ev.Type == "message_start" && ev.Message != nil:
			// message_start carries cache_creation / cache_read token counts
			usage.InputTokens = ev.Message.Usage.InputTokens
			usage.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
			usage.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
		case ev.Type == "message_delta" && ev.Usage != nil:
			usage.OutputTokens = ev.Usage.OutputTokens
			// message_delta may also carry cache fields in some providers
			if ev.Usage.CacheCreationInputTokens > 0 {
				usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
			}
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
	logUsageLine(responsesRequest.Model, anthropicRequest.Model, stats.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CacheCreationInputTokens: usage.CacheCreationInputTokens, CacheReadInputTokens: usage.CacheReadInputTokens}, server.stats)
	// Update cache registry from streaming usage signals.
	server.bridge.UpdateRegistryFromUsage(plan, cache.UsageSignals{
		InputTokens:              usage.InputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
	}, usage.InputTokens)
}

// resolveProvider selects the correct Provider for a given model alias.
// If a ProviderManager is configured, it uses it for routing.
// Otherwise it falls back to the single default provider.

func (server *Server) sessionForRequest(request *http.Request) *session.Session {
	key := sessionKeyFromRequest(request)
	if key == "" {
		return session.New()
	}

	now := time.Now()
	server.sessionsMu.Lock()
	defer server.sessionsMu.Unlock()

	server.pruneSessionsLocked(now)
	if entry, ok := server.sessions[key]; ok {
		entry.lastUsed = now
		server.sessions[key] = entry
		return entry.sess
	}

	sess := session.NewWithID(key)
	server.sessions[key] = serverSession{sess: sess, lastUsed: now}
	return sess
}

func (server *Server) pruneSessionsLocked(now time.Time) {
	for key, entry := range server.sessions {
		if now.Sub(entry.lastUsed) > sessionTTL {
			delete(server.sessions, key)
		}
	}
}

func sessionKeyFromRequest(request *http.Request) string {
	if value := strings.TrimSpace(request.Header.Get("Session_id")); value != "" {
		return "session:" + value
	}
	if value := strings.TrimSpace(request.Header.Get("X-Codex-Window-Id")); value != "" {
		return "codex-window:" + value
	}
	return ""
}

func hasToolHistory(messages []anthropic.Message) bool {
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == "tool_use" || block.Type == "tool_result" {
				return true
			}
		}
	}
	return false
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

// handleOpenAIResponse proxies a request directly to an OpenAI-compatible upstream
// without Anthropic protocol conversion. It handles both streaming and non-streaming.
func (server *Server) handleOpenAIResponse(writer http.ResponseWriter, request *http.Request, responsesRequest openai.ResponsesRequest, providerKey string, record mbtrace.Record) {
	log := logger.L().With("path", request.URL.Path, "method", request.Method)
	if server.providerMgr == nil {
		log.Error("no provider manager configured for openai protocol")
		writeOpenAIError(writer, http.StatusBadGateway, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "provider routing not configured",
			Type:    "server_error",
			Code:    "internal_error",
		}})
		return
	}

	// Resolve provider key from model alias when Bridge.ProviderFor returned empty.
	resolvedKey := providerKey
	if resolvedKey == "" {
		resolvedKey = server.providerMgr.ProviderKeyForModel(responsesRequest.Model)
	}
	baseURL := server.providerMgr.ProviderBaseURL(resolvedKey)
	apiKey := server.providerMgr.ProviderAPIKey(resolvedKey)
	if baseURL == "" {
		log.Error("openai provider has no base_url", "provider", providerKey)
		writeOpenAIError(writer, http.StatusBadGateway, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "provider not configured",
			Type:    "server_error",
			Code:    "internal_error",
		}})
		return
	}

	// Build upstream URL: baseURL + /v1/responses
	upstreamURL := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(upstreamURL, "/v1/responses") && !strings.HasSuffix(upstreamURL, "/responses") {
		upstreamURL += "/v1/responses"
	}

	upstreamRequest := responsesRequest
	upstreamRequest.Model = server.providerMgr.UpstreamModelFor(responsesRequest.Model)

	body, err := json.Marshal(upstreamRequest)
	if err != nil {
		log.Error("failed to marshal request", "error", err)
		writeOpenAIError(writer, http.StatusInternalServerError, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "internal error",
			Type:    "server_error",
			Code:    "internal_error",
		}})
		return
	}

	// Create upstream request
	upstreamReq, err := http.NewRequestWithContext(request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		log.Error("failed to create upstream request", "error", err)
		writeOpenAIError(writer, http.StatusBadGateway, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "upstream request failed",
			Type:    "server_error",
			Code:    "internal_error",
		}})
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := server.openAIHTTP
	if client == nil {
		client = &http.Client{Timeout: 0} // no timeout for streaming
	}
	upstreamResp, err := client.Do(upstreamReq)
	if err != nil {
		errLine := stats.FormatErrorLine(stats.ErrorLineParams{
			RequestModel: responsesRequest.Model,
			ActualModel:  upstreamRequest.Model,
			StatusCode:   http.StatusBadGateway,
			Message:      err.Error(),
		})
		fmt.Fprintln(logger.Output(), errLine)
		log.Error("openai upstream failed", "status", http.StatusBadGateway)
		record.Error = map[string]string{"stage": "openai_upstream", "message": err.Error()}
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: err.Error(),
			Type:    "server_error",
			Code:    "provider_error",
		}})
		return
	}
	defer upstreamResp.Body.Close()

	// Copy response headers and status
	for key, values := range upstreamResp.Header {
		for _, v := range values {
			writer.Header().Add(key, v)
		}
	}
	writer.WriteHeader(upstreamResp.StatusCode)

	var captured bytes.Buffer
	target := io.Writer(writer)
	if server.stats != nil && upstreamResp.StatusCode >= 200 && upstreamResp.StatusCode <= 299 {
		target = io.MultiWriter(writer, &captured)
	}
	if _, err := io.Copy(target, upstreamResp.Body); err != nil {
		log.Error("copy upstream response failed", "error", err)
		return
	}
	if usage, ok := openAIUsageFromResponse(captured.Bytes(), responsesRequest.Stream); ok {
		server.stats.Record(responsesRequest.Model, usage)
		logUsageLine(responsesRequest.Model, upstreamRequest.Model, usage, server.stats)
	}
}

func logUsageLine(requestModel, actualModel string, usage stats.Usage, sessionStats *stats.SessionStats) {
	var requestCost, totalCost, hitRate, writeRate float64
	if sessionStats != nil {
		requestCost = sessionStats.ComputeCost(requestModel, usage)
		totalCost = sessionStats.Summary().TotalCost
		hitRate = sessionStats.CacheHitRate()
		writeRate = sessionStats.CacheWriteRate()
	}
	fmt.Fprintln(logger.Output(), stats.FormatUsageLine(stats.UsageLineParams{
		RequestModel:   requestModel,
		ActualModel:    actualModel,
		Usage:          usage,
		RequestCost:    requestCost,
		TotalCost:      totalCost,
		CacheHitRate:   hitRate,
		CacheWriteRate: writeRate,
	}))
}

func openAIUsageFromResponse(data []byte, stream bool) (stats.Usage, bool) {
	if len(data) == 0 {
		return stats.Usage{}, false
	}
	if stream {
		return openAIUsageFromSSE(data)
	}
	var payload struct {
		Usage openai.Usage `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return stats.Usage{}, false
	}
	return statsUsageFromOpenAIUsage(payload.Usage)
}

func openAIUsageFromSSE(data []byte) (stats.Usage, bool) {
	var last stats.Usage
	found := false
	for _, event := range strings.Split(string(data), "\n\n") {
		var payload strings.Builder
		for _, line := range strings.Split(event, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				part := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if part == "" || part == "[DONE]" {
					continue
				}
				if payload.Len() > 0 {
					payload.WriteByte('\n')
				}
				payload.WriteString(part)
			}
		}
		if payload.Len() == 0 {
			continue
		}
		var envelope struct {
			Usage    openai.Usage `json:"usage"`
			Response struct {
				Usage openai.Usage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload.String()), &envelope); err != nil {
			continue
		}
		if usage, ok := statsUsageFromOpenAIUsage(envelope.Response.Usage); ok {
			last = usage
			found = true
			continue
		}
		if usage, ok := statsUsageFromOpenAIUsage(envelope.Usage); ok {
			last = usage
			found = true
		}
	}
	return last, found
}

func statsUsageFromOpenAIUsage(usage openai.Usage) (stats.Usage, bool) {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.InputTokensDetails.CachedTokens == 0 {
		return stats.Usage{}, false
	}
	cacheRead := usage.InputTokensDetails.CachedTokens
	freshInput := usage.InputTokens - cacheRead
	if freshInput < 0 {
		freshInput = 0
	}
	return stats.Usage{
		InputTokens:          freshInput,
		OutputTokens:         usage.OutputTokens,
		CacheReadInputTokens: cacheRead,
	}, true
}
func (server *Server) resolveProvider(modelAlias string, providerKey string) Provider {
	if server.providerMgr != nil {
		// First, try routing by model alias.
		if _, client, err := server.providerMgr.ClientFor(modelAlias); err == nil && client != nil {
			return server.maybeWrapInjectedSearch(client, modelAlias, providerKey)
		}
		// Fallback: try providerKey directly.
		if providerKey != "" {
			if client, err := server.providerMgr.ClientForKey(providerKey); err == nil && client != nil {
				return server.maybeWrapInjectedSearch(client, modelAlias, providerKey)
			}
		}
		// Last resort: try any available provider.
		for _, k := range server.providerMgr.ProviderKeys() {
			if c, err := server.providerMgr.ClientForKey(k); err == nil && c != nil {
				return server.maybeWrapInjectedSearch(c, modelAlias, k)
			}
		}
	}
	if server.provider != nil {
		return server.provider
	}
	return nil
}

// maybeWrapInjectedSearch wraps a client with the injected search orchestrator
// if the resolved web search mode for this provider is "injected".
func (server *Server) maybeWrapInjectedSearch(client *anthropic.Client, modelAlias string, providerKey string) Provider {
	key := providerKey
	if key == "" && server.providerMgr != nil {
		key = server.providerMgr.ProviderKeyForModel(modelAlias)
	}
	if server.providerMgr != nil && server.providerMgr.ResolvedWebSearch(key) == "injected" {
		tavilyKey := server.appConfig.WebSearchTavilyKeyForProvider(key)
		firecrawlKey := server.appConfig.WebSearchFirecrawlKeyForProvider(key)
		maxRounds := server.appConfig.WebSearchMaxRoundsForProvider(key)
		logger.L().Debug("wrapping provider with injected search orchestrator", "provider", key)
		return websearchinjected.WrapProvider(client, tavilyKey, firecrawlKey, maxRounds)
	}
	return &anthropicClientWrapper{client: client}
}

// resolveRequestOptions builds per-request bridge options based on the provider's
// resolved web search support.
func (server *Server) resolveRequestOptions(modelAlias string, providerKey string) bridge.RequestOptions {
	if server.providerMgr == nil {
		return bridge.RequestOptions{}
	}
	key := providerKey
	if key == "" {
		key = server.providerMgr.ProviderKeyForModel(modelAlias)
	}
	wsMode := server.providerMgr.ResolvedWebSearch(key)
	if wsMode == "" {
		return bridge.RequestOptions{}
	}
	return bridge.RequestOptions{
		WebSearchMode:    wsMode,
		WebSearchMaxUses: server.appConfig.WebSearchMaxUsesForProvider(key),
		FirecrawlAPIKey:  server.appConfig.WebSearchFirecrawlKeyForProvider(key),
	}
}
