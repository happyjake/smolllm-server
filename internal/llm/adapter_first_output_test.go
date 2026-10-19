package llm

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/stretchr/testify/require"
)

func agentRequest(t *testing.T) *ChatRequest {
	t.Helper()
	var req ChatRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"agent","messages":[{"role":"user","content":"hi"}]}`), &req))
	return &req
}

func firstOutputOption(t *testing.T, opts []smolllm.Option) smolllm.Options {
	t.Helper()
	resolved := smolllm.Options{}
	for _, opt := range opts {
		opt(&resolved)
	}
	return resolved
}

func TestBuildOptions_FirstOutputTimeoutReachesLibrary(t *testing.T) {
	t.Parallel()
	req := agentRequest(t)
	budget := 45.0

	_, opts, err := BuildOptions(req, nil, 180, &budget)
	require.NoError(t, err)
	require.Equal(t, 45*time.Second, firstOutputOption(t, opts).FirstOutputTimeout)
}

// Omitting the key must not append a disabling option: the library fills the
// default when no option is applied. Asserting "no option" is the only thing
// observable here, since the default lives in the library's unexported
// defaultOptions.
func TestBuildOptions_NilLeavesGuardToLibraryDefault(t *testing.T) {
	t.Parallel()
	req := agentRequest(t)

	_, opts, err := BuildOptions(req, nil, 180, nil)
	require.NoError(t, err)
	require.Zero(t, firstOutputOption(t, opts).FirstOutputTimeout,
		"a nil config must not override the library default")
}

func TestBuildOptions_ZeroDisablesGuard(t *testing.T) {
	t.Parallel()
	req := agentRequest(t)
	off := 0.0

	_, opts, err := BuildOptions(req, nil, 180, &off)
	require.NoError(t, err)
	require.Zero(t, firstOutputOption(t, opts).FirstOutputTimeout)
}
