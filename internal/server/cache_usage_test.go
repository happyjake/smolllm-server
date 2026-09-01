package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// A streaming client that asked for usage must receive it. Coding agents stream,
// so without this frame cache accounting is invisible on the transport that
// matters most.
func TestStreamingEmitsUsageWhenRequested(t *testing.T) {
	ts, _ := newTestRig(t, true)
	resp := postChat(t, ts, `{"model":"fast","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var usageFrames int
	for _, line := range strings.Split(string(body), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || strings.TrimSpace(payload) == "[DONE]" {
			continue
		}
		var chunk map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &chunk))
		if _, has := chunk["usage"]; has {
			usageFrames++
		}
	}
	require.Equal(t, 1, usageFrames, "expected exactly one usage frame before [DONE]")
}

// Without include_usage the stream must stay byte-compatible with before.
func TestStreamingOmitsUsageByDefault(t *testing.T) {
	ts, _ := newTestRig(t, true)
	resp := postChat(t, ts, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NotContains(t, string(body), `"usage"`)
}

// tool_calls: [] and null are meaningful to some providers; the key must not be
// dropped just because the union decoded no calls.
func TestEmptyToolCallsSurviveToProvider(t *testing.T) {
	for _, raw := range []string{`[]`, `null`} {
		t.Run(raw, func(t *testing.T) {
			var got map[string]any
			ts, _ := newTestRig(t, false, func(body map[string]any) { got = body })
			resp := postChat(t, ts, `{"model":"fast","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"sure","tool_calls":`+raw+`}]}`)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)

			assistant := got["messages"].([]any)[1].(map[string]any)
			require.Contains(t, assistant, "tool_calls",
				"an explicitly sent tool_calls key must reach the provider")
		})
	}
}
