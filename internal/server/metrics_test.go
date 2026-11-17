package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rocry/smolllm-go/smolllm"
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
	assert.Contains(t, out, `smolllm_attempt_first_output_seconds_bucket{alias="agent",provider="p",model="m",streaming="false",le="0.5"} 1`)
	assert.Contains(t, out, `smolllm_attempt_first_output_seconds_bucket{alias="agent",provider="p",model="m",streaming="false",le="2"} 3`)
	assert.Contains(t, out, `smolllm_attempt_first_output_seconds_bucket{alias="agent",provider="p",model="m",streaming="false",le="+Inf"} 3`)
	assert.Contains(t, out, `smolllm_attempt_first_output_seconds_sum{alias="agent",provider="p",model="m",streaming="false"} 4.5`)
	assert.Contains(t, out, `smolllm_attempt_first_output_seconds_count{alias="agent",provider="p",model="m",streaming="false"} 3`)
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
	assert.Contains(t, out, `outcome="first_output_timeout",phase="headers",streaming="false"} 4`)
	// Non-timeout outcomes still carry the phase label, empty, so the label set
	// does not change shape between outcomes. streaming is always present for
	// the same reason: a series must never appear or vanish by label shape.
	assert.Contains(t, out, `outcome="ok",phase="",streaming="false"} 10`)
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

// Probes and the half-open state need the breaker's clock to move, which the
// end-to-end tests cannot do; the rendering contract is pinned here instead.
func TestAccountMetricsRenderProbesAndHalfOpenState(t *testing.T) {
	t.Parallel()

	m := newAccountMetrics()
	m.hook("agent")(smolllm.AccountEvent{
		Kind: smolllm.AccountProbing, Provider: "doom", Account: "0123456789ab", ModelName: "a",
	})
	m.hook("agent")(smolllm.AccountEvent{
		Kind: smolllm.AccountSkipped, Provider: "zai", Account: "ba9876543210", ModelName: `q"x`,
		SkipReason: smolllm.SkipCircuitOpen,
	})
	var sb strings.Builder
	m.write(&sb, []smolllm.AccountCircuit{{Provider: "doom", Account: "0123456789ab", ModelName: "a", State: smolllm.CircuitHalfOpen}})
	out := sb.String()

	assert.Contains(t, out, "# TYPE smolllm_account_circuit_probes_total counter\n")
	assert.Contains(t, out, `smolllm_account_circuit_probes_total{alias="agent",provider="doom",account="0123456789ab",model="a"} 1`+"\n")
	assert.Contains(t, out, `smolllm_account_circuit_skips_total{alias="agent",provider="zai",account="ba9876543210",model="q\"x",reason="circuit_open"} 1`+"\n")
	assert.Contains(t, out, `smolllm_account_circuit_state{provider="doom",account="0123456789ab",model="a",state="half_open"} 1`+"\n")
	assert.Contains(t, out, `smolllm_account_circuit_state{provider="doom",account="0123456789ab",model="a",state="open"} 0`+"\n")
	assert.Contains(t, out, `smolllm_account_circuit_state{provider="zai",account="ba9876543210",model="q\"x",state="closed"} 1`+"\n")
	assert.NotContains(t, out, "smolllm_account_circuit_transitions_total")
}

// Model names arrive from requests, so the state-gauge pair set is capped:
// pairs past the cap simply have no closed series, and the map never grows
// with request diversity.
func TestSeenPairsAreCapped(t *testing.T) {
	t.Parallel()

	m := newAccountMetrics()
	m.maxSeen = 3
	for i := 0; i < 10; i++ {
		m.hook("agent")(smolllm.AccountEvent{
			Kind: smolllm.AccountUnrecognized429, Provider: "doom",
			Account: fmt.Sprintf("%012x", i), ModelName: "m",
		})
	}
	assert.Len(t, m.seen, 3, "seen never exceeds its cap")

	var sb strings.Builder
	m.write(&sb, nil)
	assert.Equal(t, 3*len(circuitStates), strings.Count(sb.String(), "smolllm_account_circuit_state{"), "one three-state series per remembered pair")
}

// Counter series are capped too: the library bounds live circuits, but
// eviction lets unique model names rotate through them forever, so a stream
// of opened transitions must stop creating series past the cap while an
// existing series keeps counting.
func TestCounterSeriesAreCapped(t *testing.T) {
	t.Parallel()

	m := newAccountMetrics()
	m.maxSeries = 3
	for i := 0; i < 10; i++ {
		m.hook("agent")(smolllm.AccountEvent{
			Kind: smolllm.AccountOpened, Provider: "doom",
			Account: fmt.Sprintf("%012x", i), ModelName: "m",
		})
	}
	assert.Len(t, m.transitions, 3, "no new series past the cap")

	first := accountTransitionKey{accountLegID: accountLegID{
		accountID: accountID{provider: "doom", account: fmt.Sprintf("%012x", 0)},
		model:     "m",
	}, transition: "opened"}
	assert.Equal(t, int64(1), m.transitions[first])
	m.hook("agent")(smolllm.AccountEvent{
		Kind: smolllm.AccountOpened, Provider: "doom",
		Account: fmt.Sprintf("%012x", 0), ModelName: "m",
	})
	assert.Equal(t, int64(2), m.transitions[first], "an existing series keeps counting past the cap")
}
