package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	"moonbridge/internal/plugin"
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
	Plugins          *plugin.Registry
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
	plugins     *plugin.Registry
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
		plugins:     cfg.Plugins,
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
	Slug                        string                    `json:"slug"`
	DisplayName                 string                    `json:"display_name"`
	Description                 string                    `json:"description,omitempty"`
	DefaultReasoningLevel       string                    `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels    []ReasoningLevelPresetDTO `json:"supported_reasoning_levels"`
	ShellType                   string                    `json:"shell_type"`
	Visibility                  string                    `json:"visibility"`
	SupportedInAPI              bool                      `json:"supported_in_api"`
	Priority                    int                       `json:"priority"`
	AdditionalSpeedTiers        []string                  `json:"additional_speed_tiers"`
	AvailabilityNux             *ModelAvailabilityNux     `json:"availability_nux"`
	Upgrade                     *ModelInfoUpgrade         `json:"upgrade"`
	BaseInstructions            string                    `json:"base_instructions"`
	SupportsReasoningSummaries  bool                      `json:"supports_reasoning_summaries"`
	DefaultReasoningSummary     string                    `json:"default_reasoning_summary"`
	SupportVerbosity            bool                      `json:"support_verbosity"`
	DefaultVerbosity            *string                   `json:"default_verbosity"`
	ApplyPatchToolType          *string                   `json:"apply_patch_tool_type"`
	WebSearchToolType           string                    `json:"web_search_tool_type"`
	TruncationPolicy            TruncationPolicyConfig    `json:"truncation_policy"`
	SupportsParallelToolCalls   bool                      `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal bool                      `json:"supports_image_detail_original"`
	ContextWindow               *int                      `json:"context_window,omitempty"`
	MaxContextWindow            *int                      `json:"max_context_window,omitempty"`
	AutoCompactTokenLimit       *int                      `json:"auto_compact_token_limit,omitempty"`
	EffectiveContextWindowPct   int                       `json:"effective_context_window_percent"`
	ExperimentalSupportedTools  []string                  `json:"experimental_supported_tools"`
	InputModalities             []string                  `json:"input_modalities"`
	SupportsSearchTool          bool                      `json:"supports_search_tool"`
}

// ModelAvailabilityNux is a placeholder for Codex model availability nux.
type ModelAvailabilityNux struct{}

// ModelInfoUpgrade is a placeholder for Codex model upgrade info.
type ModelInfoUpgrade struct{}

// TruncationPolicyConfig matches Codex's truncation_policy field.
type TruncationPolicyConfig struct {
	Mode  string `json:"mode"`
	Limit int64  `json:"limit"`
}

const (
	defaultApplyPatchToolType = "freeform"
	// DefaultCatalogTruncationLimit keeps shell tool output from being clamped
	// to zero while using a consistent token policy across generated models.
	DefaultCatalogTruncationLimit int64 = 10000
)

// ReasoningLevelPresetDTO is the JSON shape Codex expects for reasoning presets.
type ReasoningLevelPresetDTO struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

func (server *Server) handleModels(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "仅支持 GET 请求",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}
	models := server.listModels()
	resp := struct {
		Models []ModelInfo `json:"models"`
	}{
		Models: models,
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(resp)
}

func (server *Server) listModels() []ModelInfo {
	return BuildModelInfosFromConfig(server.appConfig)
}

func (server *Server) handleResponses(writer http.ResponseWriter, request *http.Request) {
	log := logger.L().With("path", request.URL.Path, "method", request.Method, "remote", request.RemoteAddr)
	log.Debug("收到请求")
	if request.Method != http.MethodPost {
		log.Warn("方法不允许", "method", request.Method)
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "方法不允许",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}

	sess := server.sessionForRequest(request)

	body, err := io.ReadAll(request.Body)
	record := mbtrace.Record{HTTPRequest: mbtrace.NewHTTPRequest(request), OpenAIRequest: mbtrace.RawJSONOrString(body)}
	if err != nil {
		log.Error("读取请求体失败", "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "读取请求体失败",
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
		log.Warn("无效的 JSON 请求体", "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "无效的 JSON 请求体",
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
		log.Warn("转换为 Anthropic 格式失败", "error", err)
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
		log.Error("模型无可用提供商", "model", responsesRequest.Model)
		writeOpenAIError(writer, http.StatusBadGateway, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("no upstream provider configured for model %q", responsesRequest.Model),
			Type:    "server_error",
			Code:    "provider_error",
		}})
		return
	}

	if responsesRequest.Stream {
		log.Debug("处理流式请求", "model", responsesRequest.Model)
		server.handleStream(writer, request, responsesRequest, anthropicRequest, plan, record, conversionContext, sess, effectiveProvider)
		return
	}

	log.Debug("发送非流式请求到提供商", "model", anthropicRequest.Model)
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
		log.Error("提供商请求失败", "status", status)
		record.Error = traceError("provider_create_message", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, status, payload)
		return
	}

	openAIResponse := server.bridge.FromAnthropicWithPlanAndContext(anthropicResponse, responsesRequest.Model, plan, conversionContext, sess)
	usage := anthropicResponse.Usage
	if server.stats != nil {
		server.stats.Record(responsesRequest.Model, anthropicRequest.Model, stats.Usage{
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
	log.Debug("开始流式传输")
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
		log.Error("提供商流式传输失败", "status", status)
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
			log.Error("流式读取错误", "error", err)
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
		server.stats.Record(responsesRequest.Model, anthropicRequest.Model, stats.Usage{
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
		sess := session.New()
		sess.InitExtensions(server.plugins.NewSessionData())
		return sess
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
	sess.InitExtensions(server.plugins.NewSessionData())
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
		fmt.Fprintf(server.traceErrors, "跟踪 %s 写入失败: %v\n", category, err)
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
		log.Error("未配置 OpenAI 协议的提供商管理器")
		writeOpenAIError(writer, http.StatusBadGateway, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "提供商路由未配置",
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
		log.Error("OpenAI 提供商缺少 base_url", "provider", providerKey)
		writeOpenAIError(writer, http.StatusBadGateway, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "提供商未配置",
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
		log.Error("序列化请求失败", "error", err)
		writeOpenAIError(writer, http.StatusInternalServerError, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "内部错误",
			Type:    "server_error",
			Code:    "internal_error",
		}})
		return
	}

	// Create upstream request
	upstreamReq, err := http.NewRequestWithContext(request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		log.Error("创建上游请求失败", "error", err)
		writeOpenAIError(writer, http.StatusBadGateway, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "上游请求失败",
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
		log.Error("OpenAI 上游请求失败", "status", http.StatusBadGateway)
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
		log.Error("复制上游响应失败", "error", err)
		return
	}
	if usage, ok := openAIUsageFromResponse(captured.Bytes(), responsesRequest.Stream); ok {
		server.stats.Record(responsesRequest.Model, upstreamRequest.Model, usage)
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
// if the resolved web search mode for this model/provider is "injected".
func (server *Server) maybeWrapInjectedSearch(client *anthropic.Client, modelAlias string, providerKey string) Provider {
	if server.providerMgr == nil {
		return &anthropicClientWrapper{client: client}
	}
	resolved := server.providerMgr.ResolvedWebSearchForModel(modelAlias)
	if resolved == "injected" {
		tavilyKey := server.appConfig.WebSearchTavilyKeyForModel(modelAlias)
		firecrawlKey := server.appConfig.WebSearchFirecrawlKeyForModel(modelAlias)
		maxRounds := server.appConfig.WebSearchMaxRoundsForModel(modelAlias)
		logger.L().Debug("包装注入式搜索编排器", "model", modelAlias)
		return websearchinjected.WrapProvider(client, tavilyKey, firecrawlKey, maxRounds)
	}
	return &anthropicClientWrapper{client: client}
}

// resolveRequestOptions builds per-request bridge options based on the provider's
// resolved web search support. Uses model-level resolution.
func (server *Server) resolveRequestOptions(modelAlias string, providerKey string) bridge.RequestOptions {
	if server.providerMgr == nil {
		return bridge.RequestOptions{}
	}
	wsMode := server.providerMgr.ResolvedWebSearchForModel(modelAlias)
	if wsMode == "" {
		return bridge.RequestOptions{}
	}
	return bridge.RequestOptions{
		WebSearchMode:    wsMode,
		WebSearchMaxUses: server.appConfig.WebSearchMaxUsesForModel(modelAlias),
		FirecrawlAPIKey:  server.appConfig.WebSearchFirecrawlKeyForModel(modelAlias),
	}
}

// BuildModelInfoFromRoute creates a Codex-compatible ModelInfo from a route entry.
func BuildModelInfoFromRoute(alias string, ownedBy string, route config.RouteEntry) ModelInfo {
	displayName := route.DisplayName
	if displayName == "" {
		displayName = alias
	}
	displayName = displayName + "(" + ownedBy + ")"
	return newModelInfo(alias, displayName, route.Description, route.ContextWindow,
		route.DefaultReasoningLevel, route.SupportedReasoningLevels,
		route.SupportsReasoningSummaries, route.DefaultReasoningSummary)
}

// BuildModelInfoFromProviderModel creates a Codex-compatible ModelInfo from a
// provider model catalog entry.
func BuildModelInfoFromProviderModel(slug string, ownedBy string, meta config.ModelMeta) ModelInfo {
	displayName := meta.DisplayName
	if displayName == "" {
		displayName = slug
	}
	displayName = displayName + "(" + ownedBy + ")"
	return newModelInfo(slug, displayName, meta.Description, meta.ContextWindow,
		meta.DefaultReasoningLevel, meta.SupportedReasoningLevels,
		meta.SupportsReasoningSummaries, meta.DefaultReasoningSummary)
}

// BuildModelInfosFromConfig returns Codex model catalog entries. Provider model
// catalogs are the primary source; routes are appended as fallback aliases.
func BuildModelInfosFromConfig(cfg config.Config) []ModelInfo {
	seen := make(map[string]bool)
	var models []ModelInfo

	providerKeys := make([]string, 0, len(cfg.ProviderDefs))
	for key := range cfg.ProviderDefs {
		providerKeys = append(providerKeys, key)
	}
	sort.Strings(providerKeys)
	for _, providerKey := range providerKeys {
		def := cfg.ProviderDefs[providerKey]
		modelNames := make([]string, 0, len(def.Models))
		for name := range def.Models {
			modelNames = append(modelNames, name)
		}
		sort.Strings(modelNames)
		for _, name := range modelNames {
			slug := providerKey + "/" + name
			if seen[slug] {
				continue
			}
			seen[slug] = true
			models = append(models, BuildModelInfoFromProviderModel(slug, providerKey, def.Models[name]))
		}
	}

	routeAliases := make([]string, 0, len(cfg.Routes))
	for alias := range cfg.Routes {
		routeAliases = append(routeAliases, alias)
	}
	sort.Strings(routeAliases)
	for _, alias := range routeAliases {
		if seen[alias] {
			continue
		}
		seen[alias] = true
		route := cfg.Routes[alias]
		ownedBy := "system"
		if route.Provider != "" {
			ownedBy = route.Provider
		}
		models = append(models, BuildModelInfoFromRoute(alias, ownedBy, route))
	}

	return models
}

// newModelInfo builds a ModelInfo with all fields Codex requires.
func newModelInfo(
	slug, displayName, description string,
	contextWindow int,
	defaultReasoningLevel string,
	supportedLevels []config.ReasoningLevelPreset,
	supportsReasoningSummaries bool,
	defaultReasoningSummary string,
) ModelInfo {
	var levels []ReasoningLevelPresetDTO
	for _, p := range supportedLevels {
		levels = append(levels, ReasoningLevelPresetDTO{Effort: p.Effort, Description: p.Description})
	}
	if levels == nil {
		levels = []ReasoningLevelPresetDTO{}
	}
	var ctxWin, maxCtxWin *int
	if contextWindow > 0 {
		v := contextWindow
		ctxWin = &v
		maxCtxWin = &v
	}
	if defaultReasoningSummary == "" {
		defaultReasoningSummary = "none"
	}
	applyPatchToolType := defaultApplyPatchToolType
	return ModelInfo{
		Slug:                       slug,
		DisplayName:                displayName,
		Description:                description,
		DefaultReasoningLevel:      defaultReasoningLevel,
		SupportedReasoningLevels:   levels,
		ShellType:                  "unified_exec",
		Visibility:                 "list",
		SupportedInAPI:             true,
		Priority:                   0,
		AdditionalSpeedTiers:       []string{},
		BaseInstructions:           "",
		SupportsReasoningSummaries: supportsReasoningSummaries,
		DefaultReasoningSummary:    defaultReasoningSummary,
		WebSearchToolType:          "text",
		ApplyPatchToolType:         &applyPatchToolType,
		TruncationPolicy:           truncationPolicyForModel(slug),
		SupportsParallelToolCalls:  true,
		ContextWindow:              ctxWin,
		MaxContextWindow:           maxCtxWin,
		EffectiveContextWindowPct:  95,
		ExperimentalSupportedTools: []string{},
		InputModalities:            []string{"text"},
	}
}

func truncationPolicyForModel(string) TruncationPolicyConfig {
	return TruncationPolicyConfig{Mode: "tokens", Limit: DefaultCatalogTruncationLimit}
}
