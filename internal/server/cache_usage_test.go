package server

import (
	"encoding/json"
	"fmt"
	"github.com/rocry/smolllm-server/internal/config"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	require.Nil(t, silent.CacheCreationInputTokens)
	encoded, err := json.Marshal(silent)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "prompt_tokens_details")
	require.NotContains(t, string(encoded), "cache_creation_input_tokens")

	reported := usageFor(smolllm.Usage{
		InputTokens: 100, OutputTokens: 5, CacheReadTokens: 80, CacheWriteTokens: 20,
		CacheReadReported: true, CacheWriteReported: true,
	})
	require.NotNil(t, reported.PromptTokensDetails)
	require.Equal(t, 80, reported.PromptTokensDetails.CachedTokens)
	require.NotNil(t, reported.CacheCreationInputTokens)
	require.Equal(t, 20, *reported.CacheCreationInputTokens)
	require.Equal(t, 105, reported.TotalTokens)

	// An explicit zero is a real answer — this call missed — and must be
	// distinguishable from the provider saying nothing at all.
	explicitMiss := usageFor(smolllm.Usage{
		InputTokens: 100, OutputTokens: 5, CacheReadTokens: 0, CacheReadReported: true,
	})
	require.NotNil(t, explicitMiss.PromptTokensDetails)
	require.Equal(t, 0, explicitMiss.PromptTokensDetails.CachedTokens)
	encodedMiss, err := json.Marshal(explicitMiss)
	require.NoError(t, err)
	require.Contains(t, string(encodedMiss), `"cached_tokens":0`)

	// Writes get the same treatment as reads: an explicit zero survives.
	explicitNoWrite := usageFor(smolllm.Usage{
		InputTokens: 100, CacheWriteTokens: 0, CacheWriteReported: true,
	})
	require.NotNil(t, explicitNoWrite.CacheCreationInputTokens)
	encodedNoWrite, err := json.Marshal(explicitNoWrite)
	require.NoError(t, err)
	require.Contains(t, string(encodedNoWrite), `"cache_creation_input_tokens":0`)
}

func TestLedgerAggregatesCacheTokens(t *testing.T) {
	l := ledger.New()
	for _, read := range []int{80, 90} {
		l.Record("fast", smolllm.RequestEvent{
			Usage: smolllm.Usage{
				Provider: "p", ModelName: "m", InputTokens: 100, OutputTokens: 5,
				CacheReadTokens: read, CacheWriteTokens: 1,
				CacheReadReported: true, CacheWriteReported: true,
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
	require.Equal(t, 2, buckets[0].CacheReportedRequests)
	require.Equal(t, 200, buckets[0].CacheReportedInputTokens)

	// A provider that reported only WRITES has said nothing about reads, so it
	// must not enlarge the read denominator.
	l.Record("fast", smolllm.RequestEvent{
		Usage: smolllm.Usage{
			Provider: "p", ModelName: "m", InputTokens: 100,
			CacheWriteTokens: 5, CacheWriteReported: true,
		},
		Timestamp: time.Now().UTC(),
	})
	buckets = l.Snapshot()
	require.Equal(t, 2, buckets[0].CacheReportedRequests, "write-only reporting is not read coverage")
	require.Equal(t, 200, buckets[0].CacheReportedInputTokens)
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

// tool_calls forwarded verbatim must not replay the streaming-only "index" key,
// which restoreToolCallExtras deliberately drops and strict providers reject.
func TestForwardedToolCallsDropStreamingIndex(t *testing.T) {
	var got map[string]any
	ts, _ := newTestRig(t, false, func(body map[string]any) { got = body })
	resp := postChat(t, ts, `{"model":"fast","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"custom","index":0,"custom":{"name":"run","input":"x"},"extra_content":{"sig":"abc"}}]}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assistant := got["messages"].([]any)[1].(map[string]any)
	calls, ok := assistant["tool_calls"].([]any)
	require.True(t, ok, "a custom tool call must still reach the provider")
	call := calls[0].(map[string]any)
	require.NotContains(t, call, "index", "streaming-only index must not be replayed")
	require.Equal(t, map[string]any{"sig": "abc"}, call["extra_content"],
		"a custom call's provider extras must survive")
}

// A failed turn still costs tokens. A client that asked for usage needs it most
// on the path where something went wrong, so the error exit emits the frame too.
func TestStreamingErrorStillEmitsUsageWhenRequested(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(w, "data: {bad-json}\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "secret-mock-key")

	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "warn"},
		Aliases: map[string]string{"fast": "mock/marvin-7b"},
	}
	srv := New(config.NewStore("", cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.HTTP.Handler)
	t.Cleanup(ts.Close)

	resp := postChat(t, ts, `{"model":"fast","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var cached float64
	var sawError, sawUsage bool
	for _, line := range strings.Split(string(body), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || strings.TrimSpace(payload) == "[DONE]" {
			continue
		}
		var chunk map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &chunk))
		if _, has := chunk["error"]; has {
			sawError = true
		}
		if usage, has := chunk["usage"].(map[string]any); has {
			sawUsage = true
			details := usage["prompt_tokens_details"].(map[string]any)
			cached = details["cached_tokens"].(float64)
		}
	}
	require.True(t, sawError, "the error frame must still be sent")
	require.True(t, sawUsage, "usage must be delivered on the error path too")
	require.Equal(t, float64(80), cached, "cache counts from before the failure must survive")
}
