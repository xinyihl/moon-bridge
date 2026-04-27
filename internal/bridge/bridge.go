package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/cache"
	"moonbridge/internal/config"
	"moonbridge/internal/logger"
	"moonbridge/internal/openai"
	"moonbridge/internal/plugin"
	"moonbridge/internal/session"
)

type Bridge struct {
	cfg      config.Config
	registry *cache.MemoryRegistry
	plugins  *plugin.Registry
}

type ConversionContext struct {
	CustomTools   map[string]CustomToolSpec
	FunctionTools map[string]FunctionToolSpec
}

type CustomToolSpec struct {
	GrammarDefinition string
	Kind              CustomToolKind
	OpenAIName        string
	ApplyPatchAction  string
}

type FunctionToolSpec struct {
	Namespace string
	Name      string
}

type CustomToolKind string

const (
	CustomToolKindRaw        CustomToolKind = "raw"
	CustomToolKindApplyPatch CustomToolKind = "apply_patch"
	CustomToolKindExec       CustomToolKind = "exec"
)

func (context ConversionContext) IsCustomTool(name string) bool {
	if len(context.CustomTools) == 0 {
		return false
	}
	_, ok := context.CustomTools[name]
	return ok
}

func (context ConversionContext) OpenAINameForCustomTool(name string) string {
	spec, ok := context.CustomTools[name]
	if !ok || spec.OpenAIName == "" {
		return name
	}
	return spec.OpenAIName
}

func (context ConversionContext) AnthropicToolChoiceName(name string) string {
	spec, ok := context.CustomTools[name]
	if !ok || spec.Kind != CustomToolKindApplyPatch {
		return name
	}
	return applyPatchToolName(name, "batch")
}

func (context ConversionContext) CustomToolInputFromRaw(name string, raw json.RawMessage) string {
	spec, ok := context.CustomTools[name]
	if !ok {
		return customToolInputFromRaw(raw)
	}
	switch spec.Kind {
	case CustomToolKindApplyPatch:
		return applyPatchInputFromProxyRaw(raw, spec.ApplyPatchAction)
	case CustomToolKindExec:
		return execInputFromProxyRaw(raw)
	default:
		return customToolInputFromRaw(raw)
	}
}

func (context ConversionContext) AnthropicToolUseForCustomTool(name string, input string) (string, json.RawMessage) {
	spec, ok := context.CustomTools[name]
	if !ok {
		return name, customToolInputObject(input)
	}
	switch spec.Kind {
	case CustomToolKindApplyPatch:
		toolName, action := applyPatchToolNameAndActionForGrammar(name, input)
		return toolName, applyPatchProxyInputFromGrammar(input, action)
	case CustomToolKindExec:
		return name, execProxyInputFromGrammar(input)
	default:
		return name, customToolInputObject(input)
	}
}

func (context ConversionContext) NormalizeCustomToolInput(name string, input string) string {
	spec, ok := context.CustomTools[name]
	if !ok {
		return input
	}
	if spec.Kind == CustomToolKindApplyPatch {
		return normalizeApplyPatchInput(input)
	}
	return input
}

func (context ConversionContext) OpenAIFunctionToolName(name string) (string, string) {
	spec, ok := context.FunctionTools[name]
	if !ok {
		return name, ""
	}
	return spec.Name, spec.Namespace
}

func (context ConversionContext) AnthropicFunctionToolName(namespace string, name string) string {
	if namespace == "" {
		return name
	}
	if strings.HasPrefix(name, namespace) {
		return name
	}
	return namespacedToolName(namespace, name)
}

type RequestError struct {
	Status  int
	Message string
	Param   string
	Code    string
}

func (err *RequestError) Error() string {
	return err.Message
}

func New(cfg config.Config, registry *cache.MemoryRegistry, plugins *plugin.Registry) *Bridge {
	if registry == nil {
		registry = cache.NewMemoryRegistry()
	}
	if plugins == nil {
		plugins = plugin.NewRegistry(nil)
	}
	return &Bridge{cfg: cfg, registry: registry, plugins: plugins}
}

// RequestOptions carries per-request overrides resolved by the server layer.
type RequestOptions struct {
	// WebSearchMode is the resolved web search support for this request's provider.
	// One of "enabled", "disabled", "injected", or empty (falls back to global config).
	WebSearchMode string
	// WebSearchMaxUses overrides the max uses for web_search tool.
	WebSearchMaxUses int
	// FirecrawlAPIKey overrides the Firecrawl API key for injected search.
	FirecrawlAPIKey string
}

// ToAnthropic converts an OpenAI Responses request to an Anthropic MessageRequest.
// Takes an optional session for per-request extension state.
func (bridge *Bridge) ToAnthropic(request openai.ResponsesRequest, sess *session.Session, opts ...RequestOptions) (anthropic.MessageRequest, cache.CacheCreationPlan, error) {
	var opt RequestOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	pluginCtx := bridge.pluginRequestContext(request.Model, sess, request.Reasoning, opt)
	log := logger.L().With("model", request.Model)
	log.Debug("正在将 OpenAI 请求转换为 Anthropic 格式")
	if request.Model == "" {
		log.Warn("模型名称是必需的")
		return anthropic.MessageRequest{}, cache.CacheCreationPlan{}, invalidRequest("模型名称是必需的", "model", "missing_required_parameter")
	}

	conversionContext := bridge.ConversionContext(request)
	messages, system, err := bridge.convertInput(request.Input, conversionContext, sess, request.Model)
	if err != nil {
		return anthropic.MessageRequest{}, cache.CacheCreationPlan{}, err
	}
	if request.Instructions != "" {
		system = append([]anthropic.ContentBlock{{Type: "text", Text: request.Instructions}}, system...)
	}
	if bridge.cfg.SystemPrompt != "" {
		system = append([]anthropic.ContentBlock{{Type: "text", Text: bridge.cfg.SystemPrompt}}, system...)
	}
	messages = bridge.plugins.RewriteMessages(pluginCtx, messages)
	if len(messages) == 0 {
		messages = []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: " "}}}}
	}

	tools, err := bridge.convertTools(request.Tools, opt)
	if err != nil {
		return anthropic.MessageRequest{}, cache.CacheCreationPlan{}, err
	}
	tools = append(tools, bridge.plugins.InjectTools(pluginCtx)...)
	toolChoice, err := bridge.convertToolChoice(request.ToolChoice, conversionContext)
	if err != nil {
		return anthropic.MessageRequest{}, cache.CacheCreationPlan{}, err
	}

	maxTokens := request.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = bridge.cfg.DefaultMaxTokens
	}
	if maxTokens == 0 {
		maxTokens = 1024
	}

	converted := anthropic.MessageRequest{
		Model:         bridge.cfg.ModelFor(request.Model),
		MaxTokens:     maxTokens,
		System:        system,
		Messages:      messages,
		Tools:         tools,
		ToolChoice:    toolChoice,
		Temperature:   request.Temperature,
		TopP:          request.TopP,
		StopSequences: parseStopSequences(request.Stop),
		Stream:        request.Stream,
		Metadata:      request.Metadata,
	}

	bridge.plugins.MutateRequest(pluginCtx, &converted)

	plan, err := bridge.planCache(request, converted)
	if err != nil {
		log.Warn("缓存规划失败", "error", err)
		return anthropic.MessageRequest{}, cache.CacheCreationPlan{}, err
	}
	bridge.injectCacheControl(&converted, plan)
	log.Debug("请求已转换", "anthropic_model", converted.Model, "max_tokens", converted.MaxTokens, "messages", len(converted.Messages), "tools", len(converted.Tools), "cache_mode", plan.Mode)

	return converted, plan, nil
}

func (bridge *Bridge) FromAnthropic(response anthropic.MessageResponse, model string) openai.Response {
	return bridge.FromAnthropicWithPlan(response, model, cache.CacheCreationPlan{})
}

func (bridge *Bridge) FromAnthropicWithContext(response anthropic.MessageResponse, model string, context ConversionContext) openai.Response {
	return bridge.FromAnthropicWithPlanAndContext(response, model, cache.CacheCreationPlan{}, context, nil)
}

// UpdateRegistryFromUsage updates the in-memory cache registry from upstream usage signals.
// This is intended for the streaming path where FromAnthropicWithPlan is not called.
func (bridge *Bridge) UpdateRegistryFromUsage(plan cache.CacheCreationPlan, signals cache.UsageSignals, inputTokens int) {
	key := plan.PrefixKey
	if key == "" {
		key = plan.LocalKey
	}
	if key == "" {
		return
	}
	bridge.registry.UpdateFromUsage(key, signals, inputTokens, parseTTL(plan.TTL))
	logger.L().Debug("缓存注册表已更新（流式）", "key", key, "input_tokens", inputTokens, "cache_creation", signals.CacheCreationInputTokens, "cache_read", signals.CacheReadInputTokens)
}

func (bridge *Bridge) FromAnthropicWithPlan(response anthropic.MessageResponse, model string, plan cache.CacheCreationPlan) openai.Response {
	return bridge.FromAnthropicWithPlanAndContext(response, model, plan, ConversionContext{}, nil)
}

func (bridge *Bridge) FromAnthropicWithPlanAndContext(response anthropic.MessageResponse, model string, plan cache.CacheCreationPlan, context ConversionContext, sess *session.Session) openai.Response {
	log := logger.L().With("model", model)
	log.Debug("正在将 Anthropic 响应转换为 OpenAI 格式", "provider_id", response.ID, "stop_reason", response.StopReason)
	registryKey := plan.PrefixKey
	if registryKey == "" {
		registryKey = plan.LocalKey
	}
	if registryKey != "" {
		bridge.registry.UpdateFromUsage(registryKey, cache.UsageSignals{
			InputTokens:              response.Usage.InputTokens,
			CacheCreationInputTokens: response.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     response.Usage.CacheReadInputTokens,
		}, response.Usage.InputTokens, parseTTL(plan.TTL))
		log.Debug("缓存注册表已更新", "key", registryKey, "input_tokens", response.Usage.InputTokens, "cache_creation", response.Usage.CacheCreationInputTokens, "cache_read", response.Usage.CacheReadInputTokens)
	}
	if sess != nil {
		bridge.plugins.RememberResponseContent(model, response.Content, sess.ExtensionData)
	}

	output := make([]openai.OutputItem, 0, len(response.Content))
	var outputText strings.Builder
	messageContent := make([]openai.ContentPart, 0)
	var thinkingText string
	hasToolCalls := false

	for index, block := range response.Content {
		// Let extensions filter/extract reasoning from content blocks.
		skip, rt := bridge.plugins.OnResponseContent(model, block)
		thinkingText += rt
		if skip {
			continue
		}
		switch block.Type {
		case "text":
			part := openai.ContentPart{Type: "output_text", Text: block.Text}
			messageContent = append(messageContent, part)
			outputText.WriteString(block.Text)
		case "tool_use":
			hasToolCalls = true
			if len(messageContent) > 0 {
				output = append(output, openai.OutputItem{
					Type:    "message",
					ID:      fmt.Sprintf("msg_item_%d", index),
					Status:  "completed",
					Role:    "assistant",
					Content: messageContent,
				})
				messageContent = nil
			}
			if block.Name == "local_shell" {
				output = append(output, openai.OutputItem{
					Type:   "local_shell_call",
					ID:     "lc_" + block.ID,
					CallID: block.ID,
					Status: "completed",
					Action: localShellActionFromRaw(block.Input),
				})
				continue
			}
			if context.IsCustomTool(block.Name) {
				log.Debug("custom tool call", "name", block.Name)
				output = append(output, openai.OutputItem{
					Type:   "custom_tool_call",
					ID:     customToolItemID(block.ID),
					CallID: block.ID,
					Name:   context.OpenAINameForCustomTool(block.Name),
					Input:  context.CustomToolInputFromRaw(block.Name, block.Input),
					Status: "completed",
				})
				continue
			}
			name, namespace := context.OpenAIFunctionToolName(block.Name)
			output = append(output, openai.OutputItem{
				Type:      "function_call",
				ID:        "fc_" + block.ID,
				CallID:    block.ID,
				Name:      name,
				Namespace: namespace,
				Arguments: string(block.Input),
				Status:    "completed",
			})
		case "server_tool_use":
			if block.Name == "web_search" {
				log.Debug("web search tool call")
				action := webSearchActionFromRaw(block.Input)
				if !hasWebSearchActionDetails(action) {
					continue
				}
				output = append(output, openai.OutputItem{
					Type:   "web_search_call",
					ID:     webSearchItemID(block.ID),
					Status: "completed",
					Action: action,
				})
			}
		}
	}
	if len(messageContent) > 0 {
		output = append(output, openai.OutputItem{
			Type:    "message",
			ID:      "msg_item_0",
			Status:  "completed",
			Role:    "assistant",
			Content: messageContent,
		})
	}

	// Let extensions inject reasoning items into the output.
	if thinkingText != "" && hasToolCalls {
		reasoningItem := openai.OutputItem{
			Type: "reasoning",
			Summary: []openai.ReasoningItemSummary{{
				Type: "summary_text",
				Text: thinkingText,
			}},
		}
		output = append([]openai.OutputItem{reasoningItem}, output...)
	}

	status, incomplete := statusFromStopReason(response.StopReason)
	usage := normalizeUsage(response.Usage)

	metadata := map[string]any{
		"provider_message_id": response.ID,
	}
	if response.Usage.CacheCreationInputTokens > 0 || response.Usage.CacheReadInputTokens > 0 || response.Usage.CacheCreation != nil {
		metadata["provider_usage"] = response.Usage
	}

	openAIResponse := openai.Response{
		ID:                responseID(response.ID),
		Object:            "response",
		CreatedAt:         time.Now().Unix(),
		Status:            status,
		Model:             model,
		Output:            output,
		OutputText:        outputText.String(),
		Usage:             usage,
		Metadata:          metadata,
		IncompleteDetails: incomplete,
	}
	bridge.plugins.PostProcessResponse(bridge.pluginRequestContext(model, sess, nil, RequestOptions{}), &openAIResponse)
	log.Info("响应已转换", "output_items", len(openAIResponse.Output), "status", openAIResponse.Status)
	return openAIResponse
}

func (bridge *Bridge) ErrorResponse(err error) (int, openai.ErrorResponse) {
	return bridge.errorResponse(err, "")
}

func (bridge *Bridge) ErrorResponseForModel(model string, err error) (int, openai.ErrorResponse) {
	return bridge.errorResponse(err, model)
}

func (bridge *Bridge) errorResponse(err error, model string) (int, openai.ErrorResponse) {
	var requestError *RequestError
	if errors.As(err, &requestError) {
		return requestError.Status, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: requestError.Message,
			Type:    "invalid_request_error",
			Param:   requestError.Param,
			Code:    requestError.Code,
		}}
	}
	if providerError, ok := anthropic.IsProviderError(err); ok {
		msg := providerError.Error()
		msg = bridge.plugins.TransformError(model, msg)
		return providerError.OpenAIStatus(), openai.ErrorResponse{Error: openai.ErrorObject{
			Message: msg,
			Type:    providerError.OpenAIType(),
			Code:    providerError.OpenAICode(),
		}}
	}
	return http.StatusInternalServerError, openai.ErrorResponse{Error: openai.ErrorObject{
		Message: err.Error(),
		Type:    "server_error",
		Code:    "internal_error",
	}}
}

// ProviderFor returns the provider key that serves the given model alias.
// Delegates to Config.ProviderFor.
func (bridge *Bridge) ProviderFor(modelAlias string) string {
	return bridge.cfg.ProviderFor(modelAlias)
}

// NewSession creates a session with extension data initialized.
func (bridge *Bridge) NewSession() *session.Session {
	sess := session.New()
	sess.InitExtensions(bridge.plugins.NewSessionData())
	return sess
}

func (bridge *Bridge) pluginRequestContext(model string, sess *session.Session, reasoning map[string]any, opt RequestOptions) *plugin.RequestContext {
	var sessionData map[string]any
	if sess != nil {
		sessionData = sess.ExtensionData
	}
	return &plugin.RequestContext{
		ModelAlias:  model,
		SessionData: sessionData,
		Reasoning:   reasoning,
		WebSearch: plugin.WebSearchInfo{
			Mode:         opt.WebSearchMode,
			MaxUses:      opt.WebSearchMaxUses,
			FirecrawlKey: opt.FirecrawlAPIKey,
		},
	}
}
