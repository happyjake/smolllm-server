package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocry/smolllm-server/internal/config"
	"github.com/stretchr/testify/require"
)

// Exercise the real router/library with unavailable providers, not permissive
// Kimi nonexistent model IDs. Each response must identify the intended backup.
func TestForcedBackupsStructuralToolsAndExactReplay(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for depth := 1; depth <= 3; depth++ {
			t.Run(fmt.Sprintf("stream=%t/depth=%d", streaming, depth), func(t *testing.T) {
				target := fmt.Sprintf("target-%d", depth)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Equal(t, target, body["model"])
					require.Equal(t, "high", body["reasoning_effort"])
					messages := body["messages"].([]any)
					last := messages[len(messages)-1].(map[string]any)
					w.Header().Set("Content-Type", "text/event-stream")
					if last["role"] == "tool" {
						require.Equal(t, "call-fleet", last["tool_call_id"])
						assistant := messages[len(messages)-2].(map[string]any)
						require.Contains(t, assistant, "tool_calls")
						delta, err := json.Marshal(map[string]any{"content": last["content"]})
						require.NoError(t, err)
						fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", delta)
						fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
					} else {
						fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-fleet","type":"function","function":{"name":"probe_read","arguments":"{\"key\":"}}]}}]}`+"\n\n")
						fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"sample\"}"}}]}}]}`+"\n\n")
						fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
					}
					fmt.Fprint(w, "data: [DONE]\n\n")
				}))
				t.Cleanup(upstream.Close)
				t.Setenv("FLEETMOCK_BASE_URL", upstream.URL)
				t.Setenv("FLEETMOCK_API_KEY", "test-only")
				prefixes := make([]string, depth)
				for i := range prefixes {
					provider := fmt.Sprintf("fleet_unavailable_%d", i)
					t.Setenv(strings.ToUpper(provider)+"_BASE_URL", "")
					t.Setenv(strings.ToUpper(provider)+"_API_KEY", "")
					prefixes[i] = provider + "/no-model"
				}
				selected := "fleetmock/" + target
				chain := strings.Join(append(prefixes, selected), ",")
				cfg := &config.Config{Server: config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "local-test"}, Aliases: map[string]string{"agent": chain}}
				srv := New(config.NewStore("", cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
				router := httptest.NewServer(srv.HTTP.Handler)
				t.Cleanup(router.Close)
				messages := []map[string]any{{"role": "user", "content": "Call probe_read key=sample, then echo its result."}}
				call := func() (map[string]any, string) {
					payload, err := json.Marshal(map[string]any{"model": "agent", "stream": streaming, "reasoning_effort": "high", "messages": messages, "tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "probe_read", "parameters": map[string]any{"type": "object"}}}}})
					require.NoError(t, err)
					req, err := http.NewRequest(http.MethodPost, router.URL+"/v1/chat/completions", strings.NewReader(string(payload)))
					require.NoError(t, err)
					req.Header.Set("Authorization", "Bearer local-test")
					resp, err := http.DefaultClient.Do(req)
					require.NoError(t, err)
					defer resp.Body.Close()
					require.Equal(t, http.StatusOK, resp.StatusCode)
					message, finish := map[string]any{}, ""
					decode := func(data string) {
						var event map[string]any
						require.NoError(t, json.Unmarshal([]byte(data), &event))
						require.Equal(t, selected, event["model"])
						for _, c := range event["choices"].([]any) {
							choice := c.(map[string]any)
							field := "message"
							if streaming {
								field = "delta"
							}
							for k, v := range choice[field].(map[string]any) {
								if k == "content" && v != nil {
									previous, _ := message[k].(string)
									message[k] = previous + v.(string)
								} else if v != nil {
									message[k] = v
								}
							}
							if f, ok := choice["finish_reason"].(string); ok {
								finish = f
							}
						}
					}
					if streaming {
						scanner := bufio.NewScanner(resp.Body)
						done := false
						for scanner.Scan() {
							line := scanner.Text()
							if line == "data: [DONE]" {
								done = true
							} else if strings.HasPrefix(line, "data: ") {
								decode(strings.TrimPrefix(line, "data: "))
							}
						}
						require.NoError(t, scanner.Err())
						require.True(t, done)
					} else {
						raw, err := io.ReadAll(resp.Body)
						require.NoError(t, err)
						decode(string(raw))
					}
					return message, finish
				}
				message, finish := call()
				require.Equal(t, "tool_calls", finish)
				calls := message["tool_calls"].([]any)
				require.Len(t, calls, 1)
				tool := calls[0].(map[string]any)
				require.Equal(t, "call-fleet", tool["id"])
				function := tool["function"].(map[string]any)
				require.Equal(t, "probe_read", function["name"])
				require.JSONEq(t, `{"key":"sample"}`, function["arguments"].(string))
				message["role"] = "assistant"
				// Unpredictable temporary directory name prevents canned replay.
				nonce := "TOOL_" + t.TempDir()
				messages = append(messages, message, map[string]any{"role": "tool", "tool_call_id": "call-fleet", "content": nonce})
				result, finish := call()
				require.Equal(t, "stop", finish)
				require.Equal(t, nonce, result["content"])
			})
		}
	}
}
