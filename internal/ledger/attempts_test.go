package ledger

import (
	"context"
	"errors"
	"fmt"
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

// The model label arrives from requests, so both per-series maps are capped:
// past the cap an existing series keeps counting and an unseen one is dropped.
func TestAttemptSeriesAreCapped(t *testing.T) {
	t.Parallel()

	l := New()
	l.maxAttemptSeries = 3
	l.maxFirstOutputSeries = 3
	for i := 0; i < 10; i++ {
		l.Record("agent", smolllm.RequestEvent{
			Usage:     smolllm.Usage{Provider: "doom", ModelName: fmt.Sprintf("unique-%d", i), TTFT: 2 * time.Second},
			Timestamp: time.Now(),
		})
	}
	assert.Len(t, l.attempts, 3, "no new attempt series past the cap")
	assert.Len(t, l.firstOutput, 3, "no new histogram series past the cap")

	l.Record("agent", smolllm.RequestEvent{
		Usage:     smolllm.Usage{Provider: "doom", ModelName: "unique-0", TTFT: 2 * time.Second},
		Timestamp: time.Now(),
	})
	assert.Equal(t, int64(2), l.attempts[attemptKey{
		alias: "agent", provider: "doom", model: "unique-0", outcome: OutcomeOK,
	}], "an existing series keeps counting past the cap")
}

// Day buckets are capped too: unique requested models past the cap drop
// their day-bucket instead of growing the map and every Snapshot copy.
func TestDayBucketsAreCapped(t *testing.T) {
	t.Parallel()

	l := New()
	l.maxBucketSeries = 3
	for i := 0; i < 10; i++ {
		l.Record("agent", smolllm.RequestEvent{
			Usage:     smolllm.Usage{Provider: "doom", ModelName: fmt.Sprintf("unique-%d", i)},
			Timestamp: time.Now(),
		})
	}
	assert.Len(t, l.buckets, 3, "no new day-bucket past the cap")
	assert.Len(t, l.Snapshot(), 3, "/stats output stays bounded")

	l.Record("agent", smolllm.RequestEvent{
		Usage:     smolllm.Usage{Provider: "doom", ModelName: "unique-0"},
		Timestamp: time.Now(),
	})
	assert.EqualValues(t, 2, l.buckets[bucketKey{
		day: time.Now().UTC().Format(time.DateOnly), alias: "agent", provider: "doom", model: "unique-0",
	}].Requests, "an existing bucket keeps counting past the cap")
}

// Pruning runs before the cap check: expired rows release capacity on every
// Record, so a full map of stale rows must admit a fresh row.
func TestPruneRunsBeforeTheBucketCap(t *testing.T) {
	t.Parallel()

	l := New()
	l.maxBucketSeries = 2
	// Seed two expired rows directly: recording them through Record would
	// prune them right away, and the scenario under test is a map already
	// full of stale rows when a fresh key arrives.
	stale := time.Now().UTC().AddDate(0, 0, -40).Format(time.DateOnly)
	for i := 0; i < 2; i++ {
		l.buckets[bucketKey{day: stale, alias: "agent", provider: "doom", model: fmt.Sprintf("stale-%d", i)}] = Bucket{
			Day: stale, Alias: "agent", Provider: "doom", Model: fmt.Sprintf("stale-%d", i), Requests: 1,
		}
	}
	require.Len(t, l.buckets, 2)

	l.Record("agent", smolllm.RequestEvent{
		Usage:     smolllm.Usage{Provider: "doom", ModelName: "fresh"},
		Timestamp: time.Now(),
	})
	today := time.Now().UTC().Format(time.DateOnly)
	key := bucketKey{day: today, alias: "agent", provider: "doom", model: "fresh"}
	assert.EqualValues(t, 1, l.buckets[key].Requests, "stale rows were pruned and the fresh row was recorded")
	assert.Len(t, l.buckets, 1, "only the fresh row remains")
}
