package bridge_test

import (
	"encoding/json"
	"testing"

	"moonbridge/internal/anthropic"
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
			{Type: "web_search"},
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
