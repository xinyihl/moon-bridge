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
	"moonbridge/internal/openai"
)

type Bridge struct {
	cfg      config.Config
	registry *cache.MemoryRegistry
}

type ConversionContext struct {
	CustomTools map[string]CustomToolSpec
}

type CustomToolSpec struct {
	GrammarDefinition string
	Kind              CustomToolKind
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

func (context ConversionContext) CustomToolInputFromRaw(name string, raw json.RawMessage) string {
	spec, ok := context.CustomTools[name]
	if !ok {
		return customToolInputFromRaw(raw)
	}
	switch spec.Kind {
	case CustomToolKindApplyPatch:
		return applyPatchInputFromProxyRaw(raw)
	case CustomToolKindExec:
		return execInputFromProxyRaw(raw)
	default:
		return customToolInputFromRaw(raw)
	}
}

func (context ConversionContext) AnthropicInputForCustomTool(name string, input string) json.RawMessage {
	spec, ok := context.CustomTools[name]
	if !ok {
		return customToolInputObject(input)
	}
	switch spec.Kind {
	case CustomToolKindApplyPatch:
		return applyPatchProxyInputFromGrammar(input)
	case CustomToolKindExec:
		return execProxyInputFromGrammar(input)
	default:
		return customToolInputObject(input)
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

type RequestError struct {
	Status  int
	Message string
	Param   string
	Code    string
}

func (err *RequestError) Error() string {
	return err.Message
}

func New(cfg config.Config, registry *cache.MemoryRegistry) *Bridge {
	if registry == nil {
		registry = cache.NewMemoryRegistry()
	}
	return &Bridge{cfg: cfg, registry: registry}
}

func (bridge *Bridge) ToAnthropic(request openai.ResponsesRequest) (anthropic.MessageRequest, cache.CacheCreationPlan, error) {
	if request.Model == "" {
		return anthropic.MessageRequest{}, cache.CacheCreationPlan{}, invalidRequest("model is required", "model", "missing_required_parameter")
	}

	conversionContext := bridge.ConversionContext(request)
	messages, system, err := bridge.convertInput(request.Input, conversionContext)
	if err != nil {
		return anthropic.MessageRequest{}, cache.CacheCreationPlan{}, err
	}
	if request.Instructions != "" {
		system = append([]anthropic.ContentBlock{{Type: "text", Text: request.Instructions}}, system...)
	}
	if len(messages) == 0 {
		messages = []anthropic.Message{{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: ""}}}}
	}

	tools, err := bridge.convertTools(request.Tools)
	if err != nil {
		return anthropic.MessageRequest{}, cache.CacheCreationPlan{}, err
	}
	toolChoice, err := bridge.convertToolChoice(request.ToolChoice)
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

	plan, err := bridge.planCache(request, converted)
	if err != nil {
		return anthropic.MessageRequest{}, cache.CacheCreationPlan{}, err
	}
	bridge.injectCacheControl(&converted, plan)

	return converted, plan, nil
}

func (bridge *Bridge) FromAnthropic(response anthropic.MessageResponse, model string) openai.Response {
	return bridge.FromAnthropicWithPlan(response, model, cache.CacheCreationPlan{})
}

func (bridge *Bridge) FromAnthropicWithContext(response anthropic.MessageResponse, model string, context ConversionContext) openai.Response {
	return bridge.FromAnthropicWithPlanAndContext(response, model, cache.CacheCreationPlan{}, context)
}

func (bridge *Bridge) FromAnthropicWithPlan(response anthropic.MessageResponse, model string, plan cache.CacheCreationPlan) openai.Response {
	return bridge.FromAnthropicWithPlanAndContext(response, model, plan, ConversionContext{})
}

func (bridge *Bridge) FromAnthropicWithPlanAndContext(response anthropic.MessageResponse, model string, plan cache.CacheCreationPlan, context ConversionContext) openai.Response {
	if plan.LocalKey != "" {
		bridge.registry.UpdateFromUsage(plan.LocalKey, cache.UsageSignals{
			InputTokens:              response.Usage.InputTokens,
			CacheCreationInputTokens: response.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     response.Usage.CacheReadInputTokens,
		}, response.Usage.InputTokens)
	}

	output := make([]openai.OutputItem, 0, len(response.Content))
	var outputText strings.Builder
	messageContent := make([]openai.ContentPart, 0)

	for index, block := range response.Content {
		switch block.Type {
		case "text":
			part := openai.ContentPart{Type: "output_text", Text: block.Text}
			messageContent = append(messageContent, part)
			outputText.WriteString(block.Text)
		case "tool_use":
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
				output = append(output, openai.OutputItem{
					Type:   "custom_tool_call",
					ID:     customToolItemID(block.ID),
					CallID: block.ID,
					Name:   block.Name,
					Input:  context.CustomToolInputFromRaw(block.Name, block.Input),
					Status: "completed",
				})
				continue
			}
			output = append(output, openai.OutputItem{
				Type:      "function_call",
				ID:        "fc_" + block.ID,
				CallID:    block.ID,
				Name:      block.Name,
				Arguments: string(block.Input),
				Status:    "completed",
			})
		case "server_tool_use":
			if block.Name == "web_search" {
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

	status, incomplete := statusFromStopReason(response.StopReason)
	usage := normalizeUsage(response.Usage)

	metadata := map[string]any{
		"provider_message_id": response.ID,
	}
	if response.Usage.CacheCreationInputTokens > 0 || response.Usage.CacheReadInputTokens > 0 || response.Usage.CacheCreation != nil {
		metadata["provider_usage"] = response.Usage
	}

	return openai.Response{
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
}

func (bridge *Bridge) ErrorResponse(err error) (int, openai.ErrorResponse) {
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
		return providerError.OpenAIStatus(), openai.ErrorResponse{Error: openai.ErrorObject{
			Message: providerError.Error(),
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

func (bridge *Bridge) convertInput(raw json.RawMessage, context ConversionContext) ([]anthropic.Message, []anthropic.ContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
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

	messages := make([]anthropic.Message, 0, len(items))
	system := make([]anthropic.ContentBlock, 0)
	for _, item := range items {
		switch {
		case item.Type == "function_call":
			toolInput := json.RawMessage(item.Arguments)
			if len(toolInput) == 0 {
				toolInput = json.RawMessage(`{}`)
			}
			appendAssistantBlock(&messages, anthropic.ContentBlock{
				Type:  "tool_use",
				ID:    firstNonEmpty(item.CallID, item.ID),
				Name:  item.Name,
				Input: toolInput,
			})
		case item.Type == "custom_tool_call":
			toolInput := json.RawMessage(item.Arguments)
			if len(toolInput) == 0 {
				toolInput = context.AnthropicInputForCustomTool(item.Name, item.Input)
			}
			appendAssistantBlock(&messages, anthropic.ContentBlock{
				Type:  "tool_use",
				ID:    firstNonEmpty(item.CallID, item.ID),
				Name:  item.Name,
				Input: toolInput,
			})
		case item.Type == "local_shell_call":
			appendAssistantBlock(&messages, anthropic.ContentBlock{
				Type:  "tool_use",
				ID:    firstNonEmpty(item.CallID, item.ID),
				Name:  "local_shell",
				Input: localShellInputFromAction(item.Action),
			})
		case strings.HasSuffix(item.Type, "_output") || item.Type == "function_call_output":
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
			if isEmptyWebSearchPreludeBlocks(blocks) {
				continue
			}
			messages = append(messages, anthropic.Message{Role: "assistant", Content: blocks})
		default:
			role := item.Role
			if role == "" {
				role = "user"
			}
			messages = append(messages, anthropic.Message{Role: role, Content: contentBlocksFromRaw(item.Content)})
		}
	}
	return messages, system, nil
}

func (bridge *Bridge) convertTools(tools []openai.Tool) ([]anthropic.Tool, error) {
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
			converted = append(converted, anthropicCustomToolFromOpenAI(tool.Name, tool))
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
					converted = append(converted, anthropicCustomToolFromOpenAI(namespacedToolName(tool.Name, child.Name), child))
				}
			}
		case "web_search", "web_search_preview":
			converted = append(converted, anthropic.Tool{
				Name:    "web_search",
				Type:    "web_search_20250305",
				MaxUses: bridge.webSearchMaxUses(),
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
	return ConversionContext{CustomTools: customToolSpecs(request.Tools, "")}
}

func customToolSpecs(tools []openai.Tool, namespace string) map[string]CustomToolSpec {
	specs := map[string]CustomToolSpec{}
	for _, tool := range tools {
		switch tool.Type {
		case "custom":
			definition := customToolGrammarDefinition(tool)
			specs[namespacedToolName(namespace, tool.Name)] = CustomToolSpec{
				GrammarDefinition: definition,
				Kind:              customToolKindFromGrammar(definition),
			}
		case "namespace":
			for name, spec := range customToolSpecs(tool.Tools, namespacedToolName(namespace, tool.Name)) {
				specs[name] = spec
			}
		}
	}
	return specs
}

func anthropicToolFromOpenAIFunction(name string, description string, parameters map[string]any) anthropic.Tool {
	if parameters == nil {
		parameters = map[string]any{"type": "object"}
	}
	return anthropic.Tool{
		Name:        name,
		Description: description,
		InputSchema: parameters,
	}
}

func anthropicCustomToolFromOpenAI(name string, tool openai.Tool) anthropic.Tool {
	definition := customToolGrammarDefinition(tool)
	kind := customToolKindFromGrammar(definition)
	if kind == CustomToolKindApplyPatch {
		return anthropic.Tool{
			Name:        name,
			Description: customToolDescription(tool) + "\n\nMoon Bridge exposes this custom grammar tool as structured JSON. Provide patch operations; Moon Bridge will reconstruct the raw Codex apply_patch grammar before returning the tool call to Codex.",
			InputSchema: applyPatchProxySchema(),
		}
	}
	if kind == CustomToolKindExec {
		return anthropic.Tool{
			Name:        name,
			Description: customToolDescription(tool) + "\n\nMoon Bridge exposes this custom grammar tool as structured JSON. Put the JavaScript source in `source`; Moon Bridge will return that source as the raw Codex custom tool input.",
			InputSchema: execProxySchema(),
		}
	}
	inputDescription := customToolInputDescription(tool)
	return anthropic.Tool{
		Name:        name,
		Description: customToolDescription(tool),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{"input": map[string]any{
				"type":        "string",
				"description": inputDescription,
			}},
			"required": []string{"input"},
		},
	}
}

func customToolDescription(tool openai.Tool) string {
	parts := []string{}
	if strings.TrimSpace(tool.Description) != "" {
		parts = append(parts, strings.TrimSpace(tool.Description))
	}
	if definition := customToolGrammarDefinition(tool); definition != "" {
		parts = append(parts, "OpenAI custom tool grammar:\n"+definition)
	}
	if len(parts) == 0 {
		return "Use this custom tool with its raw freeform input in the input field."
	}
	return strings.Join(parts, "\n\n")
}

func customToolInputDescription(tool openai.Tool) string {
	description := "Raw freeform input for this custom tool. Put only the tool input text here, not a JSON string or markdown wrapper."
	if definition := customToolGrammarDefinition(tool); definition != "" {
		if isApplyPatchGrammar(definition) {
			description = "Raw apply_patch patch text. It must start with '*** Begin Patch' and end with a bare '*** End Patch' line. Use Codex apply_patch headers such as '*** Add File:', '*** Delete File:' or '*** Update File:'. In Add File hunks, file content lines start with '+', but patch metadata lines like '*** End Patch' must not be prefixed with '+'. Do not use unified diff headers like 'diff --git', '---', '+++' and do not wrap the patch in markdown fences."
		}
		description += "\n\nGrammar:\n" + definition
	}
	return description
}

func customToolGrammarDefinition(tool openai.Tool) string {
	if tool.Format == nil {
		return ""
	}
	definition, _ := tool.Format["definition"].(string)
	return strings.TrimSpace(definition)
}

func customToolKindFromGrammar(definition string) CustomToolKind {
	switch {
	case isApplyPatchGrammar(definition):
		return CustomToolKindApplyPatch
	case isExecGrammar(definition):
		return CustomToolKindExec
	default:
		return CustomToolKindRaw
	}
}

func isApplyPatchGrammar(definition string) bool {
	return strings.Contains(definition, `begin_patch: "*** Begin Patch"`) &&
		strings.Contains(definition, `end_patch: "*** End Patch"`) &&
		strings.Contains(definition, `add_hunk: "*** Add File: "`)
}

func isExecGrammar(definition string) bool {
	return strings.Contains(definition, "@exec") ||
		(strings.Contains(definition, "pragma_source") && strings.Contains(definition, "plain_source"))
}

func applyPatchProxySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operations": map[string]any{
				"type":        "array",
				"description": "Structured patch operations. Moon Bridge reconstructs Codex apply_patch grammar from these operations.",
				"minItems":    1,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type": map[string]any{
							"type":        "string",
							"enum":        []string{"add_file", "delete_file", "update_file"},
							"description": "Patch operation type.",
						},
						"path": map[string]any{"type": "string", "description": "Target file path."},
						"move_to": map[string]any{
							"type":        "string",
							"description": "Optional destination path for update_file move operations.",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "For add_file: full file content without leading '+'.",
						},
						"hunks": map[string]any{
							"type":        "array",
							"description": "For update_file: structured hunks.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"context": map[string]any{"type": "string", "description": "Optional @@ context header text."},
									"lines": map[string]any{
										"type": "array",
										"items": map[string]any{
											"type": "object",
											"properties": map[string]any{
												"op":   map[string]any{"type": "string", "enum": []string{"context", "add", "remove"}},
												"text": map[string]any{"type": "string"},
											},
											"required": []string{"op", "text"},
										},
									},
								},
								"required": []string{"lines"},
							},
						},
						"changes": map[string]any{
							"type":        "string",
							"description": "For update_file fallback: raw hunk body using @@ plus lines prefixed by space, + or -.",
						},
					},
					"required": []string{"type", "path"},
				},
			},
		},
		"required": []string{"operations"},
	}
}

func execProxySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source": map[string]any{
				"type":        "string",
				"description": "JavaScript source code, including any // @exec pragmas if needed.",
			},
		},
		"required": []string{"source"},
	}
}

func customToolItemID(toolUseID string) string {
	if toolUseID == "" {
		return "ctc_generated"
	}
	if strings.HasPrefix(toolUseID, "ctc_") {
		return toolUseID
	}
	return "ctc_" + toolUseID
}

func customToolInputObject(input string) json.RawMessage {
	data, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		return json.RawMessage(`{"input":""}`)
	}
	return data
}

func customToolInputFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil {
		if value, ok := object["input"]; ok {
			var input string
			if err := json.Unmarshal(value, &input); err == nil {
				return input
			}
			return string(value)
		}
	}
	var input string
	if err := json.Unmarshal(raw, &input); err == nil {
		return input
	}
	return string(raw)
}

type applyPatchProxyInput struct {
	Operations []applyPatchOperation `json:"operations"`
	RawPatch   string                `json:"raw_patch"`
	Input      string                `json:"input"`
	Patch      string                `json:"patch"`
}

type applyPatchOperation struct {
	Type    string             `json:"type"`
	Path    string             `json:"path"`
	MoveTo  string             `json:"move_to"`
	Content string             `json:"content"`
	Hunks   []applyPatchHunk   `json:"hunks"`
	Changes string             `json:"changes"`
	Lines   []applyPatchLineOp `json:"lines"`
}

type applyPatchHunk struct {
	Context string             `json:"context"`
	Lines   []applyPatchLineOp `json:"lines"`
}

type applyPatchLineOp struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

func applyPatchInputFromProxyRaw(raw json.RawMessage) string {
	var input applyPatchProxyInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return normalizeApplyPatchInput(customToolInputFromRaw(raw))
	}
	switch {
	case len(input.Operations) > 0:
		return buildApplyPatchInput(input.Operations)
	case input.RawPatch != "":
		return normalizeApplyPatchInput(input.RawPatch)
	case input.Patch != "":
		return normalizeApplyPatchInput(input.Patch)
	case input.Input != "":
		return normalizeApplyPatchInput(input.Input)
	default:
		return normalizeApplyPatchInput(customToolInputFromRaw(raw))
	}
}

func applyPatchProxyInputFromGrammar(input string) json.RawMessage {
	if operations, ok := parseApplyPatchOperations(input); ok {
		data, err := json.Marshal(applyPatchProxyInput{Operations: operations})
		if err == nil {
			return data
		}
	}
	data, err := json.Marshal(applyPatchProxyInput{RawPatch: input})
	if err != nil {
		return json.RawMessage(`{"raw_patch":""}`)
	}
	return data
}

func buildApplyPatchInput(operations []applyPatchOperation) string {
	var builder strings.Builder
	builder.WriteString("*** Begin Patch\n")
	for _, operation := range operations {
		switch operation.Type {
		case "add_file", "add":
			builder.WriteString("*** Add File: ")
			builder.WriteString(operation.Path)
			builder.WriteByte('\n')
			for _, line := range splitPatchContentLines(operation.Content) {
				builder.WriteByte('+')
				builder.WriteString(line)
				builder.WriteByte('\n')
			}
		case "delete_file", "delete":
			builder.WriteString("*** Delete File: ")
			builder.WriteString(operation.Path)
			builder.WriteByte('\n')
		case "update_file", "update":
			builder.WriteString("*** Update File: ")
			builder.WriteString(operation.Path)
			builder.WriteByte('\n')
			if operation.MoveTo != "" {
				builder.WriteString("*** Move to: ")
				builder.WriteString(operation.MoveTo)
				builder.WriteByte('\n')
			}
			writeApplyPatchHunks(&builder, operation)
		}
	}
	builder.WriteString("*** End Patch")
	return normalizeApplyPatchInput(builder.String())
}

func writeApplyPatchHunks(builder *strings.Builder, operation applyPatchOperation) {
	if len(operation.Hunks) > 0 {
		for _, hunk := range operation.Hunks {
			if hunk.Context == "" {
				builder.WriteString("@@\n")
			} else {
				builder.WriteString("@@ ")
				builder.WriteString(hunk.Context)
				builder.WriteByte('\n')
			}
			for _, line := range hunk.Lines {
				writeApplyPatchLine(builder, line)
			}
		}
		return
	}
	if len(operation.Lines) > 0 {
		builder.WriteString("@@\n")
		for _, line := range operation.Lines {
			writeApplyPatchLine(builder, line)
		}
		return
	}
	if strings.TrimSpace(operation.Changes) == "" {
		return
	}
	changes := strings.TrimRight(operation.Changes, "\n")
	if !strings.HasPrefix(changes, "@@") {
		builder.WriteString("@@\n")
	}
	builder.WriteString(changes)
	builder.WriteByte('\n')
}

func writeApplyPatchLine(builder *strings.Builder, line applyPatchLineOp) {
	switch line.Op {
	case "add":
		builder.WriteByte('+')
	case "remove", "delete":
		builder.WriteByte('-')
	default:
		builder.WriteByte(' ')
	}
	builder.WriteString(line.Text)
	builder.WriteByte('\n')
}

func splitPatchContentLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		return []string{""}
	}
	return strings.Split(content, "\n")
}

func parseApplyPatchOperations(input string) ([]applyPatchOperation, bool) {
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "*** Begin Patch" || strings.TrimSpace(lines[len(lines)-1]) != "*** End Patch" {
		return nil, false
	}
	operations := make([]applyPatchOperation, 0)
	for index := 1; index < len(lines)-1; {
		line := lines[index]
		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			operation := applyPatchOperation{Type: "add_file", Path: strings.TrimPrefix(line, "*** Add File: ")}
			index++
			contentLines := make([]string, 0)
			for index < len(lines)-1 && !strings.HasPrefix(lines[index], "*** ") {
				contentLines = append(contentLines, strings.TrimPrefix(lines[index], "+"))
				index++
			}
			operation.Content = strings.Join(contentLines, "\n")
			operations = append(operations, operation)
		case strings.HasPrefix(line, "*** Delete File: "):
			operations = append(operations, applyPatchOperation{Type: "delete_file", Path: strings.TrimPrefix(line, "*** Delete File: ")})
			index++
		case strings.HasPrefix(line, "*** Update File: "):
			operation := applyPatchOperation{Type: "update_file", Path: strings.TrimPrefix(line, "*** Update File: ")}
			index++
			changes := make([]string, 0)
			if index < len(lines)-1 && strings.HasPrefix(lines[index], "*** Move to: ") {
				operation.MoveTo = strings.TrimPrefix(lines[index], "*** Move to: ")
				index++
			}
			for index < len(lines)-1 && !strings.HasPrefix(lines[index], "*** ") {
				changes = append(changes, lines[index])
				index++
			}
			operation.Changes = strings.Join(changes, "\n")
			operations = append(operations, operation)
		default:
			return nil, false
		}
	}
	return operations, len(operations) > 0
}

func execInputFromProxyRaw(raw json.RawMessage) string {
	var input struct {
		Source string `json:"source"`
		Input  string `json:"input"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return customToolInputFromRaw(raw)
	}
	if input.Source != "" {
		return input.Source
	}
	if input.Input != "" {
		return input.Input
	}
	return customToolInputFromRaw(raw)
}

func execProxyInputFromGrammar(input string) json.RawMessage {
	data, err := json.Marshal(map[string]string{"source": input})
	if err != nil {
		return json.RawMessage(`{"source":""}`)
	}
	return data
}

func normalizeApplyPatchInput(input string) string {
	lines := strings.Split(input, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return input
	}
	lastIndex := len(lines) - 1
	if strings.TrimSpace(lines[lastIndex]) == "+*** End Patch" {
		lines[lastIndex] = "*** End Patch"
		if lastIndex > 0 && strings.TrimSpace(lines[lastIndex-1]) == "+*** End of File" {
			lines = append(lines[:lastIndex-1], lines[lastIndex])
		}
		return strings.Join(lines, "\n")
	}
	return input
}

func namespacedToolName(namespace string, name string) string {
	if namespace == "" {
		return name
	}
	if name == "" {
		return namespace
	}
	if strings.HasSuffix(namespace, "_") || strings.HasPrefix(name, "_") {
		return namespace + name
	}
	return namespace + "_" + name
}

func (bridge *Bridge) convertToolChoice(raw json.RawMessage) (anthropic.ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return anthropic.ToolChoice{}, nil
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
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
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
	}, bridge.registry)
	return planner.Plan(cache.PlanInput{
		ProviderID:        "anthropic",
		UpstreamAPIKeyID:  "configured-provider-key",
		Model:             converted.Model,
		PromptCacheKey:    request.PromptCacheKey,
		ToolsHash:         toolsHash,
		SystemHash:        systemHash,
		MessagePrefixHash: messagesHash,
		ToolCount:         len(converted.Tools),
		SystemBlockCount:  len(converted.System),
		MessageCount:      len(converted.Messages),
		EstimatedTokens:   estimateTokens(converted),
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
				request.Tools[len(request.Tools)-1].CacheControl = cacheControl
			}
		case "system":
			if len(request.System) > 0 {
				request.System[len(request.System)-1].CacheControl = cacheControl
			}
		case "messages":
			if len(request.Messages) > 0 {
				messageIndex := len(request.Messages) - 1
				contentIndex := len(request.Messages[messageIndex].Content) - 1
				if contentIndex >= 0 {
					request.Messages[messageIndex].Content[contentIndex].CacheControl = cacheControl
				}
			}
		}
	}
}

type inputItem struct {
	Type      string             `json:"type"`
	ID        string             `json:"id"`
	Role      string             `json:"role"`
	Content   json.RawMessage    `json:"content"`
	CallID    string             `json:"call_id"`
	Name      string             `json:"name"`
	Arguments string             `json:"arguments"`
	Input     string             `json:"input"`
	Action    *openai.ToolAction `json:"action"`
	Output    string             `json:"output"`
}

func contentBlocksFromRaw(raw json.RawMessage) []anthropic.ContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return []anthropic.ContentBlock{{Type: "text", Text: ""}}
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		_ = json.Unmarshal(raw, &text)
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
				blocks = append(blocks, anthropic.ContentBlock{Type: "text", Text: part.Text})
			}
		}
		if len(blocks) > 0 {
			return blocks
		}
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

func statusFromStopReason(stopReason string) (string, *openai.IncompleteDetails) {
	switch stopReason {
	case "max_tokens":
		return "incomplete", &openai.IncompleteDetails{Reason: "max_output_tokens"}
	case "model_context_window":
		return "incomplete", &openai.IncompleteDetails{Reason: "max_input_tokens"}
	case "pause_turn":
		return "incomplete", &openai.IncompleteDetails{Reason: "provider_pause"}
	default:
		return "completed", nil
	}
}

func normalizeUsage(usage anthropic.Usage) openai.Usage {
	inputTokens := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	outputTokens := usage.OutputTokens
	return openai.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		InputTokensDetails: openai.InputTokensDetails{
			CachedTokens: usage.CacheReadInputTokens,
		},
	}
}

func estimateTokens(request anthropic.MessageRequest) int {
	data, _ := json.Marshal(request)
	if len(data) == 0 {
		return 0
	}
	return len(data)/4 + 1
}

func responseID(providerID string) string {
	if providerID == "" {
		return "resp_generated"
	}
	if strings.HasPrefix(providerID, "resp_") {
		return providerID
	}
	return "resp_" + providerID
}

func invalidRequest(message, param, code string) error {
	return &RequestError{Status: http.StatusBadRequest, Message: message, Param: param, Code: code}
}

func localShellSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			"working_directory": map[string]any{"type": "string"},
			"timeout_ms":        map[string]any{"type": "integer"},
			"env": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
			},
		},
		"required": []string{"command"},
	}
}

func localShellActionFromRaw(raw json.RawMessage) *openai.ToolAction {
	var input struct {
		Command          []string          `json:"command"`
		WorkingDirectory string            `json:"working_directory"`
		TimeoutMS        int               `json:"timeout_ms"`
		Env              map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return &openai.ToolAction{Type: "exec"}
	}
	return &openai.ToolAction{
		Type:             "exec",
		Command:          input.Command,
		WorkingDirectory: input.WorkingDirectory,
		TimeoutMS:        input.TimeoutMS,
		Env:              input.Env,
	}
}

func localShellInputFromAction(action *openai.ToolAction) json.RawMessage {
	if action == nil {
		return json.RawMessage(`{"command":[]}`)
	}
	data, err := json.Marshal(map[string]any{
		"command":           action.Command,
		"working_directory": action.WorkingDirectory,
		"timeout_ms":        action.TimeoutMS,
		"env":               action.Env,
	})
	if err != nil {
		return json.RawMessage(`{"command":[]}`)
	}
	return data
}

func webSearchItemID(providerID string) string {
	if providerID == "" {
		return "ws_generated"
	}
	if strings.HasPrefix(providerID, "ws_") {
		return providerID
	}
	return "ws_" + providerID
}

func webSearchActionFromRaw(raw json.RawMessage) *openai.ToolAction {
	action := &openai.ToolAction{Type: "search"}
	if len(raw) == 0 || string(raw) == "null" {
		return action
	}
	var input struct {
		Type    string   `json:"type"`
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
		URL     string   `json:"url"`
		Pattern string   `json:"pattern"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return action
	}
	if input.Type != "" {
		action.Type = input.Type
	}
	action.Query = input.Query
	action.Queries = input.Queries
	action.URL = input.URL
	action.Pattern = input.Pattern
	if action.Type == "" {
		action.Type = "search"
	}
	return action
}

func hasWebSearchActionDetails(action *openai.ToolAction) bool {
	if action == nil {
		return false
	}
	return action.Query != "" || len(action.Queries) > 0 || action.URL != "" || action.Pattern != ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
