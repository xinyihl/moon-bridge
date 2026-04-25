package app

import (
	"bytes"
	"path/filepath"
	"testing"

	mbtrace "moonbridge/internal/trace"
)

func TestWelcomeMessage(t *testing.T) {
	want := "Welcome to Moon Bridge!"

	if got := WelcomeMessage(); got != want {
		t.Fatalf("WelcomeMessage() = %q, want %q", got, want)
	}
}

func TestRunWritesWelcomeMessage(t *testing.T) {
	var output bytes.Buffer

	Run(&output)

	want := "Welcome to Moon Bridge!\n"
	if got := output.String(); got != want {
		t.Fatalf("Run() wrote %q, want %q", got, want)
	}
}

func TestCaptureTraceDirectoriesUseSession(t *testing.T) {
	responseTracer := mbtrace.New(captureResponseTraceConfig(true))
	if got, want := responseTracer.Directory(), filepath.Join("trace", "Capture", "Response", responseTracer.SessionID()); got != want {
		t.Fatalf("response trace directory = %q, want %q", got, want)
	}

	anthropicTracer := mbtrace.New(captureAnthropicTraceConfig(true))
	if got, want := anthropicTracer.Directory(), filepath.Join("trace", "Capture", "Anthropic", anthropicTracer.SessionID()); got != want {
		t.Fatalf("anthropic trace directory = %q, want %q", got, want)
	}
}
