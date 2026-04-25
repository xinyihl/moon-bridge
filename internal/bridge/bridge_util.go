package bridge

import (
	"encoding/json"
	"net/http"
	"strings"

	"moonbridge/internal/anthropic"
	"moonbridge/internal/openai"
)

func statusFromStopReason(stopReason string) (string, *openai.IncompleteDetails) {
	switch stopReason {
	case "max_tokens":
		return "incomplete", &openai.IncompleteDetails{Reason: "max_output_tokens"}
	case "model_context_window":
		return "incomplete", &openai.IncompleteDetails{Reason: "max_input_tokens"}
	case "pause_turn":
		return "incomplete", &openai.IncompleteDetails{Reason: "provider_pause"}
	default:
		return "completed", nil
	}
}

func normalizeUsage(usage anthropic.Usage) openai.Usage {
	inputTokens := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	outputTokens := usage.OutputTokens
	return openai.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		InputTokensDetails: openai.InputTokensDetails{
			CachedTokens: usage.CacheReadInputTokens,
		},
	}
}

func estimateTokens(request anthropic.MessageRequest) int {
	data, _ := json.Marshal(request)
	if len(data) == 0 {
		return 0
	}
	return len(data)/4 + 1
}

func responseID(providerID string) string {
	if providerID == "" {
		return "resp_generated"
	}
	if strings.HasPrefix(providerID, "resp_") {
		return providerID
	}
	return "resp_" + providerID
}

func invalidRequest(message, param, code string) error {
	return &RequestError{Status: http.StatusBadRequest, Message: message, Param: param, Code: code}
}

func localShellSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			"working_directory": map[string]any{"type": "string"},
			"timeout_ms":        map[string]any{"type": "integer"},
			"env": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
			},
		},
		"required": []string{"command"},
	}
}

func localShellActionFromRaw(raw json.RawMessage) *openai.ToolAction {
	var input struct {
		Command          []string          `json:"command"`
		WorkingDirectory string            `json:"working_directory"`
		TimeoutMS        int               `json:"timeout_ms"`
		Env              map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return &openai.ToolAction{Type: "exec"}
	}
	return &openai.ToolAction{
		Type:             "exec",
		Command:          input.Command,
		WorkingDirectory: input.WorkingDirectory,
		TimeoutMS:        input.TimeoutMS,
		Env:              input.Env,
	}
}

func localShellInputFromAction(action *openai.ToolAction) json.RawMessage {
	if action == nil {
		return json.RawMessage(`{"command":[]}`)
	}
	data, err := json.Marshal(map[string]any{
		"command":           action.Command,
		"working_directory": action.WorkingDirectory,
		"timeout_ms":        action.TimeoutMS,
		"env":               action.Env,
	})
	if err != nil {
		return json.RawMessage(`{"command":[]}`)
	}
	return data
}

func webSearchItemID(providerID string) string {
	if providerID == "" {
		return "ws_generated"
	}
	if strings.HasPrefix(providerID, "ws_") {
		return providerID
	}
	return "ws_" + providerID
}

func webSearchActionFromRaw(raw json.RawMessage) *openai.ToolAction {
	action := &openai.ToolAction{Type: "search"}
	if len(raw) == 0 || string(raw) == "null" {
		return action
	}
	var input struct {
		Type    string   `json:"type"`
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
		URL     string   `json:"url"`
		Pattern string   `json:"pattern"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return action
	}
	if input.Type != "" {
		action.Type = input.Type
	}
	action.Query = input.Query
	action.Queries = input.Queries
	action.URL = input.URL
	action.Pattern = input.Pattern
	if action.Type == "" {
		action.Type = "search"
	}
	return action
}

func hasWebSearchActionDetails(action *openai.ToolAction) bool {
	if action == nil {
		return false
	}
	return action.Query != "" || len(action.Queries) > 0 || action.URL != "" || action.Pattern != ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
