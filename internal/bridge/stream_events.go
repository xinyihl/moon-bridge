package bridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/openai"
)

func (converter *streamConverter) contentBlockStart(event anthropic.StreamEvent) []openai.StreamEvent {
	index := event.Index
	block := event.ContentBlock
	if block == nil {
		return nil
	}
	switch block.Type {
	case "text":
		converter.itemIDs[index] = fmt.Sprintf("msg_item_%d", index)
		converter.contentText[index] = ""
		return nil
	case "tool_use":
		if block.Name == "local_shell" {
			item := openai.OutputItem{
				Type:   "local_shell_call",
				ID:     "lc_" + block.ID,
				CallID: block.ID,
				Status: "in_progress",
				Action: localShellActionFromRaw(block.Input),
			}
			converter.itemIDs[index] = item.ID
			converter.addOutput(index, item)
			return []openai.StreamEvent{converter.outputItem("response.output_item.added", index, item)}
		}
		if converter.context.IsCustomTool(block.Name) {
			item := openai.OutputItem{
				Type:   "custom_tool_call",
				ID:     customToolItemID(block.ID),
				CallID: block.ID,
				Name:   converter.context.OpenAINameForCustomTool(block.Name),
				Input:  "",
				Status: "in_progress",
			}
			converter.itemIDs[index] = item.ID
			converter.customToolInputs[index] = ""
			converter.customToolNames[index] = block.Name
			if len(block.Input) > 0 && string(block.Input) != "{}" {
				converter.customToolInitialInputs[index] = string(block.Input)
			}
			converter.addOutput(index, item)
			return []openai.StreamEvent{converter.outputItem("response.output_item.added", index, item)}
		}
		item := openai.OutputItem{
			Type:      "function_call",
			ID:        "fc_" + block.ID,
			CallID:    block.ID,
			Name:      block.Name,
			Arguments: "",
			Status:    "in_progress",
		}
		converter.itemIDs[index] = item.ID
		converter.addOutput(index, item)
		return []openai.StreamEvent{converter.outputItem("response.output_item.added", index, item)}
	case "server_tool_use":
		if block.Name != "web_search" {
			return nil
		}
		converter.itemIDs[index] = webSearchItemID(block.ID)
		converter.webSearchActions[index] = webSearchActionFromRaw(block.Input)
		return nil
	}
	return nil
}

func (converter *streamConverter) contentBlockDelta(event anthropic.StreamEvent) []openai.StreamEvent {
	index := event.Index
	switch event.Delta.Type {
	case "text_delta":
		if event.Delta.Text == "" {
			return nil
		}
		current := converter.contentText[index]
		converter.contentText[index] = current + event.Delta.Text
		if isEmptyWebSearchPrelude(converter.contentText[index]) {
			return nil
		}
		delta := event.Delta.Text
		var events []openai.StreamEvent
		if !converter.hasOutput(index) {
			item := openai.OutputItem{
				Type:    "message",
				ID:      converter.itemIDs[index],
				Status:  "in_progress",
				Role:    "assistant",
				Content: []openai.ContentPart{},
			}
			converter.addOutput(index, item)
			events = append(events,
				converter.outputItem("response.output_item.added", index, item),
				converter.contentPart("response.content_part.added", index, 0, openai.ContentPart{Type: "output_text"}),
			)
			if current != "" {
				delta = converter.contentText[index]
			}
		}
		events = append(events, openai.StreamEvent{
			Event: "response.output_text.delta",
			Data: openai.OutputTextDeltaEvent{
				Type:           "response.output_text.delta",
				SequenceNumber: converter.next(),
				ItemID:         converter.itemIDs[index],
				OutputIndex:    converter.outputIndex(index),
				ContentIndex:   0,
				Delta:          delta,
			},
		})
		return events
	case "input_json_delta":
		if _, ok := converter.webSearchActions[index]; ok {
			converter.webSearchInputs[index] += event.Delta.PartialJSON
			return nil
		}
		if _, ok := converter.customToolInputs[index]; ok {
			converter.customToolInputs[index] += event.Delta.PartialJSON
			return nil
		}
		converter.toolArguments[index] += event.Delta.PartialJSON
		return []openai.StreamEvent{{
			Event: "response.function_call_arguments.delta",
			Data: openai.FunctionCallArgumentsDeltaEvent{
				Type:           "response.function_call_arguments.delta",
				SequenceNumber: converter.next(),
				ItemID:         converter.itemIDs[index],
				OutputIndex:    converter.outputIndex(index),
				Delta:          event.Delta.PartialJSON,
			},
		}}
	}
	return nil
}

func (converter *streamConverter) contentBlockStop(event anthropic.StreamEvent) []openai.StreamEvent {
	index := event.Index
	if text, ok := converter.contentText[index]; ok {
		if isEmptyWebSearchPrelude(text) {
			return nil
		}
		var events []openai.StreamEvent
		if !converter.hasOutput(index) {
			if text == "" {
				return nil
			}
			item := openai.OutputItem{
				Type:    "message",
				ID:      converter.itemIDs[index],
				Status:  "in_progress",
				Role:    "assistant",
				Content: []openai.ContentPart{},
			}
			converter.addOutput(index, item)
			events = append(events,
				converter.outputItem("response.output_item.added", index, item),
				converter.contentPart("response.content_part.added", index, 0, openai.ContentPart{Type: "output_text"}),
			)
		}
		converter.response.OutputText += text
		item := openai.OutputItem{
			Type:    "message",
			ID:      converter.itemIDs[index],
			Status:  "completed",
			Role:    "assistant",
			Content: []openai.ContentPart{{Type: "output_text", Text: text}},
		}
		converter.setOutput(index, item)
		events = append(events,
			openai.StreamEvent{
				Event: "response.output_text.done",
				Data: openai.OutputTextDoneEvent{
					Type:           "response.output_text.done",
					SequenceNumber: converter.next(),
					ItemID:         converter.itemIDs[index],
					OutputIndex:    converter.outputIndex(index),
					ContentIndex:   0,
					Text:           text,
				},
			},
			converter.contentPart("response.content_part.done", index, 0, openai.ContentPart{Type: "output_text", Text: text}),
			converter.outputItem("response.output_item.done", index, item),
		)
		return events
	}
	if action, ok := converter.webSearchActions[index]; ok {
		if input := converter.webSearchInputs[index]; input != "" {
			action = webSearchActionFromRaw(json.RawMessage(compactJSON(input)))
		}
		if !hasWebSearchActionDetails(action) {
			return nil
		}
		item := openai.OutputItem{
			Type:   "web_search_call",
			ID:     converter.itemIDs[index],
			Status: "in_progress",
			Action: action,
		}
		converter.addOutput(index, item)
		added := converter.outputItem("response.output_item.added", index, item)
		item.Status = "completed"
		converter.setOutput(index, item)
		return []openai.StreamEvent{added, converter.outputItem("response.output_item.done", index, item)}
	}
	if inputJSON, ok := converter.customToolInputs[index]; ok {
		if inputJSON == "" {
			inputJSON = converter.customToolInitialInputs[index]
		}
		item := converter.outputAt(index)
		toolName := converter.customToolNames[index]
		if toolName == "" {
			toolName = item.Name
		}
		item.Type = "custom_tool_call"
		item.ID = converter.itemIDs[index]
		if item.CallID == "" {
			item.CallID = strings.TrimPrefix(converter.itemIDs[index], "ctc_")
		}
		input := converter.context.CustomToolInputFromRaw(toolName, json.RawMessage(compactJSON(inputJSON)))
		item.Name = converter.context.OpenAINameForCustomTool(toolName)
		item.Input = input
		item.Status = "completed"
		converter.setOutput(index, item)
		events := make([]openai.StreamEvent, 0, 2)
		if input != "" {
			events = append(events, openai.StreamEvent{
				Event: "response.custom_tool_call_input.delta",
				Data: openai.CustomToolCallInputDeltaEvent{
					Type:           "response.custom_tool_call_input.delta",
					SequenceNumber: converter.next(),
					ItemID:         converter.itemIDs[index],
					CallID:         item.CallID,
					OutputIndex:    converter.outputIndex(index),
					Delta:          input,
				},
			})
		}
		events = append(events, converter.outputItem("response.output_item.done", index, item))
		return events
	}
	if arguments, ok := converter.toolArguments[index]; ok {
		if strings.HasPrefix(converter.itemIDs[index], "lc_") {
			item := openai.OutputItem{
				Type:   "local_shell_call",
				ID:     converter.itemIDs[index],
				CallID: strings.TrimPrefix(converter.itemIDs[index], "lc_"),
				Action: localShellActionFromRaw(json.RawMessage(compactJSON(arguments))),
				Status: "completed",
			}
			converter.setOutput(index, item)
			return []openai.StreamEvent{
				converter.outputItem("response.output_item.done", index, item),
			}
		}
		item := converter.outputAt(index)
		item.Type = "function_call"
		item.ID = converter.itemIDs[index]
		item.CallID = strings.TrimPrefix(converter.itemIDs[index], "fc_")
		item.Arguments = compactJSON(arguments)
		item.Status = "completed"
		converter.setOutput(index, item)
		return []openai.StreamEvent{
			{
				Event: "response.function_call_arguments.done",
				Data: openai.FunctionCallArgumentsDoneEvent{
					Type:           "response.function_call_arguments.done",
					SequenceNumber: converter.next(),
					ItemID:         converter.itemIDs[index],
					OutputIndex:    converter.outputIndex(index),
					Arguments:      compactJSON(arguments),
				},
			},
			converter.outputItem("response.output_item.done", index, item),
		}
	}
	return nil
}
