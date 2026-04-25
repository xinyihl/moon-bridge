package deepseekv4

import (
	"encoding/json"
	"strings"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/openai"
)

// IsEnabled returns whether the DeepSeek V4 extension should be active.
func IsEnabled(cfg interface{ DeepSeekV4Enabled() bool }) bool {
	return cfg.DeepSeekV4Enabled()
}

// StripReasoningContent removes the reasoning_content field from message
// content before sending it back as input in multi-round conversations.
// DeepSeek returns 400 if reasoning_content appears in the input messages.
func StripReasoningContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return raw
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "\"") {
		return raw
	}
	if !strings.HasPrefix(trimmed, "[") {
		return raw
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw
	}
	changed := false
	for i, item := range items {
		if _, ok := item["reasoning_content"]; ok {
			delete(items[i], "reasoning_content")
			changed = true
		}
		// Also strip nested content parts if they carry reasoning_content.
		if content, ok := item["content"].([]any); ok {
			for j, part := range content {
				if m, ok := part.(map[string]any); ok {
					if _, ok := m["reasoning_content"]; ok {
						delete(m, "reasoning_content")
						content[j] = m
						changed = true
					}
				}
			}
			items[i]["content"] = content
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(items)
	if err != nil {
		return raw
	}
	return out
}

// ExtractReasoningContent pulls reasoning_content out of Anthropic-style
// response content blocks.  DeepSeek returns reasoning_content as a sibling
// field inside the message (not inside choices[0].delta like OpenAI).
// When bridged through Anthropic-compatible endpoints it usually shows up as
// an extra text block or as a field on the message object.
func ExtractReasoningContent(blocks []anthropic.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "reasoning_content" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// InjectReasoningIntoOutput turns DeepSeek reasoning_content into an
// OpenAI-style output item so that Codex clients can display it.
func InjectReasoningIntoOutput(output []openai.OutputItem, reasoning string) []openai.OutputItem {
	if reasoning == "" {
		return output
	}
	// Place a reasoning message before the first assistant message.
	reasoningItem := openai.OutputItem{
		Type:   "message",
		ID:     "msg_reasoning_0",
		Status: "completed",
		Role:   "assistant",
		Content: []openai.ContentPart{
			{Type: "output_text", Text: reasoning},
		},
	}
	// If the first item is already an assistant message, prepend reasoning.
	if len(output) > 0 && output[0].Type == "message" && output[0].Role == "assistant" {
		return append([]openai.OutputItem{reasoningItem}, output...)
	}
	return append([]openai.OutputItem{reasoningItem}, output...)
}

// StreamDeltaForReasoning returns the delta text for a reasoning_content
// block in a streaming response.  DeepSeek streams reasoning_content via
// delta.reasoning_content before the normal content.
func StreamDeltaForReasoning(delta anthropic.StreamDelta) string {
	// DeepSeek-compatible providers may use a custom delta type.
	if delta.Type == "reasoning_content_delta" {
		return delta.Text
	}
	return ""
}

// IsReasoningContentBlock reports whether a content block start event
// represents reasoning content rather than normal text.
func IsReasoningContentBlock(block *anthropic.ContentBlock) bool {
	if block == nil {
		return false
	}
	return block.Type == "reasoning_content"
}

// ToAnthropicRequest mutates an Anthropic request for DeepSeek V4 quirks:
//   - Drop unsupported parameters (temperature, top_p) because DeepSeek
//     ignores them and they can confuse some proxies.
//   - Map reasoning_effort to DeepSeek thinking level: below "high" => "high",
//     "high" and above => "max".
func ToAnthropicRequest(req *anthropic.MessageRequest, reasoning map[string]any) {
	req.Temperature = nil
	req.TopP = nil

	level := extractReasoningEffort(reasoning)
	if level == "" {
		return
	}
	var thinkingLevel string
	switch level {
	case "low", "medium":
		thinkingLevel = "high"
	case "high", "maximum", "max":
		thinkingLevel = "max"
	default:
		// Unknown values: treat as high if lexicographically less than "high",
		// otherwise max.
		if level < "high" {
			thinkingLevel = "high"
		} else {
			thinkingLevel = "max"
		}
	}
	req.Thinking = &anthropic.ThinkingConfig{
		Type:         "enabled",
		BudgetTokens: budgetForLevel(thinkingLevel, req.MaxTokens),
	}
}
func extractReasoningEffort(reasoning map[string]any) string {
	if reasoning == nil {
		return ""
	}
	v, ok := reasoning["effort"].(string)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(v))
}

func budgetForLevel(level string, maxTokens int) int {
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	switch level {
	case "high":
		return maxTokens / 2
	case "max":
		return maxTokens * 3 / 4
	default:
		return maxTokens / 2
	}
}
