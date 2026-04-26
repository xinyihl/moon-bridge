package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"moonbridge/internal/logger"
	"strings"
	"sync"
	"time"
)

const (
	StateWarming      = "warming"
	StateWarm         = "warm"
	StateExpired      = "expired"
	StateNotCacheable = "not_cacheable"
	StateMissed       = "missed"
	StateFailed       = "failed"
)

type PlannerConfig struct {
	Mode                     string
	TTL                      string
	PromptCaching            bool
	AutomaticPromptCache     bool
	ExplicitCacheBreakpoints bool
	MaxBreakpoints           int
	MinCacheTokens           int
	ExpectedReuse            int
	MinimumValueScore        int
}

type PlanInput struct {
	ProviderID         string
	UpstreamWorkspace  string
	UpstreamAPIKeyID   string
	Model              string
	PromptCacheKey     string
	ToolsHash          string
	SystemHash         string
	MessagePrefixHash  string
	MessageBreakpoints []MessageBreakpointCandidate
	ToolCount          int
	SystemBlockCount   int
	MessageCount       int
	EstimatedTokens    int
}

type MessageBreakpointCandidate struct {
	MessageIndex int
	ContentIndex int
	BlockPath    string
	Hash         string
	Role         string
}

type CacheCreationPlan struct {
	Mode        string
	TTL         string
	LocalKey    string
	Breakpoints []CacheBreakpoint
	WarmPolicy  string
	Reason      string
}

type CacheBreakpoint struct {
	Scope        string
	BlockPath    string
	TTL          string
	Hash         string
	ScopeIndex   int
	ContentIndex int
}

type UsageSignals struct {
	InputTokens              int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

type RegistryEntry struct {
	State                    string
	LocalKey                 string
	CreatedAt                time.Time
	ExpiresAt                time.Time
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	MissCount                int
}

type MemoryRegistry struct {
	mu      sync.Mutex
	entries map[string]RegistryEntry
}

type Planner struct {
	cfg      PlannerConfig
	registry *MemoryRegistry
}

func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{entries: map[string]RegistryEntry{}}
}

func (registry *MemoryRegistry) Get(key string) (RegistryEntry, bool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry, ok := registry.entries[key]
	return entry, ok
}

func (registry *MemoryRegistry) Set(entry RegistryEntry) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.entries[entry.LocalKey] = entry
}

func (registry *MemoryRegistry) UpdateFromUsage(key string, usage UsageSignals, inputTokens int) {
	registry.mu.Lock()
	defer registry.mu.Unlock()

	entry := registry.entries[key]
	entry.LocalKey = key
	now := time.Now()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}

	switch {
	case usage.CacheCreationInputTokens > 0:
		entry.State = StateWarm
		entry.CacheCreationInputTokens = usage.CacheCreationInputTokens
		entry.ExpiresAt = now.Add(5 * time.Minute)
	case usage.CacheReadInputTokens > 0:
		entry.State = StateWarm
		entry.CacheReadInputTokens = usage.CacheReadInputTokens
	case inputTokens <= 16:
		entry.State = StateNotCacheable
	default:
		entry.State = StateMissed
		entry.MissCount++
	}

	registry.entries[key] = entry
}

func NewPlanner(cfg PlannerConfig) *Planner {
	return NewPlannerWithRegistry(cfg, nil)
}

func NewPlannerWithRegistry(cfg PlannerConfig, registry *MemoryRegistry) *Planner {
	if cfg.Mode == "" {
		cfg.Mode = "automatic"
	}
	if cfg.TTL == "" {
		cfg.TTL = "5m"
	}
	if cfg.MaxBreakpoints == 0 {
		cfg.MaxBreakpoints = 4
	}
	if cfg.ExpectedReuse == 0 {
		cfg.ExpectedReuse = 1
	}
	return &Planner{cfg: cfg, registry: registry}
}

func (planner *Planner) Plan(input PlanInput) (CacheCreationPlan, error) {
	log := logger.L().With("model", input.Model)
	if !planner.cfg.PromptCaching || planner.cfg.Mode == "off" {
		log.Debug("cache disabled", "reason", "prompt_caching_disabled")
		return CacheCreationPlan{Mode: "off", TTL: planner.cfg.TTL, Reason: "prompt_caching_disabled"}, nil
	}
	if planner.cfg.MinCacheTokens > 0 && input.EstimatedTokens > 0 && input.EstimatedTokens < planner.cfg.MinCacheTokens {
		log.Debug("cache disabled", "reason", "below_min_cache_tokens", "estimated_tokens", input.EstimatedTokens, "min", planner.cfg.MinCacheTokens)
		return CacheCreationPlan{Mode: "off", TTL: planner.cfg.TTL, Reason: "below_min_cache_tokens"}, nil
	}
	if planner.cfg.MinimumValueScore > 0 && input.EstimatedTokens*planner.cfg.ExpectedReuse < planner.cfg.MinimumValueScore {
		log.Debug("cache disabled", "reason", "below_minimum_value_score", "estimated_tokens", input.EstimatedTokens, "expected_reuse", planner.cfg.ExpectedReuse)
		return CacheCreationPlan{Mode: "off", TTL: planner.cfg.TTL, Reason: "below_minimum_value_score"}, nil
	}

	useAutomatic := planner.cfg.AutomaticPromptCache && (planner.cfg.Mode == "automatic" || planner.cfg.Mode == "hybrid")
	useExplicit := planner.cfg.ExplicitCacheBreakpoints && (planner.cfg.Mode == "automatic" || planner.cfg.Mode == "explicit" || planner.cfg.Mode == "hybrid")
	if !useAutomatic && !useExplicit {
		return CacheCreationPlan{Mode: "off", TTL: planner.cfg.TTL, Reason: "cache_controls_disabled"}, nil
	}

	plan := CacheCreationPlan{
		Mode:       effectiveMode(useAutomatic, useExplicit),
		TTL:        planner.cfg.TTL,
		LocalKey:   localKey(input, planner.cfg.TTL),
		WarmPolicy: "none",
	}
	if planner.registry != nil {
		if entry, ok := planner.registry.Get(plan.LocalKey); ok && entry.State == StateWarm && (entry.ExpiresAt.IsZero() || entry.ExpiresAt.After(time.Now())) {
			plan.Reason = "registry_warm"
			log.Debug("cache registry warm", "key", plan.LocalKey)
		}
	}

	if useExplicit {
		plan.Breakpoints = planner.breakpoints(input)
		log.Debug("cache plan", "mode", plan.Mode, "breakpoints", len(plan.Breakpoints), "reason", plan.Reason)
		if len(plan.Breakpoints) == 0 {
			if useAutomatic {
				plan.Mode = "automatic"
				plan.Reason = "no_stable_breakpoints"
				return plan, nil
			}
			return CacheCreationPlan{
				Mode:     "off",
				TTL:      planner.cfg.TTL,
				LocalKey: plan.LocalKey,
				Reason:   "no_stable_breakpoints",
			}, nil
		}
	}
	log.Debug("cache plan", "mode", plan.Mode, "breakpoints", len(plan.Breakpoints), "reason", plan.Reason)
	return plan, nil
}

func effectiveMode(useAutomatic bool, useExplicit bool) string {
	switch {
	case useAutomatic && useExplicit:
		return "hybrid"
	case useAutomatic:
		return "automatic"
	case useExplicit:
		return "explicit"
	default:
		return "off"
	}
}

func (planner *Planner) breakpoints(input PlanInput) []CacheBreakpoint {
	maxBreakpoints := planner.cfg.MaxBreakpoints
	if maxBreakpoints <= 0 {
		maxBreakpoints = 4
	}
	breakpoints := make([]CacheBreakpoint, 0, maxBreakpoints)
	add := func(scope, path, hash string, scopeIndex int, contentIndex int) {
		if len(breakpoints) >= maxBreakpoints || hash == "" {
			return
		}
		breakpoints = append(breakpoints, CacheBreakpoint{
			Scope:        scope,
			BlockPath:    path,
			TTL:          planner.cfg.TTL,
			Hash:         hash,
			ScopeIndex:   scopeIndex,
			ContentIndex: contentIndex,
		})
	}
	if input.ToolCount > 0 {
		lastToolIndex := input.ToolCount - 1
		add("tools", "tools["+itoa(lastToolIndex)+"]", input.ToolsHash, lastToolIndex, -1)
	}
	if input.SystemBlockCount > 0 {
		lastSystemIndex := input.SystemBlockCount - 1
		add("system", "system["+itoa(lastSystemIndex)+"]", input.SystemHash, lastSystemIndex, -1)
	}
	remaining := maxBreakpoints - len(breakpoints)
	if remaining > 0 {
		for _, candidate := range selectedMessageBreakpoints(input, remaining) {
			hash := candidate.Hash
			if hash == "" {
				hash = input.MessagePrefixHash
			}
			add("messages", candidate.BlockPath, hash, candidate.MessageIndex, candidate.ContentIndex)
		}
	}
	return breakpoints
}

func selectedMessageBreakpoints(input PlanInput, limit int) []MessageBreakpointCandidate {
	if limit <= 0 {
		return nil
	}
	candidates := input.MessageBreakpoints
	if len(candidates) == 0 && input.MessageCount > 0 {
		lastMessageIndex := input.MessageCount - 1
		candidates = []MessageBreakpointCandidate{{
			MessageIndex: lastMessageIndex,
			ContentIndex: -1,
			BlockPath:    "messages[" + itoa(lastMessageIndex) + "].content[last]",
			Hash:         input.MessagePrefixHash,
		}}
	}
	if len(candidates) == 0 {
		return nil
	}

	preferred := make([]MessageBreakpointCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Role == "user" {
			preferred = append(preferred, candidate)
		}
	}

	selected := evenlySpacedMessageBreakpoints(preferred, limit)
	if len(selected) >= limit {
		return selected
	}

	usedPaths := make(map[string]struct{}, len(selected))
	for _, candidate := range selected {
		usedPaths[candidate.BlockPath] = struct{}{}
	}

	remaining := make([]MessageBreakpointCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := usedPaths[candidate.BlockPath]; ok {
			continue
		}
		remaining = append(remaining, candidate)
	}
	selected = append(selected, evenlySpacedMessageBreakpoints(remaining, limit-len(selected))...)
	return selected
}

func evenlySpacedMessageBreakpoints(candidates []MessageBreakpointCandidate, limit int) []MessageBreakpointCandidate {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}
	if limit >= len(candidates) {
		return append([]MessageBreakpointCandidate(nil), candidates...)
	}

	selected := make([]MessageBreakpointCandidate, 0, limit)
	seen := make(map[int]struct{}, limit)
	for slot := 1; slot <= limit; slot++ {
		index := (slot*len(candidates) + limit - 1) / limit
		if index > 0 {
			index--
		}
		if index >= len(candidates) {
			index = len(candidates) - 1
		}
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		selected = append(selected, candidates[index])
	}
	if len(selected) >= limit {
		return selected
	}
	for index, candidate := range candidates {
		if len(selected) >= limit {
			break
		}
		if _, ok := seen[index]; ok {
			continue
		}
		selected = append(selected, candidate)
	}
	return selected
}

func CanonicalHash(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func localKey(input PlanInput, ttl string) string {
	parts := []string{
		input.ProviderID,
		input.UpstreamWorkspace,
		input.UpstreamAPIKeyID,
		input.Model,
		ttl,
		input.PromptCacheKey,
		input.ToolsHash,
		input.SystemHash,
		input.MessagePrefixHash,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
