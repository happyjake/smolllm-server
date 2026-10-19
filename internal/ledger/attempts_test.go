package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The distinction this exists for: a leg abandoned for silence is not a model
// failure. Counting it as one would demote a healthy model out of a chain
// because a proxy queued it.
func TestFirstOutputTimeoutIsNotCountedAsAModelError(t *testing.T) {
	t.Parallel()

	l := New()
	l.Record("agent", smolllm.RequestEvent{
		Usage:              smolllm.Usage{Provider: "p", ModelName: "m", TTFT: 30 * time.Second},
		Error:              &smolllm.FirstOutputTimeoutError{Model: "m", Phase: "headers"},
		Timestamp:          time.Now(),
		FirstOutputTimeout: true,
		FirstOutputPhase:   "headers",
	})

	byOutcome := map[string]AttemptCount{}
	for _, a := range l.Attempts() {
		byOutcome[a.Outcome] = a
	}
	require.Contains(t, byOutcome, OutcomeFirstOutput)
	assert.NotContains(t, byOutcome, OutcomeError, "silence must not be filed as a model error")
	assert.Equal(t, "headers", byOutcome[OutcomeFirstOutput].Phase)
	assert.EqualValues(t, 1, byOutcome[OutcomeFirstOutput].Count)

	// The day bucket still counts it as a failure, which is honest: the caller
	// got no answer. What must not happen is calling it the model's fault.
	buckets := l.Snapshot()
	require.Len(t, buckets, 1)
	assert.Equal(t, 1, buckets[0].Requests)
	assert.Equal(t, 1, buckets[0].Failures)
}

// A caller hanging up is not a provider problem either.
func TestCanceledIsItsOwnOutcome(t *testing.T) {
	t.Parallel()

	l := New()
	l.Record("agent", smolllm.RequestEvent{
		Usage:     smolllm.Usage{Provider: "p", ModelName: "m"},
		Error:     context.Canceled,
		Timestamp: time.Now(),
	})
	attempts := l.Attempts()
	require.Len(t, attempts, 1)
	assert.Equal(t, OutcomeCanceled, attempts[0].Outcome)
}

func TestRealErrorStaysAnError(t *testing.T) {
	t.Parallel()

	l := New()
	l.Record("agent", smolllm.RequestEvent{
		Usage:     smolllm.Usage{Provider: "p", ModelName: "m"},
		Error:     errors.New("502 bad gateway"),
		Timestamp: time.Now(),
	})
	attempts := l.Attempts()
	require.Len(t, attempts, 1)
	assert.Equal(t, OutcomeError, attempts[0].Outcome)
	assert.Empty(t, attempts[0].Phase)
}

// Only successful attempts feed the histogram, and the buckets must be
// cumulative so the rendering matches Prometheus semantics.
func TestFirstOutputHistogramIsCumulativeAndSkipsTimeouts(t *testing.T) {
	t.Parallel()

	l := New()
	record := func(ttft time.Duration, timeout bool) {
		ev := smolllm.RequestEvent{
			Usage:     smolllm.Usage{Provider: "p", ModelName: "m", TTFT: ttft},
			Timestamp: time.Now(),
		}
		if timeout {
			ev.FirstOutputTimeout = true
			ev.FirstOutputPhase = "headers"
			ev.Error = &smolllm.FirstOutputTimeoutError{Model: "m", Phase: "headers"}
		}
		l.Record("agent", ev)
	}
	record(300*time.Millisecond, false) // bucket 0 (<=0.5)
	record(3*time.Second, false)        // bucket 3 (<=5)
	record(30*time.Second, true)        // excluded

	samples := l.FirstOutputs()
	require.Len(t, samples, 1)
	s := samples[0]
	assert.EqualValues(t, 2, s.Count, "the abandoned attempt must not enter the latency histogram")

	last := s.Counts[len(s.Counts)-1]
	assert.EqualValues(t, 2, last, "+Inf bucket must equal the sample count")

	// Cumulative: monotonically non-decreasing across buckets.
	for i := 1; i < len(s.Counts); i++ {
		assert.GreaterOrEqual(t, s.Counts[i], s.Counts[i-1],
			"bucket %d regressed; counts must be cumulative", i)
	}
	// 0.3s falls in the first bucket, so every later bucket includes it.
	assert.EqualValues(t, 1, s.Counts[0])
	assert.EqualValues(t, 2, s.Counts[3])
	assert.InDelta(t, 3.3, s.Sum, 0.001)
}

// A timeout's TTFT is the budget, not a latency sample; folding it in would
// drag the distribution toward whatever budget happens to be configured.
func TestTimeoutTTFTDoesNotBiasTheHistogram(t *testing.T) {
	t.Parallel()

	l := New()
	l.Record("agent", smolllm.RequestEvent{
		Usage:              smolllm.Usage{Provider: "p", ModelName: "m", TTFT: 30 * time.Second},
		Timestamp:          time.Now(),
		FirstOutputTimeout: true,
		FirstOutputPhase:   "stream",
		Error:              &smolllm.FirstOutputTimeoutError{Model: "m", Phase: "stream"},
	})
	assert.Empty(t, l.FirstOutputs(), "an abandoned attempt is not a latency observation")
}
