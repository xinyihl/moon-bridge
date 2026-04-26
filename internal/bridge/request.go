package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/cache"
	deepseekv4 "moonbridge/internal/extensions/deepseek_v4"
	"moonbridge/internal/extensions/websearchinjected"
	"moonbridge/internal/logger"
	"moonbridge/internal/openai"
	"moonbridge/internal/session"
)

func (bridge *Bridge) convertInput(raw json.RawMessage, context ConversionContext, sess *session.Session, deepseekV4Enabled bool) ([]anthropic.Message, []anthropic.ContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	if deepseekV4Enabled {
		raw = deepseekv4.StripReasoningContent(raw)
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, nil, invalidRequest("input must be a string or array", "input", "invalid_request_error")
		}
		return []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: text}}}}, nil, nil
	}
	if !strings.HasPrefix(trimmed, "[") {
		return nil, nil, invalidRequest("input must be a string or array", "input", "invalid_request_error")
	}

	var items []inputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, invalidRequest("input array is invalid", "input", "invalid_request_error")
	}

	// Get the per-request DeepSeek state from the session.
	ds := perRequestDeepSeek(sess, deepseekV4Enabled)

	messages := make([]anthropic.Message, 0, len(items))
	system := make([]anthropic.ContentBlock, 0)
	seenToolHistory := false
	for _, item := range items {
		switch {
		case item.Phase == "commentary":
			continue
		case item.Type == "reasoning":
			continue
		case item.Type == "function_call":
			seenToolHistory = true
			toolName := context.AnthropicFunctionToolName(item.Namespace, item.Name)
			toolInput := toolInputFromArguments(item.Arguments)
			if ds != nil {
				ds.PrependCachedForToolUse(&messages, firstNonEmpty(item.CallID, item.ID))
			}
			appendAssistantBlock(&messages, anthropic.ContentBlock{
				Type:  "tool_use",
				ID:    firstNonEmpty(item.CallID, item.ID),
				Name:  toolName,
				Input: toolInput,
			})
		case item.Type == "custom_tool_call":
			seenToolHistory = true
			toolName := item.Name
			toolInput := json.RawMessage(item.Arguments)
			if strings.TrimSpace(item.Arguments) == "" {
				toolName, toolInput = context.AnthropicToolUseForCustomTool(item.Name, item.Input)
			} else {
				toolInput = toolInputFromArguments(item.Arguments)
			}
			if ds != nil {
				ds.PrependCachedForToolUse(&messages, firstNonEmpty(item.CallID, item.ID))
			}
			appendAssistantBlock(&messages, anthropic.ContentBlock{
				Type:  "tool_use",
				ID:    firstNonEmpty(item.CallID, item.ID),
				Name:  toolName,
				Input: toolInput,
			})
		case item.Type == "local_shell_call":
			seenToolHistory = true
			if ds != nil {
				ds.PrependCachedForToolUse(&messages, firstNonEmpty(item.CallID, item.ID))
			}
			appendAssistantBlock(&messages, anthropic.ContentBlock{
				Type:  "tool_use",
				ID:    firstNonEmpty(item.CallID, item.ID),
				Name:  "local_shell",
				Input: localShellInputFromAction(item.Action),
			})
		case strings.HasSuffix(item.Type, "_output") || item.Type == "function_call_output":
			seenToolHistory = true
			appendToolResultBlock(&messages, anthropic.ContentBlock{
				Type:      "tool_result",
				ToolUseID: firstNonEmpty(item.CallID, item.ID),
				Content:   item.Output,
			})
		case item.Type == "web_search_call":
			continue
		case item.Role == "system" || item.Role == "developer":
			system = append(system, contentBlocksFromRaw(item.Content)...)
		case item.Role == "assistant":
			blocks := contentBlocksFromRaw(item.Content)
			if len(blocks) == 0 || isEmptyWebSearchPreludeBlocks(blocks) {
				continue
			}
			if seenToolHistory && ds != nil {
				blocks = ds.PrependCachedForAssistantText(blocks)
			}
			messages = append(messages, anthropic.Message{Role: "assistant", Content: blocks})
		default:
			role := item.Role
			if role == "" {
				role = "user"
			}
			blocks := contentBlocksFromRaw(item.Content)
			if len(blocks) == 0 {
				continue
			}
			messages = append(messages, anthropic.Message{Role: role, Content: blocks})
		}
	}
	return messages, system, nil
}

func (bridge *Bridge) convertTools(tools []openai.Tool, opt RequestOptions) ([]anthropic.Tool, error) {
	converted := make([]anthropic.Tool, 0, len(tools))
	for index, tool := range tools {
		switch tool.Type {
		case "function":
			converted = append(converted, anthropicToolFromOpenAIFunction(tool.Name, tool.Description, tool.Parameters))
		case "local_shell":
			converted = append(converted, anthropic.Tool{
				Name:        "local_shell",
				Description: "Run a local shell command. Use only when you need command output from the user's workspace.",
				InputSchema: localShellSchema(),
			})
		case "custom":
			converted = append(converted, anthropicCustomToolsFromOpenAI(tool.Name, tool)...)
		case "namespace":
			for _, child := range tool.Tools {
				switch child.Type {
				case "function":
					converted = append(converted, anthropicToolFromOpenAIFunction(
						namespacedToolName(tool.Name, child.Name),
						child.Description,
						child.Parameters,
					))
				case "custom":
					converted = append(converted, anthropicCustomToolsFromOpenAI(namespacedToolName(tool.Name, child.Name), child)...)
				}
			}
		case "web_search", "web_search_preview":
			wsMode := opt.WebSearchMode
			if wsMode == "" {
				// Fall back to global config for backward compatibility.
				if bridge.cfg.WebSearchInjected() {
					wsMode = "injected"
				} else if bridge.cfg.WebSearchEnabled() {
					wsMode = "enabled"
				} else {
					wsMode = "disabled"
				}
			}
			if wsMode == "injected" {
				fcKey := opt.FirecrawlAPIKey
				if fcKey == "" {
					fcKey = bridge.cfg.FirecrawlAPIKey
				}
				converted = append(converted, websearchinjected.InjectTools(fcKey)...)
				continue
			}
			if wsMode != "enabled" {
				log := logger.L().With("tool_type", tool.Type)
				log.Debug("skipping web_search tool because provider support is disabled")
				continue
			}
			maxUses := opt.WebSearchMaxUses
			if maxUses <= 0 {
				maxUses = bridge.webSearchMaxUses()
			}
			converted = append(converted, anthropic.Tool{
				Name:    "web_search",
				Type:    "web_search_20250305",
				MaxUses: maxUses,
			})
		case "file_search", "computer_use_preview", "image_generation":
			continue
		default:
			return nil, &RequestError{
				Status:  http.StatusBadRequest,
				Message: "Unsupported tool type: " + tool.Type,
				Param:   fmt.Sprintf("tools[%d].type", index),
				Code:    "unsupported_parameter",
			}
		}
	}
	return converted, nil
}

func (bridge *Bridge) webSearchMaxUses() int {
	if bridge.cfg.WebSearchMaxUses > 0 {
		return bridge.cfg.WebSearchMaxUses
	}
	return 8
}

func (bridge *Bridge) ConversionContext(request openai.ResponsesRequest) ConversionContext {
	return ConversionContext{
		CustomTools:   customToolSpecs(request.Tools, ""),
		FunctionTools: functionToolSpecs(request.Tools, ""),
	}
}

func (bridge *Bridge) convertToolChoice(raw json.RawMessage, context ConversionContext) (anthropic.ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return anthropic.ToolChoice{Type: "auto"}, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		switch value {
		case "auto", "none":
			return anthropic.ToolChoice{Type: value}, nil
		case "required":
			return anthropic.ToolChoice{Type: "any"}, nil
		default:
			return anthropic.ToolChoice{}, invalidRequest("unsupported tool_choice", "tool_choice", "unsupported_parameter")
		}
	}
	var object struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Function  struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return anthropic.ToolChoice{}, invalidRequest("invalid tool_choice", "tool_choice", "invalid_request_error")
	}
	name := object.Name
	if name == "" {
		name = object.Function.Name
	}
	if name != "" {
		if object.Namespace != "" {
			name = context.AnthropicFunctionToolName(object.Namespace, name)
		}
		if mapped := context.AnthropicToolChoiceName(name); mapped != "" {
			name = mapped
		}
		return anthropic.ToolChoice{Type: "tool", Name: name}, nil
	}
	return anthropic.ToolChoice{}, invalidRequest("unsupported tool_choice", "tool_choice", "unsupported_parameter")
}

func (bridge *Bridge) planCache(request openai.ResponsesRequest, converted anthropic.MessageRequest) (cache.CacheCreationPlan, error) {
	cfg := bridge.cfg.Cache
	if request.PromptCacheRetention == "24h" && !cfg.AllowRetentionDowngrade {
		return cache.CacheCreationPlan{}, &RequestError{
			Status:  http.StatusBadRequest,
			Message: "prompt_cache_retention 24h is not supported by Anthropic prompt caching",
			Param:   "prompt_cache_retention",
			Code:    "unsupported_parameter",
		}
	}

	ttl := cfg.TTL
	if request.PromptCacheRetention == "in_memory" {
		ttl = "5m"
	}
	if request.PromptCacheRetention == "24h" && cfg.AllowRetentionDowngrade {
		ttl = "1h"
	}

	toolsHash, _ := cache.CanonicalHash(converted.Tools)
	systemHash, _ := cache.CanonicalHash(converted.System)
	messagesHash, _ := cache.CanonicalHash(converted.Messages)
	planner := cache.NewPlannerWithRegistry(cache.PlannerConfig{
		Mode:                     cfg.Mode,
		TTL:                      ttl,
		PromptCaching:            cfg.PromptCaching,
		AutomaticPromptCache:     cfg.AutomaticPromptCache,
		ExplicitCacheBreakpoints: cfg.ExplicitCacheBreakpoints,
		MaxBreakpoints:           cfg.MaxBreakpoints,
		MinCacheTokens:           cfg.MinCacheTokens,
		ExpectedReuse:            cfg.ExpectedReuse,
		MinimumValueScore:        cfg.MinimumValueScore,
		MinBreakpointTokens:      cfg.MinBreakpointTokens,
	}, bridge.registry)
	return planner.Plan(cache.PlanInput{
		ProviderID:            "anthropic",
		UpstreamAPIKeyID:      "configured-provider-key",
		Model:                 converted.Model,
		PromptCacheKey:        request.PromptCacheKey,
		ToolsHash:             toolsHash,
		SystemHash:            systemHash,
		MessagePrefixHash:     messagesHash,
		MessageBreakpoints:    cacheMessageBreakpointCandidates(converted.Messages),
		ToolCount:             len(converted.Tools),
		SystemBlockCount:      len(converted.System),
		MessageCount:          len(converted.Messages),
		EstimatedTokens:       estimateTokens(converted),
		EstimatedToolTokens:   estimatePartTokens(converted.Tools),
		EstimatedSystemTokens: estimatePartTokens(converted.System),
	})
}

func (bridge *Bridge) injectCacheControl(request *anthropic.MessageRequest, plan cache.CacheCreationPlan) {
	if plan.Mode == "off" {
		return
	}
	cacheControl := &anthropic.CacheControl{Type: "ephemeral"}
	if plan.TTL == "1h" {
		cacheControl.TTL = "1h"
	}
	if plan.Mode == "automatic" || plan.Mode == "hybrid" {
		request.CacheControl = cacheControl
	}
	for _, breakpointValue := range plan.Breakpoints {
		switch breakpointValue.Scope {
		case "tools":
			if len(request.Tools) > 0 {
				index := breakpointValue.ScopeIndex
				if index < 0 || index >= len(request.Tools) {
					index = len(request.Tools) - 1
				}
				request.Tools[index].CacheControl = cacheControl
			}
		case "system":
			if len(request.System) > 0 {
				index := breakpointValue.ScopeIndex
				if index < 0 || index >= len(request.System) {
					index = len(request.System) - 1
				}
				request.System[index].CacheControl = cacheControl
			}
		case "messages":
			if len(request.Messages) > 0 {
				messageIndex := breakpointValue.ScopeIndex
				if messageIndex < 0 || messageIndex >= len(request.Messages) {
					messageIndex = len(request.Messages) - 1
				}
				contentIndex := len(request.Messages[messageIndex].Content) - 1
				if breakpointValue.ContentIndex >= 0 && breakpointValue.ContentIndex < len(request.Messages[messageIndex].Content) {
					contentIndex = breakpointValue.ContentIndex
				}
				if contentIndex >= 0 {
					request.Messages[messageIndex].Content[contentIndex].CacheControl = cacheControl
				}
			}
		}
	}
}

func cacheMessageBreakpointCandidates(messages []anthropic.Message) []cache.MessageBreakpointCandidate {
	candidates := make([]cache.MessageBreakpointCandidate, 0, len(messages))
	for messageIndex, message := range messages {
		contentIndex := lastCacheableContentIndex(message.Content)
		if contentIndex < 0 {
			continue
		}
		blockPath := fmt.Sprintf("messages[%d].content[%d]", messageIndex, contentIndex)
		if contentIndex == len(message.Content)-1 {
			blockPath = fmt.Sprintf("messages[%d].content[last]", messageIndex)
		}
		candidates = append(candidates, cache.MessageBreakpointCandidate{
			MessageIndex: messageIndex,
			ContentIndex: contentIndex,
			BlockPath:    blockPath,
			Role:         message.Role,
		})
	}
	return candidates
}

func lastCacheableContentIndex(content []anthropic.ContentBlock) int {
	for index := len(content) - 1; index >= 0; index-- {
		block := content[index]
		if block.Type == "text" && strings.TrimSpace(block.Text) == "" {
			continue
		}
		return index
	}
	return -1
}

// perRequestDeepSeek returns the DeepSeek state from a session if DeepSeek V4 is enabled.
func perRequestDeepSeek(sess *session.Session, deepseekV4Enabled bool) *deepseekv4.State {
	if !deepseekV4Enabled {
		return nil
	}
	if sess == nil {
		return nil
	}
	return sess.DeepSeek
}

type inputItem struct {
	Type      string             `json:"type"`
	ID        string             `json:"id"`
	Role      string             `json:"role"`
	Phase     string             `json:"phase"`
	Content   json.RawMessage    `json:"content"`
	CallID    string             `json:"call_id"`
	Name      string             `json:"name"`
	Namespace string             `json:"namespace"`
	Arguments string             `json:"arguments"`
	Input     string             `json:"input"`
	Action    *openai.ToolAction `json:"action"`
	Output    string             `json:"output"`
}

// toolInputFromArguments recovers histories poisoned by concatenated tool
// argument objects while leaving ordinary invalid JSON on the existing path.
func toolInputFromArguments(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	if recovered, ok := lastConcatenatedJSONValue(trimmed); ok {
		return recovered
	}
	return json.RawMessage(trimmed)
}

func lastConcatenatedJSONValue(value string) (json.RawMessage, bool) {
	decoder := json.NewDecoder(strings.NewReader(value))
	var last json.RawMessage
	count := 0
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			return last, count > 1
		}
		if err != nil {
			return nil, false
		}
		last = append(json.RawMessage(nil), raw...)
		count++
	}
}

func contentBlocksFromRaw(raw json.RawMessage) []anthropic.ContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		_ = json.Unmarshal(raw, &text)
		if text == "" {
			return nil
		}
		return []anthropic.ContentBlock{{Type: "text", Text: text}}
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil && len(parts) > 0 {
		blocks := make([]anthropic.ContentBlock, 0, len(parts))
		for _, part := range parts {
			if part.Type == "input_text" || part.Type == "text" || part.Type == "output_text" {
				if part.Text == "" {
					continue
				}
				blocks = append(blocks, anthropic.ContentBlock{Type: "text", Text: part.Text})
			}
		}
		return blocks
	}
	if trimmed == "" {
		return nil
	}
	return []anthropic.ContentBlock{{Type: "text", Text: trimmed}}
}

func isEmptyWebSearchPreludeBlocks(blocks []anthropic.ContentBlock) bool {
	if len(blocks) != 1 || blocks[0].Type != "text" {
		return false
	}
	return isEmptyWebSearchPrelude(blocks[0].Text)
}

func isEmptyWebSearchPrelude(text string) bool {
	return strings.TrimSpace(text) == "Search results for query:"
}

func appendAssistantBlock(messages *[]anthropic.Message, block anthropic.ContentBlock) {
	lastIndex := len(*messages) - 1
	if lastIndex >= 0 && (*messages)[lastIndex].Role == "assistant" {
		(*messages)[lastIndex].Content = append((*messages)[lastIndex].Content, block)
		return
	}
	*messages = append(*messages, anthropic.Message{Role: "assistant", Content: []anthropic.ContentBlock{block}})
}

func appendToolResultBlock(messages *[]anthropic.Message, block anthropic.ContentBlock) {
	lastIndex := len(*messages) - 1
	if lastIndex >= 0 && (*messages)[lastIndex].Role == "user" && allContentBlocksHaveType((*messages)[lastIndex].Content, "tool_result") {
		(*messages)[lastIndex].Content = append((*messages)[lastIndex].Content, block)
		return
	}
	*messages = append(*messages, anthropic.Message{Role: "user", Content: []anthropic.ContentBlock{block}})
}

func allContentBlocksHaveType(blocks []anthropic.ContentBlock, blockType string) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if block.Type != blockType {
			return false
		}
	}
	return true
}

func parseStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err == nil {
		return multiple
	}
	return nil
}
