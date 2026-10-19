package ledger

import (
	"testing"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A streaming and a blocking attempt are different measurements: a streaming
// TTFT is time to first byte, a blocking TTFT is the whole call. Keeping them
// in one series makes the latency distribution describe neither, so the mode
// must survive into both the counter and the histogram.
func TestAttemptsAndSamplesCarryStreamingMode(t *testing.T) {
	t.Parallel()

	l := New()
	l.Record("agent", smolllm.RequestEvent{
		Usage:     smolllm.Usage{Provider: "openai", ModelName: "m", TTFT: 2 * time.Second},
		Streaming: true,
		Timestamp: time.Now().UTC(),
	})
	l.Record("agent", smolllm.RequestEvent{
		Usage:     smolllm.Usage{Provider: "openai", ModelName: "m", TTFT: 20 * time.Second},
		Streaming: false,
		Timestamp: time.Now().UTC(),
	})

	attempts := l.Attempts()
	require.Len(t, attempts, 2, "the two modes must not collapse into one series")
	seen := map[bool]int64{}
	for _, a := range attempts {
		seen[a.Streaming] = a.Count
	}
	assert.Equal(t, int64(1), seen[true], "streaming attempt series missing")
	assert.Equal(t, int64(1), seen[false], "blocking attempt series missing")

	samples := l.FirstOutputs()
	require.Len(t, samples, 2, "the two modes must not share a histogram")
	sums := map[bool]float64{}
	for _, s := range samples {
		sums[s.Streaming] = s.Sum
	}

	streaming, ok := sums[true]
	require.True(t, ok, "streaming histogram missing")
	assert.InDelta(t, 2.0, streaming, 1e-9)

	blocking, ok := sums[false]
	require.True(t, ok, "blocking histogram missing")
	assert.InDelta(t, 20.0, blocking, 1e-9)
}
