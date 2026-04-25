package bridge_test

import (
	"encoding/json"
	"testing"

	"moonbridge/internal/openai"
)

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

func TestToAnthropicSkipsCodexWebSearchToolWhenProviderDisabled(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`"search the web"`),
		Tools: []openai.Tool{
			{Type: "web_search", SearchContentTypes: []string{"text", "image"}},
			{Type: "local_shell"},
		},
	}

	bridgeUnderTest := testBridgeWithWebSearchDisabled()
	converted, _, err := bridgeUnderTest.ToAnthropic(request)
	if err != nil {
		t.Fatalf("ToAnthropic() error = %v", err)
	}
	if len(converted.Tools) != 1 || converted.Tools[0].Name != "local_shell" {
		t.Fatalf("tools = %+v", converted.Tools)
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

func TestToAnthropicSkipsEmptyAssistantMessageBeforeToolCall(t *testing.T) {
	request := openai.ResponsesRequest{
		Model: "gpt-test",
		Input: json.RawMessage(`[
			{"role":"user","content":[{"type":"input_text","text":"inspect trace"}],"type":"message"},
			{"role":"assistant","content":[{"type":"output_text","text":""}],"type":"message"},
			{"arguments":"{\"cmd\":\"ls trace\"}","call_id":"call_ls","name":"exec_command","type":"function_call"},
			{"call_id":"call_ls","output":"trace/Transform\n","type":"function_call_output"}
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
	if assistant.Role != "assistant" || len(assistant.Content) != 1 {
		t.Fatalf("assistant = %+v", assistant)
	}
	if assistant.Content[0].Type != "tool_use" || assistant.Content[0].Name != "exec_command" {
		t.Fatalf("assistant content = %+v", assistant.Content)
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
