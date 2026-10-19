package server

import (
	"strings"
	"testing"

	"github.com/rocry/smolllm-server/internal/ledger"
	"github.com/stretchr/testify/assert"
)

// The histogram must expose the shape Prometheus expects: cumulative _bucket
// lines with a le label, a +Inf bucket equal to the count, plus _sum and
// _count. Getting this subtly wrong yields a plausible-looking graph of the
// wrong thing.
func TestMetricsRendersAValidHistogram(t *testing.T) {
	t.Parallel()

	var sb strings.Builder
	writeMetrics(&sb, nil, []ledger.FirstOutputSample{{
		Alias: "agent", Provider: "p", Model: "m",
		Bounds: []float64{0.5, 1, 2},
		Counts: []int64{1, 2, 3, 3}, // already cumulative, +Inf last
		Sum:    4.5,
		Count:  3,
	}})
	out := sb.String()

	assert.Contains(t, out, "# TYPE smolllm_attempt_first_output_seconds histogram")
	assert.Contains(t, out, `smolllm_attempt_first_output_seconds_bucket{alias="agent",provider="p",model="m",le="0.5"} 1`)
	assert.Contains(t, out, `smolllm_attempt_first_output_seconds_bucket{alias="agent",provider="p",model="m",le="2"} 3`)
	assert.Contains(t, out, `smolllm_attempt_first_output_seconds_bucket{alias="agent",provider="p",model="m",le="+Inf"} 3`)
	assert.Contains(t, out, `smolllm_attempt_first_output_seconds_sum{alias="agent",provider="p",model="m"} 4.5`)
	assert.Contains(t, out, `smolllm_attempt_first_output_seconds_count{alias="agent",provider="p",model="m"} 3`)
}

func TestMetricsRendersAttemptCounters(t *testing.T) {
	t.Parallel()

	var sb strings.Builder
	writeMetrics(&sb, []ledger.AttemptCount{
		{Alias: "agent", Provider: "p", Model: "m", Outcome: ledger.OutcomeFirstOutput, Phase: "headers", Count: 4},
		{Alias: "agent", Provider: "p", Model: "m", Outcome: ledger.OutcomeOK, Count: 10},
	}, nil)
	out := sb.String()

	assert.Contains(t, out, "# TYPE smolllm_attempts_total counter")
	assert.Contains(t, out, `outcome="first_output_timeout",phase="headers"} 4`)
	// Non-timeout outcomes still carry the phase label, empty, so the label set
	// does not change shape between outcomes.
	assert.Contains(t, out, `outcome="ok",phase=""} 10`)
}

// A model name containing a quote or newline must not corrupt the scrape: one
// bad label would otherwise invalidate every other line in the response.
func TestMetricsEscapesLabelValues(t *testing.T) {
	t.Parallel()

	var sb strings.Builder
	writeMetrics(&sb, []ledger.AttemptCount{{
		Alias: `al"ias`, Provider: "p", Model: "m\nline", Outcome: "ok", Count: 1,
	}}, nil)
	out := sb.String()

	assert.NotContains(t, out, `alias="al"ias"`, "an unescaped quote would break the line")
	assert.Contains(t, out, `alias="al\"ias"`)
	assert.Contains(t, out, `model="m\nline"`)
	// Every emitted line must still be well-formed: exactly one opening brace
	// when it has labels, and it must end with a numeric sample.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		assert.Equal(t, 1, strings.Count(line, "{"), line)
		assert.Equal(t, 1, strings.Count(line, "}"), line)
	}
}

// Whole-number bounds must not render as 5e+00: operators read these.
func TestFloatFormatting(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "5", formatFloat(5))
	assert.Equal(t, "0.5", formatFloat(0.5))
}
