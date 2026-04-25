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
