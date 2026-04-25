package trace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const DefaultRoot = "trace"

type Config struct {
	Enabled   bool
	Root      string
	SessionID string
}

type Tracer struct {
	enabled   bool
	root      string
	sessionID string
	counter   atomic.Uint64
}

type Record struct {
	SessionID             string      `json:"session_id"`
	RequestNumber         uint64      `json:"request_number"`
	CapturedAt            string      `json:"captured_at"`
	HTTPRequest           HTTPRequest `json:"http_request"`
	ProxyRequest          any         `json:"proxy_request,omitempty"`
	UpstreamRequest       any         `json:"upstream_request,omitempty"`
	UpstreamResponse      any         `json:"upstream_response,omitempty"`
	OpenAIRequest         any         `json:"openai_request,omitempty"`
	AnthropicRequest      any         `json:"anthropic_request,omitempty"`
	AnthropicResponse     any         `json:"anthropic_response,omitempty"`
	AnthropicStreamEvents any         `json:"anthropic_stream_events,omitempty"`
	OpenAIResponse        any         `json:"openai_response,omitempty"`
	OpenAIStreamEvents    any         `json:"openai_stream_events,omitempty"`
	Error                 any         `json:"error,omitempty"`
}

type HTTPRequest struct {
	Method     string      `json:"method"`
	RequestURI string      `json:"request_uri"`
	Headers    http.Header `json:"headers,omitempty"`
	RemoteAddr string      `json:"remote_addr,omitempty"`
}

func New(cfg Config) *Tracer {
	root := cfg.Root
	if root == "" {
		root = DefaultRoot
	}
	sessionID := cfg.SessionID
	if sessionID == "" {
		sessionID = newSessionID()
	}
	return &Tracer{enabled: cfg.Enabled, root: root, sessionID: sessionID}
}

func (tracer *Tracer) Enabled() bool {
	return tracer != nil && tracer.enabled
}

func (tracer *Tracer) SessionID() string {
	if tracer == nil {
		return ""
	}
	return tracer.sessionID
}

func (tracer *Tracer) Directory() string {
	if tracer == nil {
		return ""
	}
	return filepath.Join(tracer.root, tracer.sessionID)
}

func (tracer *Tracer) Write(record Record) (string, error) {
	if !tracer.Enabled() {
		return "", nil
	}

	record.SessionID = tracer.sessionID
	record.RequestNumber = tracer.counter.Add(1)
	if record.CapturedAt == "" {
		record.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}

	redacted, err := redactForJSON(record)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(redacted, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')

	sessionDir := tracer.Directory()
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(sessionDir, fmt.Sprintf("%d.json", record.RequestNumber))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func NewHTTPRequest(request *http.Request) HTTPRequest {
	return HTTPRequest{
		Method:     request.Method,
		RequestURI: request.URL.RequestURI(),
		Headers:    request.Header.Clone(),
		RemoteAddr: request.RemoteAddr,
	}
}

func RawJSONOrString(data []byte) any {
	trimmed := strings.TrimSpace(string(data))
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage([]byte(trimmed))
	}
	return string(data)
}

func redactForJSON(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return redactValue(decoded), nil
}

func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, child := range typed {
			if isAPIKeyField(key) {
				redacted[key] = redactSensitiveValue(child)
				continue
			}
			redacted[key] = redactValue(child)
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for index, child := range typed {
			redacted[index] = redactValue(child)
		}
		return redacted
	default:
		return value
	}
}

func redactSensitiveValue(value any) any {
	switch typed := value.(type) {
	case []any:
		redacted := make([]any, len(typed))
		for index := range typed {
			redacted[index] = "[REDACTED]"
		}
		return redacted
	default:
		return "[REDACTED]"
	}
}

func isAPIKeyField(field string) bool {
	normalized := strings.ToLower(strings.TrimSpace(field))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "apikey", "anthropic-api-key", "openai-api-key":
		return true
	default:
		return false
	}
}

func newSessionID() string {
	randomBytes := make([]byte, 4)
	if _, err := rand.Read(randomBytes); err != nil {
		return time.Now().UTC().Format("20060102T150405Z")
	}
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(randomBytes)
}
