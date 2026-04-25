package bridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/openai"
)

func (bridge *Bridge) ConvertStreamEvents(events []anthropic.StreamEvent, model string) []openai.StreamEvent {
	return bridge.ConvertStreamEventsWithContext(events, model, ConversionContext{})
}

func (bridge *Bridge) ConvertStreamEventsWithContext(events []anthropic.StreamEvent, model string, context ConversionContext) []openai.StreamEvent {
	converter := streamConverter{
		bridge:                  bridge,
		model:                   model,
		context:                 context,
		contentText:             map[int]string{},
		toolArguments:           map[int]string{},
		customToolInputs:        map[int]string{},
		customToolInitialInputs: map[int]string{},
		webSearchActions:        map[int]*openai.ToolAction{},
		webSearchInputs:         map[int]string{},
		itemIDs:                 map[int]string{},
		outputIndexes:           map[int]int{},
	}
	var converted []openai.StreamEvent
	for _, event := range events {
		converted = append(converted, converter.convert(event)...)
	}
	return converted
}

type streamConverter struct {
	bridge                  *Bridge
	model                   string
	context                 ConversionContext
	sequence                int64
	response                openai.Response
	contentText             map[int]string
	toolArguments           map[int]string
	customToolInputs        map[int]string
	customToolInitialInputs map[int]string
	webSearchActions        map[int]*openai.ToolAction
	webSearchInputs         map[int]string
	itemIDs                 map[int]string
	outputIndexes           map[int]int
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
				Name:   block.Name,
				Input:  "",
				Status: "in_progress",
			}
			converter.itemIDs[index] = item.ID
			converter.customToolInputs[index] = ""
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
		item.Type = "custom_tool_call"
		item.ID = converter.itemIDs[index]
		if item.CallID == "" {
			item.CallID = strings.TrimPrefix(converter.itemIDs[index], "ctc_")
		}
		input := converter.context.CustomToolInputFromRaw(item.Name, json.RawMessage(compactJSON(inputJSON)))
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

func (converter *streamConverter) outputAt(index int) openai.OutputItem {
	outputIndex := converter.outputIndex(index)
	if outputIndex < len(converter.response.Output) {
		return converter.response.Output[outputIndex]
	}
	return openai.OutputItem{}
}

func (converter *streamConverter) setOutput(index int, item openai.OutputItem) {
	converter.response.Output[converter.outputIndex(index)] = item
}

func (converter *streamConverter) addOutput(index int, item openai.OutputItem) {
	converter.outputIndexes[index] = len(converter.response.Output)
	converter.response.Output = append(converter.response.Output, item)
}

func (converter *streamConverter) hasOutput(index int) bool {
	_, ok := converter.outputIndexes[index]
	return ok
}

func (converter *streamConverter) removeOutput(index int) {
	outputIndex, ok := converter.outputIndexes[index]
	if !ok {
		return
	}
	if outputIndex == len(converter.response.Output)-1 {
		converter.response.Output = converter.response.Output[:outputIndex]
		delete(converter.outputIndexes, index)
		delete(converter.itemIDs, index)
	}
}

func (converter *streamConverter) outputIndex(index int) int {
	return converter.outputIndexes[index]
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
			OutputIndex:    converter.outputIndex(index),
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
			OutputIndex:    converter.outputIndex(outputIndex),
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
