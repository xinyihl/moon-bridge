package stats

import (
	"bytes"
	"strings"
	"testing"
)

func TestSummaryLogValueAlwaysIncludesCost(t *testing.T) {
	attrs := (Summary{}).LogValue().Group()
	for _, attr := range attrs {
		if attr.Key == "cost_cny" {
			return
		}
	}
	t.Fatalf("cost_cny missing from log attrs: %+v", attrs)
}

func TestWriteSummaryAlwaysIncludesTotalCost(t *testing.T) {
	var output bytes.Buffer

	WriteSummary(&output, Summary{})

	for _, want := range []string{
		"统计：缓存命中率 0.0%",
		"累计计费 0.00 元",
		"累计费用:",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("summary missing %q: %s", want, output.String())
		}
	}
}

func TestFormatUsageLine(t *testing.T) {
	line := FormatUsageLine(UsageLineParams{
		RequestModel: "moonbridge",
		ActualModel:  "deepseek-v4-pro",
		Usage: Usage{
			InputTokens:              2_000_000,
			CacheCreationInputTokens: 500_000,
			CacheReadInputTokens:     500_000,
			OutputTokens:             250_000,
		},
		RequestCost:    6.789,
		TotalCost:      12.345,
		CacheHitRate:   25.00,
		CacheWriteRate: 25.00,
	})

	for _, want := range []string{
		"模型: moonbridge ➡️ deepseek-v4-pro",
		"读取 0.5000 M",
		"写入 0.5000 M",
		"首次 1.0000 M",
		"输出: 0.2500 M",
		"计费: 本请求 6.7890 元",
		"累计 12.3450 元",
		"命中率 25.00%",
		"写入率 25.00%",
		"读写比 1.00",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("usage line missing %q: %s", want, line)
		}
	}
}
