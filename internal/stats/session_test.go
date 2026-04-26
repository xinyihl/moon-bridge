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
		"Summary：Session Cache Hit Rate(AVG): 0.0%, Billing: 0.00 CNY",
		"Total Cost:",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("summary missing %q: %s", want, output.String())
		}
	}
}

func TestFormatUsageLine(t *testing.T) {
	line := FormatUsageLine("moonbridge", Usage{
		InputTokens:              1_000_000,
		CacheCreationInputTokens: 500_000,
		CacheReadInputTokens:     500_000,
		OutputTokens:             250_000,
	}, 12.345, 6.789)

	for _, want := range []string{
		"moonbridge Usage:",
		"1.500000 M Input",
		"0.250000 M Output",
		"Session Cache Hit Rate: 12.35%",
		"Billing: 6.79 CNY",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("usage line missing %q: %s", want, line)
		}
	}
}
