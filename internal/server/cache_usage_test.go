package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/ledger"
	"github.com/stretchr/testify/require"
)

// usageFor must not claim "nothing was cached" when the provider simply did not
// report cache data — an omitted details object and {"cached_tokens":0} mean
// very different things to anyone costing a workload.
func TestUsageForOmitsCacheDetailsWhenNotReported(t *testing.T) {
	silent := usageFor(smolllm.Usage{InputTokens: 100, OutputTokens: 5})
	require.Nil(t, silent.PromptTokensDetails)
	require.Zero(t, silent.CacheCreationInputTokens)
	encoded, err := json.Marshal(silent)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "prompt_tokens_details")
	require.NotContains(t, string(encoded), "cache_creation_input_tokens")

	reported := usageFor(smolllm.Usage{
		InputTokens: 100, OutputTokens: 5, CacheReadTokens: 80, CacheWriteTokens: 20,
	})
	require.NotNil(t, reported.PromptTokensDetails)
	require.Equal(t, 80, reported.PromptTokensDetails.CachedTokens)
	require.Equal(t, 20, reported.CacheCreationInputTokens)
	require.Equal(t, 105, reported.TotalTokens)
}

func TestLedgerAggregatesCacheTokens(t *testing.T) {
	l := ledger.New()
	for _, read := range []int{80, 90} {
		l.Record("fast", smolllm.RequestEvent{
			Usage: smolllm.Usage{
				Provider: "p", ModelName: "m", InputTokens: 100, OutputTokens: 5,
				CacheReadTokens: read, CacheWriteTokens: 1,
			},
			// A zero Timestamp buckets to year 0001 and is pruned immediately by
			// the ledger's 31-day retention.
			Timestamp: time.Now().UTC(),
		})
	}
	buckets := l.Snapshot()
	require.Len(t, buckets, 1)
	require.Equal(t, 170, buckets[0].CacheReadTokens)
	require.Equal(t, 2, buckets[0].CacheWriteTokens)
	require.Equal(t, 200, buckets[0].InputTokens)
}
