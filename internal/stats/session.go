package stats

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// ModelPricing holds per-model pricing in RMB per M tokens.
type ModelPricing struct {
	InputPrice      float64
	OutputPrice     float64
	CacheWritePrice float64
	CacheReadPrice  float64
}

// Usage represents token usage from an Anthropic response.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// SessionStats tracks cumulative token usage and cost across a session.
type SessionStats struct {
	mu sync.RWMutex

	startTime time.Time

	// Cumulative counts
	totalRequests      int64
	totalInputTokens   int64
	totalOutputTokens  int64
	totalCacheCreation int64
	totalCacheRead     int64

	// Cumulative cost (RMB)
	totalCost float64

	// Per-model breakdown
	byModel map[string]*ModelStats
	pricing map[string]ModelPricing
}

// ModelStats tracks usage and cost for a specific model.
type ModelStats struct {
	Requests      int64
	InputTokens   int64
	OutputTokens  int64
	CacheCreation int64
	CacheRead     int64
	Cost          float64
}

// NewSessionStats creates a new session stats tracker.
func NewSessionStats() *SessionStats {
	return &SessionStats{
		startTime: time.Now(),
		byModel:   make(map[string]*ModelStats),
		pricing:   make(map[string]ModelPricing),
	}
}

// SetPricing configures per-model pricing for cost calculation.
func (s *SessionStats) SetPricing(pricing map[string]ModelPricing) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pricing = pricing
}

// Record adds a usage record to the session stats.
// If pricing is configured for the model, cost is computed automatically.
func (s *SessionStats) Record(model string, usage Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalRequests++
	s.totalInputTokens += int64(usage.InputTokens)
	s.totalOutputTokens += int64(usage.OutputTokens)
	s.totalCacheCreation += int64(usage.CacheCreationInputTokens)
	s.totalCacheRead += int64(usage.CacheReadInputTokens)

	// Compute cost if pricing is available for this model
	var cost float64
	if p, ok := s.pricing[model]; ok {
		cost = computeCost(usage, p)
		s.totalCost += cost
	}

	if model != "" {
		if s.byModel[model] == nil {
			s.byModel[model] = &ModelStats{}
		}
		s.byModel[model].Requests++
		s.byModel[model].InputTokens += int64(usage.InputTokens)
		s.byModel[model].OutputTokens += int64(usage.OutputTokens)
		s.byModel[model].CacheCreation += int64(usage.CacheCreationInputTokens)
		s.byModel[model].CacheRead += int64(usage.CacheReadInputTokens)
		s.byModel[model].Cost += cost
	}
}

// computeCost calculates the cost in RMB for a single usage record.
// All prices are per M tokens.
func computeCost(usage Usage, p ModelPricing) float64 {
	freshInput := float64(usage.InputTokens)
	cacheWrite := float64(usage.CacheCreationInputTokens)
	cacheRead := float64(usage.CacheReadInputTokens)
	output := float64(usage.OutputTokens)

	return freshInput*p.InputPrice/1000000 +
		cacheWrite*p.CacheWritePrice/1000000 +
		cacheRead*p.CacheReadPrice/1000000 +
		output*p.OutputPrice/1000000
}

// CacheHitRate returns the cache hit rate as a percentage.
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
		TotalCost:           s.totalCost,
		ByModel:             copyByModel(s.byModel),
	}
}

func copyByModel(src map[string]*ModelStats) map[string]*ModelStats {
	if src == nil {
		return nil
	}
	dst := make(map[string]*ModelStats, len(src))
	for k, v := range src {
		cp := *v
		dst[k] = &cp
	}
	return dst
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
	TotalCost           float64
	ByModel             map[string]*ModelStats
}

// LogValue implements slog.LogValuer for structured logging.
func (s Summary) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.Int64("requests", s.Requests),
		slog.Int64("input_tokens", s.InputTokens),
		slog.Int64("output_tokens", s.OutputTokens),
		slog.Int64("cache_creation", s.CacheCreation),
		slog.Int64("cache_read", s.CacheRead),
		slog.Float64("cache_hit_rate", s.CacheHitRate),
		slog.Int64("cache_saved_tokens", s.EffectiveInputSaved),
		slog.Duration("duration", s.Duration),
	}
	attrs = append(attrs, slog.Float64("cost_cny", s.TotalCost))
	return slog.GroupValue(attrs...)
}

func FormatUsageLine(model string, usage Usage, cacheHitRate float64, billing float64) string {
	inputTokens := usage.InputTokens + usage.CacheReadInputTokens
	return fmt.Sprintf("%s Usage: %.6f M Input, %.6f M Output, Session Cache Hit Rate: %.2f%%, Billing: %.2f CNY",
		model,
		float64(inputTokens)/1_000_000,
		float64(usage.OutputTokens)/1_000_000,
		cacheHitRate,
		billing,
	)
}

func FormatSummaryLine(s Summary) string {
	return fmt.Sprintf("Summary：Session Cache Hit Rate(AVG): %.1f%%, Billing: %.2f CNY", s.CacheHitRate, s.TotalCost)
}

// WriteSummary writes a human-readable summary to the writer.
func WriteSummary(w io.Writer, s Summary) {
	fmt.Fprintln(w, FormatSummaryLine(s))
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
	fmt.Fprintf(w, "  Total Cost: ¥%.6f\n", s.TotalCost)
	for model, ms := range s.ByModel {
		if ms.Cost > 0 {
			fmt.Fprintf(w, "    %s: ¥%.6f (%d req, %d in, %d out)\n",
				model, ms.Cost, ms.Requests, ms.InputTokens+ms.CacheCreation+ms.CacheRead, ms.OutputTokens)
		}
	}
}

// ComputeCost returns the cost in RMB for a given model and usage without
// recording it in the session stats. Returns 0 if no pricing is configured
// for the model.
func (s *SessionStats) ComputeCost(model string, usage Usage) float64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pricing[model]
	if !ok {
		return 0
	}
	return computeCost(usage, p)
}
