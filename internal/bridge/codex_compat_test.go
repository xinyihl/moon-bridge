package bridge_test

import (
	"encoding/json"
	"strings"
	"testing"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/openai"
)

func mustMarshalRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return data
}

type applyPatchHistoryInput struct {
	Operations []struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"operations"`
}

func applyPatchGrammarForTest() string {
	return "start: begin_patch hunk+ end_patch\nbegin_patch: \"*** Begin Patch\" LF\nend_patch: \"*** End Patch\" LF?\nadd_hunk: \"*** Add File: \" filename LF add_line+\ndelete_hunk: \"*** Delete File: \" filename LF\nupdate_hunk: \"*** Update File: \" filename LF\nfilename: /(.+)/\nadd_line: \"+\" /(.*)/ LF -> line\n"
}

func TestToAnthropicAcceptsCodexLocalShellTool(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`[
			{"role":"user","content":[{"type":"input_text","text":"list files"}]}
		]`),
		Tools: []openai.Tool{{Type: "local_shell"}},
	}
	parallel := false
	request.ParallelToolCalls = &parallel
	request.Reasoning = map[string]any{"effort": "medium"}
	request.Include = []string{"reasoning.encrypted_content"}
	request.ClientMetadata = map[string]any{"originator": "codex_cli"}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Tools) != 1 {
		t.Fatalf("tools = %+v", converted.Tools)
	}
	tool := converted.Tools[0]
	if tool.Name != "local_shell" {
		t.Fatalf("tool name = %q", tool.Name)
	}
	if tool.InputSchema["type"] != "object" {
		t.Fatalf("tool schema = %+v", tool.InputSchema)
	}
	if converted.Messages[0].Content[0].Text != "list files" {
		t.Fatalf("messages = %+v", converted.Messages)
	}
}

func TestToAnthropicIgnoresCodexNativeBuiltInTools(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`"hello"`),
		Tools: []openai.Tool{
			{Type: "local_shell"},
			{Type: "file_search"},
		},
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Tools) != 1 {
		t.Fatalf("tools = %+v", converted.Tools)
	}
	if converted.Tools[0].Name != "local_shell" {
		t.Fatalf("tool = %+v", converted.Tools[0])
	}
}

func TestToAnthropicConvertsCodexWebSearchTool(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`"search the web"`),
		Tools: []openai.Tool{
			{Type: "web_search", SearchContentTypes: []string{"text", "image"}},
		},
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Tools) != 1 {
		t.Fatalf("tools = %+v", converted.Tools)
	}
	tool := converted.Tools[0]
	if tool.Name != "web_search" || tool.Type != "web_search_20250305" || tool.MaxUses != 8 {
		t.Fatalf("web search tool = %+v", tool)
	}
	if tool.InputSchema != nil {
		t.Fatalf("InputSchema = %+v", tool.InputSchema)
	}
}

func TestToAnthropicKeepsCodexWebSearchToolForModelDecision(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`[
			{"role":"user","content":[{"type":"input_text","text":"根据知识库写一份 README 使用指南"}],"type":"message"}
		]`),
		Tools: []openai.Tool{
			{Type: "web_search", SearchContentTypes: []string{"text", "image"}},
			{Type: "function", Name: "list_mcp_resources", Parameters: map[string]any{"type": "object"}},
		},
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Tools) != 2 {
		t.Fatalf("tools = %+v", converted.Tools)
	}
	if converted.Tools[0].Name != "web_search" || converted.Tools[0].Type != "web_search_20250305" {
		t.Fatalf("web search tool = %+v", converted.Tools[0])
	}
	if converted.Tools[1].Name != "list_mcp_resources" {
		t.Fatalf("tool = %+v", converted.Tools[1])
	}
}

func TestToAnthropicPreservesCustomToolGrammar(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`"rewrite buffer"`),
		Tools: []openai.Tool{{
			Type:        "custom",
			Name:        "rewrite_buffer",
			Description: "Rewrite a buffer using freeform text.",
			Format: map[string]any{
				"type":       "grammar",
				"syntax":     "lark",
				"definition": "start: line+\nline: /.+/ NEWLINE\n%import common.NEWLINE\n",
			},
		}},
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Tools) != 1 {
		t.Fatalf("tools = %+v", converted.Tools)
	}
	tool := converted.Tools[0]
	if tool.Name != "rewrite_buffer" {
		t.Fatalf("tool = %+v", tool)
	}
	inputSchema, ok := tool.InputSchema["properties"].(map[string]any)["input"].(map[string]any)
	if !ok {
		t.Fatalf("input schema = %+v", tool.InputSchema)
	}
	description, _ := inputSchema["description"].(string)
	if !strings.Contains(tool.Description, "start: line+") || !strings.Contains(description, "start: line+") {
		t.Fatalf("custom grammar was not preserved: tool=%q input=%q", tool.Description, description)
	}
}

func TestFromAnthropicMapsRequestCustomToolToCustomToolCall(t *testing.T) {
	input := "replace this buffer\nwith this text\n"
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Tools: []openai.Tool{{Type: "custom", Name: "rewrite_buffer"}},
	}
	response := anthropic.MessageResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       "assistant",
		StopReason: "tool_use",
		Content: []anthropic.ContentBlock{{
			Type:  "tool_use",
			ID:    "tool_rewrite",
			Name:  "rewrite_buffer",
			Input: mustMarshalRaw(t, map[string]any{"input": input}),
		}},
	}

	bridgeUnderTest := testBridge()
	converted := bridgeUnderTest.FromAnthropicWithContext(response, "gpt-test", bridgeUnderTest.ConversionContext(request))
	if len(converted.Output) != 1 {
		t.Fatalf("output = %+v", converted.Output)
	}
	item := converted.Output[0]
	if item.Type != "custom_tool_call" || item.CallID != "tool_rewrite" || item.Name != "rewrite_buffer" || item.Input != input {
		t.Fatalf("custom tool call = %+v", item)
	}
	if item.Arguments != "" {
		t.Fatalf("Arguments = %q, want empty for custom tool", item.Arguments)
	}
}

func TestFromAnthropicKeepsUnregisteredToolUseAsFunctionCall(t *testing.T) {
	response := anthropic.MessageResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       "assistant",
		StopReason: "tool_use",
		Content: []anthropic.ContentBlock{{
			Type:  "tool_use",
			ID:    "tool_lookup",
			Name:  "lookup",
			Input: json.RawMessage(`{"id":"42"}`),
		}},
	}

	converted := testBridge().FromAnthropicWithContext(response, "gpt-test", testBridge().ConversionContext(openai.ResponsesRequest{
		Model: "gpt-test",
		Tools: []openai.Tool{{Type: "custom", Name: "rewrite_buffer"}},
	}))
	if len(converted.Output) != 1 {
		t.Fatalf("output = %+v", converted.Output)
	}
	item := converted.Output[0]
	if item.Type != "function_call" || item.Name != "lookup" || item.Arguments != `{"id":"42"}` {
		t.Fatalf("function call = %+v", item)
	}
}

func TestFromAnthropicNormalizesApplyPatchGrammarTerminator(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: docs/api.md\n+# API\n+content\n+*** End of File\n+*** End Patch"
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Tools: []openai.Tool{{
			Type: "custom",
			Name: "patcher",
			Format: map[string]any{
				"type":       "grammar",
				"syntax":     "lark",
				"definition": "begin_patch: \"*** Begin Patch\"\nend_patch: \"*** End Patch\"\nadd_hunk: \"*** Add File: \"\n",
			},
		}},
	}
	response := anthropic.MessageResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       "assistant",
		StopReason: "tool_use",
		Content: []anthropic.ContentBlock{{
			Type:  "tool_use",
			ID:    "tool_patch",
			Name:  "patcher",
			Input: mustMarshalRaw(t, map[string]any{"input": input}),
		}},
	}

	bridgeUnderTest := testBridge()
	converted := bridgeUnderTest.FromAnthropicWithContext(response, "gpt-test", bridgeUnderTest.ConversionContext(request))
	if len(converted.Output) != 1 {
		t.Fatalf("output = %+v", converted.Output)
	}
	got := converted.Output[0].Input
	if strings.Contains(got, "+*** End Patch") || strings.Contains(got, "+*** End of File") {
		t.Fatalf("patch terminators were not normalized: %q", got)
	}
	if !strings.HasSuffix(got, "*** End Patch") {
		t.Fatalf("patch = %q", got)
	}
}

func TestToAnthropicConvertsApplyPatchGrammarToolToSchemaProxy(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`"write docs"`),
		Tools: []openai.Tool{{
			Type:   "custom",
			Name:   "apply_patch",
			Format: map[string]any{"type": "grammar", "syntax": "lark", "definition": applyPatchGrammarForTest()},
		}},
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Tools) != 1 {
		t.Fatalf("tools = %+v", converted.Tools)
	}
	properties, ok := converted.Tools[0].InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema = %+v", converted.Tools[0].InputSchema)
	}
	if _, ok := properties["operations"]; !ok {
		t.Fatalf("operations schema missing: %+v", properties)
	}
	if _, ok := properties["input"]; ok {
		t.Fatalf("raw input hole should not be the primary apply_patch schema: %+v", properties)
	}
	if _, ok := properties["raw_patch"]; ok {
		t.Fatalf("raw patch should not be exposed in apply_patch schema: %+v", properties)
	}
	required, ok := converted.Tools[0].InputSchema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "operations" {
		t.Fatalf("required schema = %+v", converted.Tools[0].InputSchema["required"])
	}
}

func TestFromAnthropicBuildsApplyPatchGrammarFromProxyOperations(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Tools: []openai.Tool{{
			Type:   "custom",
			Name:   "apply_patch",
			Format: map[string]any{"type": "grammar", "syntax": "lark", "definition": applyPatchGrammarForTest()},
		}},
	}
	response := anthropic.MessageResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       "assistant",
		StopReason: "tool_use",
		Content: []anthropic.ContentBlock{{
			Type: "tool_use",
			ID:   "tool_patch",
			Name: "apply_patch",
			Input: mustMarshalRaw(t, map[string]any{"operations": []map[string]any{{
				"type":    "add_file",
				"path":    "docs/api.md",
				"content": "# API\ncontent\n",
			}}}),
		}},
	}

	bridgeUnderTest := testBridge()
	converted := bridgeUnderTest.FromAnthropicWithContext(response, "gpt-test", bridgeUnderTest.ConversionContext(request))
	if len(converted.Output) != 1 {
		t.Fatalf("output = %+v", converted.Output)
	}
	want := "*** Begin Patch\n*** Add File: docs/api.md\n+# API\n+content\n*** End Patch"
	if converted.Output[0].Input != want {
		t.Fatalf("patch = %q, want %q", converted.Output[0].Input, want)
	}
}

func TestToAnthropicConvertsApplyPatchHistoryToProxyOperations(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: docs/api.md\n+# API\n+content\n*** End Patch"
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: mustMarshalRaw(t, []map[string]any{{
			"type":    "custom_tool_call",
			"call_id": "tool_patch",
			"name":    "apply_patch",
			"input":   patch,
		}}),
		Tools: []openai.Tool{{
			Type:   "custom",
			Name:   "apply_patch",
			Format: map[string]any{"type": "grammar", "syntax": "lark", "definition": applyPatchGrammarForTest()},
		}},
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	var input applyPatchHistoryInput
	if err := json.Unmarshal(converted.Messages[0].Content[0].Input, &input); err != nil {
		t.Fatalf("tool input JSON = %s: %v", string(converted.Messages[0].Content[0].Input), err)
	}
	if len(input.Operations) != 1 || input.Operations[0].Type != "add_file" || input.Operations[0].Path != "docs/api.md" || input.Operations[0].Content != "# API\ncontent" {
		t.Fatalf("proxy operations = %+v", input.Operations)
	}
}

func TestToAnthropicConvertsExecGrammarToolToSourceSchema(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`"run js"`),
		Tools: []openai.Tool{{
			Type:   "custom",
			Name:   "exec",
			Format: map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: pragma_source | plain_source\npragma_source: \"// @exec:\" /.+/\nplain_source: /(.|\\n)+/\n"},
		}},
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	properties := converted.Tools[0].InputSchema["properties"].(map[string]any)
	if _, ok := properties["source"]; !ok {
		t.Fatalf("source schema missing: %+v", properties)
	}
}

func TestFromAnthropicBuildsExecGrammarFromSourceProxy(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Tools: []openai.Tool{{
			Type:   "custom",
			Name:   "exec",
			Format: map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: pragma_source | plain_source\npragma_source: \"// @exec:\" /.+/\nplain_source: /(.|\\n)+/\n"},
		}},
	}
	response := anthropic.MessageResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       "assistant",
		StopReason: "tool_use",
		Content: []anthropic.ContentBlock{{
			Type:  "tool_use",
			ID:    "tool_exec",
			Name:  "exec",
			Input: mustMarshalRaw(t, map[string]any{"source": "console.log(42);\n"}),
		}},
	}

	bridgeUnderTest := testBridge()
	converted := bridgeUnderTest.FromAnthropicWithContext(response, "gpt-test", bridgeUnderTest.ConversionContext(request))
	if converted.Output[0].Input != "console.log(42);\n" {
		t.Fatalf("exec input = %q", converted.Output[0].Input)
	}
}

func TestToAnthropicConvertsCustomToolCallHistoryToJSONInput(t *testing.T) {
	inputText := "replace this buffer\nwith this text\n"
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: mustMarshalRaw(t, []map[string]any{
			{"type": "custom_tool_call", "call_id": "tool_rewrite", "name": "rewrite_buffer", "input": inputText},
			{"type": "custom_tool_call_output", "call_id": "tool_rewrite", "output": "rewritten\n"},
		}),
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Messages) != 2 {
		t.Fatalf("messages = %+v", converted.Messages)
	}
	toolUse := converted.Messages[0].Content[0]
	if toolUse.Type != "tool_use" || toolUse.Name != "rewrite_buffer" || toolUse.ID != "tool_rewrite" {
		t.Fatalf("tool use = %+v", toolUse)
	}
	var input struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(toolUse.Input, &input); err != nil {
		t.Fatalf("tool input is invalid JSON: %s: %v", string(toolUse.Input), err)
	}
	if input.Input != inputText {
		t.Fatalf("input = %q", input.Input)
	}
	if converted.Messages[1].Content[0].ToolUseID != "tool_rewrite" {
		t.Fatalf("tool result = %+v", converted.Messages[1].Content[0])
	}
}

func TestToAnthropicConvertsNamespacedCustomToolWithContext(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`"rewrite buffer"`),
		Tools: []openai.Tool{{
			Type: "namespace",
			Name: "mcp__editor__",
			Tools: []openai.Tool{{
				Type:        "custom",
				Name:        "rewrite_buffer",
				Description: "Rewrite a buffer using freeform text.",
				Format: map[string]any{
					"type":       "grammar",
					"syntax":     "lark",
					"definition": "start: line+\n",
				},
			}},
		}},
	}

	bridgeUnderTest := testBridge()
	convertedRequest, _, err := bridgeUnderTest.ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(convertedRequest.Tools) != 1 || convertedRequest.Tools[0].Name != "mcp__editor__rewrite_buffer" {
		t.Fatalf("tools = %+v", convertedRequest.Tools)
	}

	response := anthropic.MessageResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       "assistant",
		StopReason: "tool_use",
		Content: []anthropic.ContentBlock{{
			Type:  "tool_use",
			ID:    "tool_rewrite",
			Name:  "mcp__editor__rewrite_buffer",
			Input: mustMarshalRaw(t, map[string]any{"input": "new text\n"}),
		}},
	}
	convertedResponse := bridgeUnderTest.FromAnthropicWithContext(response, "gpt-test", bridgeUnderTest.ConversionContext(request))
	if len(convertedResponse.Output) != 1 || convertedResponse.Output[0].Type != "custom_tool_call" {
		t.Fatalf("output = %+v", convertedResponse.Output)
	}
}

func TestToAnthropicFlattensCodexNamespaceTools(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`"hello"`),
		Tools: []openai.Tool{
			{
				Type:        "namespace",
				Name:        "mcp__deepwiki__",
				Description: "DeepWiki tools",
				Tools: []openai.Tool{
					{
						Type:        "function",
						Name:        "ask_question",
						Description: "Ask a repository question",
						Parameters: map[string]any{
							"type":     "object",
							"required": []string{"repoName", "question"},
							"properties": map[string]any{
								"repoName": map[string]any{"type": "string"},
								"question": map[string]any{"type": "string"},
							},
						},
					},
					{
						Type: "function",
						Name: "read_wiki_structure",
					},
				},
			},
		},
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Tools) != 2 {
		t.Fatalf("tools = %+v", converted.Tools)
	}
	if converted.Tools[0].Name != "mcp__deepwiki__ask_question" {
		t.Fatalf("first tool name = %q", converted.Tools[0].Name)
	}
	if converted.Tools[0].Description != "Ask a repository question" {
		t.Fatalf("first tool description = %q", converted.Tools[0].Description)
	}
	if converted.Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("first tool schema = %+v", converted.Tools[0].InputSchema)
	}
	if converted.Tools[1].Name != "mcp__deepwiki__read_wiki_structure" {
		t.Fatalf("second tool name = %q", converted.Tools[1].Name)
	}
	if converted.Tools[1].InputSchema["type"] != "object" {
		t.Fatalf("second tool schema = %+v", converted.Tools[1].InputSchema)
	}
}

func TestFromAnthropicMapsLocalShellToolUseForCodex(t *testing.T) {
	response := anthropic.MessageResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       "assistant",
		StopReason: "tool_use",
		Content: []anthropic.ContentBlock{
			{
				Type:  "tool_use",
				ID:    "toolu_shell",
				Name:  "local_shell",
				Input: json.RawMessage(`{"command":["bash","-lc","pwd"],"working_directory":"/tmp","timeout_ms":1000}`),
			},
		},
	}

	converted := testBridge().FromAnthropic(response, "gpt-test")
	if len(converted.Output) != 1 {
		t.Fatalf("output = %+v", converted.Output)
	}
	item := converted.Output[0]
	if item.Type != "local_shell_call" {
		t.Fatalf("item type = %q", item.Type)
	}
	if item.CallID != "toolu_shell" {
		t.Fatalf("call_id = %q", item.CallID)
	}
	if item.Action == nil || len(item.Action.Command) != 3 || item.Action.Command[2] != "pwd" {
		t.Fatalf("action = %+v", item.Action)
	}
}

func TestFromAnthropicMapsWebSearchServerToolUseForCodex(t *testing.T) {
	response := anthropic.MessageResponse{
		ID:         "msg_123",
		Type:       "message",
		Role:       "assistant",
		StopReason: "end_turn",
		Content: []anthropic.ContentBlock{
			{
				Type:  "server_tool_use",
				ID:    "srvtoolu_123",
				Name:  "web_search",
				Input: json.RawMessage(`{"type":"search","query":"Kimi K2.6","queries":["Kimi K2.6","Moonshot K2.6"]}`),
			},
			{Type: "web_search_tool_result", ToolUseID: "srvtoolu_123", Content: []any{
				map[string]any{"type": "web_search_result", "url": "https://example.test", "title": "Example"},
			}},
			{Type: "text", Text: "Found results."},
		},
	}

	converted := testBridge().FromAnthropic(response, "gpt-test")
	if len(converted.Output) != 2 {
		t.Fatalf("output = %+v", converted.Output)
	}
	item := converted.Output[0]
	if item.Type != "web_search_call" || item.ID != "ws_srvtoolu_123" || item.Status != "completed" {
		t.Fatalf("web search item = %+v", item)
	}
	if item.Action == nil || item.Action.Type != "search" || item.Action.Query != "Kimi K2.6" || len(item.Action.Queries) != 2 {
		t.Fatalf("web search action = %+v", item.Action)
	}
	if converted.Output[1].Type != "message" || converted.OutputText != "Found results." {
		t.Fatalf("message output = %+v text=%q", converted.Output[1], converted.OutputText)
	}
}

func TestToAnthropicConvertsCodexLocalShellHistoryAndOutput(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`[
			{"type":"local_shell_call","id":"lc_1","call_id":"toolu_shell","action":{"type":"exec","command":["bash","-lc","pwd"]}},
			{"type":"local_shell_call_output","call_id":"toolu_shell","output":"/repo\n"}
		]`),
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if converted.Messages[0].Role != "assistant" || converted.Messages[0].Content[0].Type != "tool_use" {
		t.Fatalf("assistant history = %+v", converted.Messages[0])
	}
	if converted.Messages[1].Role != "user" || converted.Messages[1].Content[0].Type != "tool_result" {
		t.Fatalf("tool output = %+v", converted.Messages[1])
	}
}

func TestToAnthropicGroupsParallelFunctionCallsBeforeOutputs(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`[
			{"role":"user","content":[{"type":"input_text","text":"inspect project"}],"type":"message"},
			{"arguments":"{\"cmd\":\"find . -maxdepth 2 -type f\"}","call_id":"tool_find","name":"exec_command","type":"function_call"},
			{"arguments":"{\"cmd\":\"ls -la\"}","call_id":"tool_ls","name":"exec_command","type":"function_call"},
			{"call_id":"tool_find","output":"go.mod\nREADME.md\n","type":"function_call_output"},
			{"call_id":"tool_ls","output":"total 8\n","type":"function_call_output"}
		]`),
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Messages) != 3 {
		t.Fatalf("messages = %+v", converted.Messages)
	}
	assistant := converted.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 2 {
		t.Fatalf("assistant tool calls = %+v", assistant)
	}
	if assistant.Content[0].Type != "tool_use" || assistant.Content[0].ID != "tool_find" {
		t.Fatalf("first tool call = %+v", assistant.Content[0])
	}
	if assistant.Content[1].Type != "tool_use" || assistant.Content[1].ID != "tool_ls" {
		t.Fatalf("second tool call = %+v", assistant.Content[1])
	}
	results := converted.Messages[2]
	if results.Role != "user" || len(results.Content) != 2 {
		t.Fatalf("tool results = %+v", results)
	}
	if results.Content[0].Type != "tool_result" || results.Content[0].ToolUseID != "tool_find" {
		t.Fatalf("first tool result = %+v", results.Content[0])
	}
	if results.Content[1].Type != "tool_result" || results.Content[1].ToolUseID != "tool_ls" {
		t.Fatalf("second tool result = %+v", results.Content[1])
	}
}

func TestToAnthropicMergesAssistantTextWithFollowingToolCall(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`[
			{"role":"user","content":[{"type":"input_text","text":"inspect project"}],"type":"message"},
			{"role":"assistant","content":[{"type":"output_text","text":"I will inspect the tree."}],"type":"message"},
			{"arguments":"{\"cmd\":\"find . -maxdepth 2 -type f\"}","call_id":"tool_find","name":"exec_command","type":"function_call"},
			{"call_id":"tool_find","output":"go.mod\nREADME.md\n","type":"function_call_output"}
		]`),
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Messages) != 3 {
		t.Fatalf("messages = %+v", converted.Messages)
	}
	assistant := converted.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 2 {
		t.Fatalf("assistant = %+v", assistant)
	}
	if assistant.Content[0].Type != "text" || assistant.Content[1].Type != "tool_use" {
		t.Fatalf("assistant content = %+v", assistant.Content)
	}
	if converted.Messages[2].Content[0].ToolUseID != "tool_find" {
		t.Fatalf("tool result = %+v", converted.Messages[2])
	}
}

func TestToAnthropicSkipsCodexWebSearchHistoryItems(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`[
			{"role":"user","content":[{"type":"input_text","text":"search news"}],"type":"message"},
			{"id":"ws_123","type":"web_search_call","status":"completed","action":{"type":"search","query":"Kimi K2.6"}},
			{"role":"assistant","content":[{"type":"output_text","text":"I found news."}],"type":"message"},
			{"role":"user","content":[{"type":"input_text","text":"summarize it"}],"type":"message"}
		]`),
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Messages) != 3 {
		t.Fatalf("messages = %+v", converted.Messages)
	}
	for _, message := range converted.Messages {
		if len(message.Content) == 1 && message.Content[0].Text == "" {
			t.Fatalf("unexpected empty message from web_search_call history: %+v", converted.Messages)
		}
	}
}

func TestToAnthropicSkipsEmptyWebSearchPreludeHistory(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`[
			{"role":"user","content":[{"type":"input_text","text":"根据知识库写 README"}],"type":"message"},
			{"role":"assistant","content":[{"type":"output_text","text":"Search results for query: "}],"type":"message"},
			{"type":"web_search_call","status":"completed","action":{"type":"search"}},
			{"type":"function_call","call_id":"tool_1","name":"list_mcp_resources","arguments":"{\"server\":\"deepwiki\"}"},
			{"type":"function_call_output","call_id":"tool_1","output":"{\"resources\":[]}"}
		]`),
	}

	converted, _, err := testBridge().ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	for _, message := range converted.Messages {
		for _, block := range message.Content {
			if block.Text == "Search results for query: " {
				t.Fatalf("dirty search prelude was preserved: %+v", converted.Messages)
			}
		}
	}
}
