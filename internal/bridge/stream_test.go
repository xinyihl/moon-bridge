package bridge_test

import (
	"encoding/json"
	"testing"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/openai"
)

func TestConvertStreamEventsConvertsTextLifecycle(t *testing.T) {
	events := []anthropic.StreamEvent{
		{Type: "message_start", Message: &anthropic.MessageResponse{ID: "msg_1", Type: "message", Role: "assistant"}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthropic.ContentBlock{Type: "text"}},
		{Type: "content_block_delta", Index: 0, Delta: anthropic.StreamDelta{Type: "text_delta", Text: "Hi"}},
		{Type: "content_block_stop", Index: 0},
		{Type: "message_delta", Delta: anthropic.StreamDelta{StopReason: "end_turn"}, Usage: &anthropic.Usage{OutputTokens: 1}},
		{Type: "message_stop"},
	}

	converted := testBridge().ConvertStreamEvents(events, "gpt-test")
	names := make([]string, 0, len(converted))
	for _, event := range converted {
		names = append(names, event.Event)
	}

	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(names) != len(want) {
		t.Fatalf("events = %v", names)
	}
	for index, wantName := range want {
		if names[index] != wantName {
			t.Fatalf("event %d = %q, want %q; all=%v", index, names[index], wantName, names)
		}
	}

	delta := converted[4].Data.(openai.OutputTextDeltaEvent)
	if delta.Delta != "Hi" {
		t.Fatalf("delta = %+v", delta)
	}
}

func TestConvertStreamEventsConvertsToolArguments(t *testing.T) {
	rawInput := json.RawMessage(`{}`)
	events := []anthropic.StreamEvent{
		{Type: "message_start", Message: &anthropic.MessageResponse{ID: "msg_1", Type: "message", Role: "assistant"}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthropic.ContentBlock{Type: "tool_use", ID: "toolu_1", Name: "lookup", Input: rawInput}},
		{Type: "content_block_delta", Index: 0, Delta: anthropic.StreamDelta{Type: "input_json_delta", PartialJSON: `{"id"`}},
		{Type: "content_block_delta", Index: 0, Delta: anthropic.StreamDelta{Type: "input_json_delta", PartialJSON: `:"42"}`}},
		{Type: "content_block_stop", Index: 0},
		{Type: "message_delta", Delta: anthropic.StreamDelta{StopReason: "tool_use"}},
		{Type: "message_stop"},
	}

	converted := testBridge().ConvertStreamEvents(events, "gpt-test")
	var done openai.FunctionCallArgumentsDoneEvent
	for _, event := range converted {
		if event.Event == "response.function_call_arguments.done" {
			done = event.Data.(openai.FunctionCallArgumentsDoneEvent)
		}
	}
	if done.Arguments != `{"id":"42"}` {
		t.Fatalf("arguments = %q", done.Arguments)
	}

	completed := streamLifecycleResponse(t, converted, "response.completed")
	if len(completed.Output) != 1 {
		t.Fatalf("completed output = %+v", completed.Output)
	}
	item := completed.Output[0]
	if item.Type != "function_call" || item.Status != "completed" || item.Name != "lookup" || item.CallID != "toolu_1" || item.Arguments != `{"id":"42"}` {
		t.Fatalf("completed function item = %+v", item)
	}
}

func TestConvertStreamEventsMarksTextOutputCompletedInFinalResponse(t *testing.T) {
	events := []anthropic.StreamEvent{
		{Type: "message_start", Message: &anthropic.MessageResponse{ID: "msg_1", Type: "message", Role: "assistant"}},
		{Type: "content_block_start", Index: 0, ContentBlock: &anthropic.ContentBlock{Type: "text"}},
		{Type: "content_block_delta", Index: 0, Delta: anthropic.StreamDelta{Type: "text_delta", Text: "Hi"}},
		{Type: "content_block_stop", Index: 0},
		{Type: "message_delta", Delta: anthropic.StreamDelta{StopReason: "end_turn"}},
		{Type: "message_stop"},
	}

	converted := testBridge().ConvertStreamEvents(events, "gpt-test")
	completed := streamLifecycleResponse(t, converted, "response.completed")
	if len(completed.Output) != 1 {
		t.Fatalf("completed output = %+v", completed.Output)
	}
	item := completed.Output[0]
	if item.Type != "message" || item.Status != "completed" || len(item.Content) != 1 || item.Content[0].Text != "Hi" {
		t.Fatalf("completed message item = %+v", item)
	}
}

func streamLifecycleResponse(t *testing.T, events []openai.StreamEvent, eventName string) openai.Response {
	t.Helper()

	for _, event := range events {
		if event.Event != eventName {
			continue
		}
		lifecycle, ok := event.Data.(openai.ResponseLifecycleEvent)
		if !ok {
			t.Fatalf("%s data = %T", eventName, event.Data)
		}
		return lifecycle.Response
	}
	t.Fatalf("%s not found in %+v", eventName, events)
	return openai.Response{}
}
