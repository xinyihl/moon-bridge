package stats

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Usage represents token usage from an Anthropic response.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// SessionStats tracks cumulative token usage across a session.
type SessionStats struct {
	mu sync.RWMutex

	startTime time.Time

	// Cumulative counts
	totalRequests    int64
	totalInputTokens int64
	totalOutputTokens int64
	totalCacheCreation int64
	totalCacheRead   int64

	// Per-model breakdown (optional detailed tracking)
	byModel map[string]*ModelStats
}

// ModelStats tracks usage for a specific model.
type ModelStats struct {
	Requests      int64
	InputTokens   int64
	OutputTokens  int64
	CacheCreation int64
	CacheRead     int64
}

// NewSessionStats creates a new session stats tracker.
func NewSessionStats() *SessionStats {
	return &SessionStats{
		startTime: time.Now(),
		byModel:   make(map[string]*ModelStats),
	}
}

// Record adds a usage record to the session stats.
func (s *SessionStats) Record(model string, usage Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalRequests++
	s.totalInputTokens += int64(usage.InputTokens)
	s.totalOutputTokens += int64(usage.OutputTokens)
	s.totalCacheCreation += int64(usage.CacheCreationInputTokens)
	s.totalCacheRead += int64(usage.CacheReadInputTokens)

	if model != "" {
		if s.byModel[model] == nil {
			s.byModel[model] = &ModelStats{}
		}
		s.byModel[model].Requests++
		s.byModel[model].InputTokens += int64(usage.InputTokens)
		s.byModel[model].OutputTokens += int64(usage.OutputTokens)
		s.byModel[model].CacheCreation += int64(usage.CacheCreationInputTokens)
		s.byModel[model].CacheRead += int64(usage.CacheReadInputTokens)
	}
}

// CacheHitRate returns the cache hit rate as a percentage.
// Rate = cache_read_tokens / (input_tokens + cache_creation + cache_read)
// Returns 0 if the receiver is nil.
func (s *SessionStats) CacheHitRate() float64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	totalInput := s.totalInputTokens + s.totalCacheCreation + s.totalCacheRead
	if totalInput == 0 {
		return 0
	}
	return float64(s.totalCacheRead) / float64(totalInput) * 100
}

// Summary returns a summary of the session stats.
func (s *SessionStats) Summary() Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	totalInput := s.totalInputTokens + s.totalCacheCreation + s.totalCacheRead
	var cacheHitRate float64
	if totalInput > 0 {
		cacheHitRate = float64(s.totalCacheRead) / float64(totalInput) * 100
	}

	return Summary{
		Duration:            time.Since(s.startTime),
		Requests:            s.totalRequests,
		InputTokens:         s.totalInputTokens,
		OutputTokens:        s.totalOutputTokens,
		CacheCreation:       s.totalCacheCreation,
		CacheRead:           s.totalCacheRead,
		CacheHitRate:        cacheHitRate,
		EffectiveInputSaved: s.totalCacheRead,
	}
}

// Summary is a snapshot of session stats.
type Summary struct {
	Duration            time.Duration
	Requests            int64
	InputTokens         int64
	OutputTokens        int64
	CacheCreation       int64
	CacheRead           int64
	CacheHitRate        float64
	EffectiveInputSaved int64
}

// LogValue implements slog.LogValuer for structured logging.
func (s Summary) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("requests", s.Requests),
		slog.Int64("input_tokens", s.InputTokens),
		slog.Int64("output_tokens", s.OutputTokens),
		slog.Int64("cache_creation", s.CacheCreation),
		slog.Int64("cache_read", s.CacheRead),
		slog.Float64("cache_hit_rate", s.CacheHitRate),
		slog.Int64("cache_saved_tokens", s.EffectiveInputSaved),
		slog.Duration("duration", s.Duration),
	)
}

// WriteSummary writes a human-readable summary to the writer.
func WriteSummary(w io.Writer, s Summary) {
	fmt.Fprintf(w, "Session Stats: %d requests, %s duration\n", s.Requests, s.Duration.Round(time.Second))
	fmt.Fprintf(w, "  Input:  %d tokens (%d fresh, %d cache creation, %d cache read)\n",
		s.InputTokens+s.CacheCreation+s.CacheRead,
		s.InputTokens,
		s.CacheCreation,
		s.CacheRead)
	fmt.Fprintf(w, "  Output: %d tokens\n", s.OutputTokens)
	if s.CacheHitRate > 0 {
		fmt.Fprintf(w, "  Cache Hit Rate: %.1f%% (saved %d tokens)\n", s.CacheHitRate, s.EffectiveInputSaved)
	}
}
