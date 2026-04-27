package deepseekv4

import (
	"encoding/json"
	"strings"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/openai"
	"moonbridge/internal/plugin"
)

const PluginName = "deepseek_v4"

// EnabledFunc determines if the plugin is active for a model.
type EnabledFunc func(modelAlias string) bool

// DSPlugin implements the new plugin.Plugin interface plus relevant capabilities.
type DSPlugin struct {
	plugin.BasePlugin
	isEnabled EnabledFunc
}

// NewPlugin creates a DeepSeek V4 plugin.
func NewPlugin(isEnabled EnabledFunc) *DSPlugin {
	return &DSPlugin{isEnabled: isEnabled}
}

func (p *DSPlugin) Name() string                    { return PluginName }
func (p *DSPlugin) EnabledForModel(model string) bool { return p.isEnabled(model) }

// --- InputPreprocessor ---

func (p *DSPlugin) PreprocessInput(_ *plugin.RequestContext, raw json.RawMessage) json.RawMessage {
	return StripReasoningContent(raw)
}

// --- RequestMutator ---

func (p *DSPlugin) MutateRequest(ctx *plugin.RequestContext, req *anthropic.MessageRequest) {
	ToAnthropicRequest(req, ctx.Reasoning)
}

// --- MessageRewriter ---

func (p *DSPlugin) RewriteMessages(ctx *plugin.RequestContext, messages []anthropic.Message) []anthropic.Message {
	return messages
}

// --- ThinkingPrepender ---

func (p *DSPlugin) PrependThinkingForToolUse(messages []anthropic.Message, toolCallID string, sessionState any) []anthropic.Message {
	state, _ := sessionState.(*State)
	if state == nil {
		return messages
	}
	state.PrependCachedForToolUse(&messages, toolCallID)
	return messages
}

func (p *DSPlugin) PrependThinkingForAssistant(blocks []anthropic.ContentBlock, sessionState any) []anthropic.ContentBlock {
	state, _ := sessionState.(*State)
	if state == nil {
		return blocks
	}
	return state.PrependCachedForAssistantText(blocks)
}

// --- ContentFilter ---

func (p *DSPlugin) FilterContent(_ *plugin.RequestContext, block anthropic.ContentBlock) (skip bool, extra []openai.OutputItem) {
	switch block.Type {
	case "thinking", "reasoning_content":
		text := ExtractReasoningContent([]anthropic.ContentBlock{block})
		if text != "" {
			extra = append(extra, openai.OutputItem{
				Type: "reasoning",
				Summary: []openai.ReasoningItemSummary{{
					Type: "summary_text",
					Text: text,
				}},
			})
		}
		return true, extra
	case "text":
		if IsReasoningContentBlock(&block) {
			return true, nil
		}
	}
	return false, nil
}

// --- ContentRememberer ---

func (p *DSPlugin) RememberContent(ctx *plugin.RequestContext, content []anthropic.ContentBlock) {
	state, _ := ctx.SessionState(PluginName).(*State)
	if state == nil {
		return
	}
	state.RememberFromContent(content)
}

// --- StreamInterceptor ---

func (p *DSPlugin) NewStreamState() any {
	return NewStreamState()
}

func (p *DSPlugin) OnStreamEvent(ctx *plugin.StreamContext, event plugin.StreamEvent) (consumed bool, emit []openai.StreamEvent) {
	ss, _ := ctx.StreamState.(*StreamState)
	if ss == nil {
		return false, nil
	}

	switch event.Type {
	case "block_start":
		if ss.Start(event.Index, event.Block) {
			return true, nil
		}
	case "block_delta":
		if ss.Delta(event.Index, event.Delta) {
			return true, nil
		}
	case "block_stop":
		if ss.Stop(event.Index) {
			// Thinking block completed; the bridge will handle emitting
			// the reasoning item from CompletedThinkingText().
			return true, nil
		}
	}
	return false, nil
}

func (p *DSPlugin) OnStreamComplete(ctx *plugin.StreamContext, outputText string) {
	ss, _ := ctx.StreamState.(*StreamState)
	state, _ := ctx.SessionState(PluginName).(*State)
	if ss == nil || state == nil {
		return
	}
	state.RememberStreamResult(ss, outputText)
}

// --- ErrorTransformer ---

func (p *DSPlugin) TransformError(_ *plugin.RequestContext, msg string) string {
	if strings.Contains(msg, "content[].thinking") && strings.Contains(msg, "thinking mode") {
		return "Missing required thinking blocks - ensure reasoning items are preserved in conversation history for tool-call turns."
	}
	return msg
}

// --- SessionStateProvider ---

func (p *DSPlugin) NewSessionState() any {
	return NewState()
}

// Compile-time interface checks.
var (
	_ plugin.Plugin              = (*DSPlugin)(nil)
	_ plugin.InputPreprocessor   = (*DSPlugin)(nil)
	_ plugin.RequestMutator      = (*DSPlugin)(nil)
	_ plugin.MessageRewriter     = (*DSPlugin)(nil)
	_ plugin.ContentFilter       = (*DSPlugin)(nil)
	_ plugin.ContentRememberer   = (*DSPlugin)(nil)
	_ plugin.StreamInterceptor   = (*DSPlugin)(nil)
	_ plugin.ErrorTransformer    = (*DSPlugin)(nil)
	_ plugin.SessionStateProvider = (*DSPlugin)(nil)
	_ plugin.ThinkingPrepender    = (*DSPlugin)(nil)
)
