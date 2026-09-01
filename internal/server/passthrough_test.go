package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// A cache_control marker on message content must reach the provider. The openai
// param union drops keys it does not model, so before restoreMessageExtras these
// markers vanished and Anthropic-style prompt caching silently did not work —
// while the identical marker on a tool definition survived, because tools ride
// the raw-JSON pass-through path. Caching half-applied is worse than off.
func TestPassThrough_CacheControlOnMessageContent(t *testing.T) {
	var got map[string]any
	ts, _ := newTestRig(t, false, func(body map[string]any) { got = body })

	resp := postChat(t, ts, `{"model":"fast","messages":[
		{"role":"system","content":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}],
		"tools":[{"type":"function","function":{"name":"bash"},"cache_control":{"type":"ephemeral"}}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	messages, ok := got["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 2)
	for i, role := range []string{"system", "user"} {
		message := messages[i].(map[string]any)
		require.Equal(t, role, message["role"])
		parts, ok := message["content"].([]any)
		require.Truef(t, ok, "%s content should stay an array of parts", role)
		part := parts[0].(map[string]any)
		require.Equalf(t, map[string]any{"type": "ephemeral"}, part["cache_control"],
			"cache_control stripped from %s content", role)
		require.Equal(t, "text", part["type"])
	}
	require.Contains(t, string(mustJSON(t, got["tools"])), "cache_control")
}

// Top-level keys the server does not model reach the provider unchanged.
func TestPassThrough_UnmodeledTopLevelFields(t *testing.T) {
	var got map[string]any
	ts, _ := newTestRig(t, false, func(body map[string]any) { got = body })

	resp := postChat(t, ts, `{"model":"fast","messages":[{"role":"user","content":"hi"}],
		"prompt_cache_retention":"24h","metadata":{"session":"abc"},"future_field":[1,2]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, "24h", got["prompt_cache_retention"])
	require.Equal(t, map[string]any{"session": "abc"}, got["metadata"])
	require.Equal(t, []any{float64(1), float64(2)}, got["future_field"])
}

// stream_options is read back by smolllm-go's stream parser and usage collection,
// so smolllm.WithExtraBody PANICS when handed one. Forwarding a client's value
// would drop the connection (net/http recovers a handler panic into a reset, not
// a 400) on the first OpenAI-SDK client — and those send it routinely. The
// client's value must be dropped and the library's own kept.
func TestPassThrough_ClientStreamOptionsCannotReachExtraBody(t *testing.T) {
	var got map[string]any
	ts, _ := newTestRig(t, false, func(body map[string]any) { got = body })

	resp := postChat(t, ts, `{"model":"fast","messages":[{"role":"user","content":"hi"}],
		"stream_options":{"include_usage":false,"hijacked":true}}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	options, ok := got["stream_options"].(map[string]any)
	require.True(t, ok, "library's own stream_options must still be present")
	require.NotContains(t, options, "hijacked", "client must not override stream_options")
	require.Equal(t, true, options["include_usage"])
}

// A message the union encodes losslessly must produce no extras at all: the
// subset check must not mistake normal round-tripping for loss and start
// force-overriding content that was never damaged.
func TestPassThrough_LosslessMessageIsUntouched(t *testing.T) {
	var got map[string]any
	ts, _ := newTestRig(t, false, func(body map[string]any) { got = body })

	resp := postChat(t, ts, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	message := got["messages"].([]any)[0].(map[string]any)
	require.Equal(t, "hi", message["content"], "plain string content must not become an array")
	for key := range message {
		require.Contains(t, []string{"role", "content"}, key, "unexpected key %q added to a clean message", key)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
