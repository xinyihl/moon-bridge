package bridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/openai"
)

func (bridge *Bridge) ConvertStreamEvents(events []anthropic.StreamEvent, model string) []openai.StreamEvent {
	converter := streamConverter{
		bridge:        bridge,
		model:         model,
		contentText:   map[int]string{},
		toolArguments: map[int]string{},
		itemIDs:       map[int]string{},
	}
	var converted []openai.StreamEvent
	for _, event := range events {
		converted = append(converted, converter.convert(event)...)
	}
	return converted
}

type streamConverter struct {
	bridge        *Bridge
	model         string
	sequence      int64
	response      openai.Response
	contentText   map[int]string
	toolArguments map[int]string
	itemIDs       map[int]string
}

func (converter *streamConverter) convert(event anthropic.StreamEvent) []openai.StreamEvent {
	switch event.Type {
	case "message_start":
		converter.response = openai.Response{
			ID:     responseID(event.Message.ID),
			Object: "response",
			Status: "in_progress",
			Model:  converter.model,
			Output: []openai.OutputItem{},
		}
		return []openai.StreamEvent{
			converter.lifecycle("response.created"),
			converter.lifecycle("response.in_progress"),
		}
	case "content_block_start":
		return converter.contentBlockStart(event)
	case "content_block_delta":
		return converter.contentBlockDelta(event)
	case "content_block_stop":
		return converter.contentBlockStop(event)
	case "message_delta":
		if event.Delta.StopReason != "" {
			converter.response.Status, converter.response.IncompleteDetails = statusFromStopReason(event.Delta.StopReason)
		}
		if event.Usage != nil {
			converter.response.Usage = normalizeUsage(*event.Usage)
		}
	case "message_stop":
		if converter.response.Status == "" || converter.response.Status == "in_progress" {
			converter.response.Status = "completed"
		}
		if converter.response.Status == "incomplete" {
			return []openai.StreamEvent{converter.lifecycle("response.incomplete")}
		}
		return []openai.StreamEvent{converter.lifecycle("response.completed")}
	case "error":
		converter.response.Status = "failed"
		if event.Error != nil {
			converter.response.Error = &openai.ErrorObject{Message: event.Error.Message, Type: "server_error", Code: event.Error.Type}
		}
		return []openai.StreamEvent{converter.lifecycle("response.failed")}
	}
	return nil
}

func (converter *streamConverter) contentBlockStart(event anthropic.StreamEvent) []openai.StreamEvent {
	index := event.Index
	block := event.ContentBlock
	if block == nil {
		return nil
	}
	switch block.Type {
	case "text":
		item := openai.OutputItem{
			Type:    "message",
			ID:      fmt.Sprintf("msg_item_%d", index),
			Status:  "in_progress",
			Role:    "assistant",
			Content: []openai.ContentPart{},
		}
		converter.itemIDs[index] = item.ID
		converter.response.Output = append(converter.response.Output, item)
		return []openai.StreamEvent{
			converter.outputItem("response.output_item.added", index, item),
			converter.contentPart("response.content_part.added", index, 0, openai.ContentPart{Type: "output_text"}),
		}
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
			converter.response.Output = append(converter.response.Output, item)
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
		converter.response.Output = append(converter.response.Output, item)
		return []openai.StreamEvent{converter.outputItem("response.output_item.added", index, item)}
	}
	return nil
}

func (converter *streamConverter) contentBlockDelta(event anthropic.StreamEvent) []openai.StreamEvent {
	index := event.Index
	switch event.Delta.Type {
	case "text_delta":
		converter.contentText[index] += event.Delta.Text
		return []openai.StreamEvent{{
			Event: "response.output_text.delta",
			Data: openai.OutputTextDeltaEvent{
				Type:           "response.output_text.delta",
				SequenceNumber: converter.next(),
				ItemID:         converter.itemIDs[index],
				OutputIndex:    index,
				ContentIndex:   0,
				Delta:          event.Delta.Text,
			},
		}}
	case "input_json_delta":
		converter.toolArguments[index] += event.Delta.PartialJSON
		return []openai.StreamEvent{{
			Event: "response.function_call_arguments.delta",
			Data: openai.FunctionCallArgumentsDeltaEvent{
				Type:           "response.function_call_arguments.delta",
				SequenceNumber: converter.next(),
				ItemID:         converter.itemIDs[index],
				OutputIndex:    index,
				Delta:          event.Delta.PartialJSON,
			},
		}}
	}
	return nil
}

func (converter *streamConverter) contentBlockStop(event anthropic.StreamEvent) []openai.StreamEvent {
	index := event.Index
	if text, ok := converter.contentText[index]; ok {
		converter.response.OutputText += text
		item := openai.OutputItem{
			Type:    "message",
			ID:      converter.itemIDs[index],
			Status:  "completed",
			Role:    "assistant",
			Content: []openai.ContentPart{{Type: "output_text", Text: text}},
		}
		converter.setOutput(index, item)
		return []openai.StreamEvent{
			{
				Event: "response.output_text.done",
				Data: openai.OutputTextDoneEvent{
					Type:           "response.output_text.done",
					SequenceNumber: converter.next(),
					ItemID:         converter.itemIDs[index],
					OutputIndex:    index,
					ContentIndex:   0,
					Text:           text,
				},
			},
			converter.contentPart("response.content_part.done", index, 0, openai.ContentPart{Type: "output_text", Text: text}),
			converter.outputItem("response.output_item.done", index, item),
		}
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
					OutputIndex:    index,
					Arguments:      compactJSON(arguments),
				},
			},
			converter.outputItem("response.output_item.done", index, item),
		}
	}
	return nil
}

func (converter *streamConverter) outputAt(index int) openai.OutputItem {
	if index < len(converter.response.Output) {
		return converter.response.Output[index]
	}
	return openai.OutputItem{}
}

func (converter *streamConverter) setOutput(index int, item openai.OutputItem) {
	for len(converter.response.Output) <= index {
		converter.response.Output = append(converter.response.Output, openai.OutputItem{})
	}
	converter.response.Output[index] = item
}

func (converter *streamConverter) lifecycle(event string) openai.StreamEvent {
	converter.response.Status = statusForLifecycle(event, converter.response.Status)
	return openai.StreamEvent{
		Event: event,
		Data: openai.ResponseLifecycleEvent{
			Type:           event,
			SequenceNumber: converter.next(),
			Response:       converter.response,
		},
	}
}

func (converter *streamConverter) outputItem(event string, index int, item openai.OutputItem) openai.StreamEvent {
	return openai.StreamEvent{
		Event: event,
		Data: openai.OutputItemEvent{
			Type:           event,
			SequenceNumber: converter.next(),
			OutputIndex:    index,
			Item:           item,
		},
	}
}

func (converter *streamConverter) contentPart(event string, outputIndex int, contentIndex int, part openai.ContentPart) openai.StreamEvent {
	return openai.StreamEvent{
		Event: event,
		Data: openai.ContentPartEvent{
			Type:           event,
			SequenceNumber: converter.next(),
			ItemID:         converter.itemIDs[outputIndex],
			OutputIndex:    outputIndex,
			ContentIndex:   contentIndex,
			Part:           part,
		},
	}
}

func (converter *streamConverter) next() int64 {
	converter.sequence++
	return converter.sequence
}

func statusForLifecycle(event string, current string) string {
	switch event {
	case "response.created", "response.in_progress":
		return "in_progress"
	case "response.completed":
		return "completed"
	case "response.incomplete":
		return "incomplete"
	case "response.failed":
		return "failed"
	default:
		return current
	}
}

func compactJSON(value string) string {
	var raw any
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return value
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return value
	}
	return string(data)
}
