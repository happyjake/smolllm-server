package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/ledger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A streamed first-output timeout must reach the ledger.
//
// This is a regression test for a bug that shipped: the first-output guard runs
// after the stream is established, so when it abandoned a leg nothing downstream
// emitted a hook. The identical blocking failure was recorded, so the asymmetry
// was invisible in code review — only a live silent upstream showed it, by
// recording one attempt through Ask and zero through Stream.
//
// The consequence was not cosmetic. `smolllm_attempts_total{outcome=
// "first_output_timeout"}` is what separates "queued upstream" from "model is
// broken", and streaming is the transport production uses. With this bug that
// counter could never move, leaving both the hang-share panel's timeout half and
// the SmolllmFirstOutputTimeouts alert permanently unfirable.
//
// Deliberately wired through the real library and the real ledger.Hook rather
// than by handing the ledger a fabricated event: a test that constructs the
// event itself stays green while the library emits nothing, which is exactly how
// this got missed. The unit test one level down (ledger.TestAttemptsAndSamples
// CarryStreamingMode) has that blind spot by design.
func TestStreamedFirstOutputTimeoutReachesTheLedger(t *testing.T) {
	t.Parallel()

	// An upstream that accepts the request, sends headers, and then says
	// nothing: the queueing failure the guard exists for. It is not an error,
	// so nothing retries it and nothing fails fast — only the budget ends it.
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer silent.Close()

	budget := 0.15 // seconds
	ldg := ledger.New()
	ledgerHook := ldg.Hook("agent")
	hook := smolllm.WithHook(func(e smolllm.RequestEvent) { ledgerHook(e) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Stream, not Ask: the blocking path never lost the event, so testing it
	// would pass with or without the fix.
	resp, err := smolllm.Stream(ctx, smolllm.PromptFromString("hi"),
		smolllm.WithModel("openai/model-a"),
		smolllm.WithBaseURL(silent.URL+"/"),
		smolllm.WithAPIKey("test-key"),
		smolllm.WithFirstOutputTimeout(time.Duration(budget*float64(time.Second))),
		smolllm.WithTimeout(10*time.Second),
		hook,
	)
	if err == nil {
		err = resp.Stream.Wait()
		_, _ = io.Copy(io.Discard, strings.NewReader(""))
	}
	require.Error(t, err, "a silent upstream must fail, not hang")

	phase, ok := smolllm.IsFirstOutputTimeout(err)
	require.True(t, ok, "expected a first-output timeout, got %v", err)
	assert.Equal(t, "stream", phase)

	// The assertion the bug violated.
	attempts := ldg.Attempts()
	require.NotEmpty(t, attempts,
		"a streamed first-output timeout left no attempt in the ledger")

	var found bool
	for _, a := range attempts {
		if a.Outcome == ledger.OutcomeFirstOutput {
			found = true
			assert.Equal(t, "stream", a.Phase, "phase must survive into the ledger")
			assert.True(t, a.Streaming, "the streaming dimension must be recorded too")
			assert.Equal(t, int64(1), a.Count)
		}
	}
	assert.True(t, found,
		"no outcome=%q attempt; streaming timeouts would be invisible to the metric "+
			"that distinguishes queueing from a broken model", ledger.OutcomeFirstOutput)

	// The latency histogram must NOT gain this attempt. A timeout's "latency"
	// is the budget itself, already visible as an outcome; folding it into the
	// distribution would drag p90 toward whatever budget is configured and make
	// the panel describe the guard rather than the upstream. Pinned here so the
	// ledger event test above cannot later be "fixed" by also histogramming it.
	assert.Empty(t, ldg.FirstOutputs(),
		"a timed-out attempt must not enter the first-output latency histogram")
}
