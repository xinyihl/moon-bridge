package bridge

import (
	"encoding/json"
	"fmt"

	"moonbridge/internal/anthropic"
	deepseekv4 "moonbridge/internal/extensions/deepseek_v4"
	"moonbridge/internal/openai"
	"moonbridge/internal/session"
)

func (bridge *Bridge) ConvertStreamEvents(events []anthropic.StreamEvent, model string) []openai.StreamEvent {
	return bridge.ConvertStreamEventsWithContext(events, model, ConversionContext{}, nil)
}

type StreamOptions struct {
	PersistFinalTextReasoning bool
}

func (bridge *Bridge) ConvertStreamEventsWithContext(events []anthropic.StreamEvent, model string, context ConversionContext, sess *session.Session, opts ...StreamOptions) []openai.StreamEvent {
	deepseekV4Enabled := bridge.cfg.DeepSeekV4ForModel(model)
	var opt StreamOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	converter := streamConverter{
		bridge:                  bridge,
		model:                   model,
		context:                 context,
		persistTextReasoning:    opt.PersistFinalTextReasoning,
		contentText:             map[int]string{},
		toolArguments:           map[int]string{},
		customToolInputs:        map[int]string{},
		customToolInitialInputs: map[int]string{},
		customToolNames:         map[int]string{},
		webSearchActions:        map[int]*openai.ToolAction{},
		webSearchInputs:         map[int]string{},
		itemIDs:                 map[int]string{},
		outputIndexes:           map[int]int{},
	}
	if deepseekV4Enabled && sess != nil && sess.DeepSeek != nil {
		converter.deepseek = deepseekv4.NewStreamState()
	}
	var converted []openai.StreamEvent
	for _, event := range events {
		converted = append(converted, converter.convert(event)...)
	}

	// After stream completes, feed the result back to the session's DeepSeek state.
	if converter.deepseek != nil && sess != nil && sess.DeepSeek != nil && deepseekV4Enabled {
		sess.DeepSeek.RememberStreamResult(converter.deepseek, converter.response.OutputText)
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
	customToolNames         map[int]string
	webSearchActions        map[int]*openai.ToolAction
	webSearchInputs         map[int]string
	deepseek                *deepseekv4.StreamState
	pendingReasoningText    string
	reasoningEmitted        bool
	persistTextReasoning    bool
	hasToolCalls            bool
	itemIDs                 map[int]string
	outputIndexes           map[int]int
}

func (converter *streamConverter) convert(event anthropic.StreamEvent) []openai.StreamEvent {
	switch event.Type {
	case "message_start":
		rid := responseID(event.Message.ID)
		converter.response = openai.Response{
			ID:     rid,
			Object: "response",
			Status: "in_progress",
			Model:  converter.model,
			Output: []openai.OutputItem{},
			Usage:  normalizeUsage(event.Message.Usage),
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
			updated := normalizeUsage(*event.Usage)
			if converter.response.Usage.InputTokens > 0 {
				updated.InputTokens = converter.response.Usage.InputTokens
				updated.InputTokensDetails = converter.response.Usage.InputTokensDetails
			}
			updated.TotalTokens = updated.InputTokens + updated.OutputTokens
			converter.response.Usage = updated
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

func (converter *streamConverter) outputAt(index int) openai.OutputItem {
	outputIndex := converter.sliceIndex(index)
	if outputIndex < len(converter.response.Output) {
		return converter.response.Output[outputIndex]
	}
	return openai.OutputItem{}
}

func (converter *streamConverter) setOutput(index int, item openai.OutputItem) {
	converter.response.Output[converter.sliceIndex(index)] = item
}

func (converter *streamConverter) addOutput(index int, item openai.OutputItem) {
	converter.outputIndexes[index] = len(converter.response.Output)
	converter.response.Output = append(converter.response.Output, item)
}

// resetBlockState starts a fresh logical content block for providers that
// reuse Anthropic stream indexes while preserving already-emitted outputs.
func (converter *streamConverter) resetBlockState(index int) {
	delete(converter.contentText, index)
	delete(converter.toolArguments, index)
	delete(converter.customToolInputs, index)
	delete(converter.customToolInitialInputs, index)
	delete(converter.customToolNames, index)
	delete(converter.webSearchActions, index)
	delete(converter.webSearchInputs, index)
	delete(converter.itemIDs, index)
	delete(converter.outputIndexes, index)
	if converter.deepseek != nil {
		converter.deepseek.Reset(index)
	}
}

func (converter *streamConverter) hasOutput(index int) bool {
	_, ok := converter.outputIndexes[index]
	return ok
}

func (converter *streamConverter) removeOutput(index int) {
	sliceIdx, ok := converter.outputIndexes[index]
	if !ok {
		return
	}
	if sliceIdx == len(converter.response.Output)-1 {
		converter.response.Output = converter.response.Output[:sliceIdx]
		delete(converter.outputIndexes, index)
		delete(converter.itemIDs, index)
	}
}

// sliceIndex returns the position in the internal response.Output slice.
func (converter *streamConverter) sliceIndex(index int) int {
	return converter.outputIndexes[index]
}

// outputIndex returns the wire-level output index for SSE events.
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

func (converter *streamConverter) outputItemAt(event string, outputIndex int, item openai.OutputItem) openai.StreamEvent {
	return openai.StreamEvent{
		Event: event,
		Data: openai.OutputItemEvent{
			Type:           event,
			SequenceNumber: converter.next(),
			OutputIndex:    outputIndex,
			Item:           item,
		},
	}
}

func (converter *streamConverter) emitPendingReasoningItem() []openai.StreamEvent {
	if converter.deepseek == nil || converter.reasoningEmitted || converter.pendingReasoningText == "" {
		return nil
	}
	outputIndex := len(converter.response.Output)
	item := openai.OutputItem{
		Type: "reasoning",
		ID:   fmt.Sprintf("rsn_%d", outputIndex),
		Summary: []openai.ReasoningItemSummary{{
			Type: "summary_text",
			Text: converter.pendingReasoningText,
		}},
	}
	converter.reasoningEmitted = true
	converter.response.Output = append(converter.response.Output, item)

	return []openai.StreamEvent{
		converter.outputItemAt("response.output_item.added", outputIndex, item),
		converter.outputItemAt("response.output_item.done", outputIndex, item),
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
