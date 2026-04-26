package bridge

import "moonbridge/internal/openai"

// PreambleResult holds the events emitted before upstream collection begins,
// plus the sequence counter so the real stream events can continue numbering.
type PreambleResult struct {
	Events       []openai.StreamEvent
	ResponseID   string
	NextSequence int64
	// PreambleOutputCount is the number of output items emitted in the preamble
	// (currently 1: the commentary message). The real stream converter should
	// offset its output indexes by this amount.
	PreambleOutputCount int
}

// BuildPreambleEvents generates SSE events that open the response lifecycle
// and emit a commentary message ("Collecting from upstream...") so the client
// sees activity immediately. The real upstream events should skip their own
// response.created / response.in_progress and continue the same response.
func (bridge *Bridge) BuildPreambleEvents(model string) PreambleResult {
	respID := "resp_preamble"
	itemID := "msg_preamble"
	text := "Collecting from upstream..."

	resp := openai.Response{
		ID:     respID,
		Object: "response",
		Status: "in_progress",
		Model:  model,
		Output: []openai.OutputItem{},
	}

	var seq int64
	next := func() int64 { seq++; return seq }

	item := openai.OutputItem{
		Type:   "message",
		ID:     itemID,
		Status: "in_progress",
		Role:   "assistant",
		Phase:  "commentary",
		Content: []openai.ContentPart{
			{Type: "output_text", Text: ""},
		},
	}

	resp.Output = append(resp.Output, item)

	events := []openai.StreamEvent{
		{Event: "response.created", Data: openai.ResponseLifecycleEvent{
			Type: "response.created", SequenceNumber: next(), Response: cloneResponse(resp),
		}},
		{Event: "response.in_progress", Data: openai.ResponseLifecycleEvent{
			Type: "response.in_progress", SequenceNumber: next(), Response: cloneResponse(resp),
		}},
		{Event: "response.output_item.added", Data: openai.OutputItemEvent{
			Type: "response.output_item.added", SequenceNumber: next(),
			OutputIndex: 0, Item: item,
		}},
		{Event: "response.content_part.added", Data: openai.ContentPartEvent{
			Type: "response.content_part.added", SequenceNumber: next(),
			ItemID: itemID, OutputIndex: 0, ContentIndex: 0,
			Part: openai.ContentPart{Type: "output_text", Text: ""},
		}},
		{Event: "response.output_text.delta", Data: openai.OutputTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: next(),
			ItemID: itemID, OutputIndex: 0, ContentIndex: 0, Delta: text,
		}},
		{Event: "response.output_text.done", Data: openai.OutputTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: next(),
			ItemID: itemID, OutputIndex: 0, ContentIndex: 0, Text: text,
		}},
		{Event: "response.content_part.done", Data: openai.ContentPartEvent{
			Type: "response.content_part.done", SequenceNumber: next(),
			ItemID: itemID, OutputIndex: 0, ContentIndex: 0,
			Part: openai.ContentPart{Type: "output_text", Text: text},
		}},
	}

	item.Status = "completed"
	item.Content = []openai.ContentPart{{Type: "output_text", Text: text}}
	events = append(events, openai.StreamEvent{
		Event: "response.output_item.done",
		Data: openai.OutputItemEvent{
			Type: "response.output_item.done", SequenceNumber: next(),
			OutputIndex: 0, Item: item,
		},
	})

	return PreambleResult{
		Events:              events,
		ResponseID:          respID,
		NextSequence:        seq,
		PreambleOutputCount: 1,
	}
}

// cloneResponse returns a shallow copy with a copied Output slice.
func cloneResponse(r openai.Response) openai.Response {
	out := r
	out.Output = make([]openai.OutputItem, len(r.Output))
	copy(out.Output, r.Output)
	return out
}
