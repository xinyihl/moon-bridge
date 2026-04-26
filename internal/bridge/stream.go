package bridge

import (
	"encoding/json"

	"moonbridge/internal/anthropic"
	deepseekv4 "moonbridge/internal/extensions/deepseek_v4"
	"moonbridge/internal/openai"
	"moonbridge/internal/session"
)

// StreamOptions controls how the stream converter behaves when a preamble
// has already been emitted to the client.
type StreamOptions struct {
	// SequenceOffset is added to every sequence number so numbering
	// continues from the preamble.
	SequenceOffset int64
	// SkipInitialLifecycle causes the converter to drop the response.created
	// and response.in_progress events (already sent in the preamble).
	SkipInitialLifecycle bool
	// OutputIndexOffset shifts all output_index values so real content
	// items don't collide with preamble items.
	OutputIndexOffset int
	// ResponseID overrides the response ID to match the preamble.
	ResponseID string
}

func (bridge *Bridge) ConvertStreamEvents(events []anthropic.StreamEvent, model string) []openai.StreamEvent {
	return bridge.ConvertStreamEventsWithContext(events, model, ConversionContext{}, nil, StreamOptions{})
}

func (bridge *Bridge) ConvertStreamEventsWithContext(events []anthropic.StreamEvent, model string, context ConversionContext, sess *session.Session, opts StreamOptions) []openai.StreamEvent {
	converter := streamConverter{
		bridge:                  bridge,
		model:                   model,
		context:                 context,
		contentText:             map[int]string{},
		toolArguments:           map[int]string{},
		customToolInputs:        map[int]string{},
		customToolInitialInputs: map[int]string{},
		customToolNames:         map[int]string{},
		webSearchActions:        map[int]*openai.ToolAction{},
		webSearchInputs:         map[int]string{},
		itemIDs:                 map[int]string{},
		outputIndexes:           map[int]int{},
		opts:                    opts,
	}
	if bridge.cfg.DeepSeekV4Enabled() && sess != nil && sess.DeepSeek != nil {
		converter.deepseek = deepseekv4.NewStreamState()
	}
	var converted []openai.StreamEvent
	for _, event := range events {
		converted = append(converted, converter.convert(event)...)
	}
	
	// After stream completes, feed the result back to the session's DeepSeek state.
	if converter.deepseek != nil && sess != nil && sess.DeepSeek != nil && bridge.cfg.DeepSeekV4Enabled() {
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
	itemIDs                 map[int]string
	outputIndexes           map[int]int
	opts                    StreamOptions
}

func (converter *streamConverter) convert(event anthropic.StreamEvent) []openai.StreamEvent {
	switch event.Type {
	case "message_start":
		rid := responseID(event.Message.ID)
		if converter.opts.ResponseID != "" {
			rid = converter.opts.ResponseID
		}
		converter.response = openai.Response{
			ID:     rid,
			Object: "response",
			Status: "in_progress",
			Model:  converter.model,
			Output: []openai.OutputItem{},
		}
		if converter.opts.SkipInitialLifecycle {
			return nil
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

// outputIndex returns the wire-level output index for SSE events,
// accounting for any preamble items.
func (converter *streamConverter) outputIndex(index int) int {
	return converter.outputIndexes[index] + converter.opts.OutputIndexOffset
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
	return converter.sequence + converter.opts.SequenceOffset
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
