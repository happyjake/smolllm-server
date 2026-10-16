package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocry/smolllm-server/internal/config"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/stretchr/testify/require"
)

func TestCreditTallyReportedMissingZeroAndRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      []string
		credit   *json.Number
		known    int
		complete bool
	}{
		{"reported", []string{`{"credit":2.62}`}, ptrCredit(2.62), 1, true},
		{"explicit-zero", []string{`{"credit":0}`}, ptrCredit(0), 1, true},
		{"absent", []string{`{"prompt_tokens":1}`}, nil, 0, false},
		{"invalid", []string{`{"credit":"2.62"}`, `{"credit":-1}`, `{"credit":null}`}, nil, 0, false},
		{"retry-sum", []string{`{"credit":1.5}`, `{"credit":2.62}`}, ptrCredit(4.12), 2, true},
		{"partial-retry", []string{`{}`, `{"credit":2.62}`}, ptrCredit(2.62), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := new(creditTally)
			for _, raw := range tc.raw {
				b.record(smolllm.RequestEvent{Usage: smolllm.Usage{RawUsage: json.RawMessage(raw)}})
			}
			u := b.apply(usageFor(smolllm.Usage{}))
			require.Equal(t, tc.credit, u.Credit)
			require.Equal(t, len(tc.raw), u.CreditAttempts)
			require.Equal(t, tc.known, u.CreditKnownAttempts)
			require.Equal(t, tc.complete, u.CreditComplete)
		})
	}
}
func ptrCredit(v float64) *json.Number { raw, _ := json.Marshal(v); n := json.Number(raw); return &n }

// Exercise the full HTTP→library hook→server usage path, not just the fold.
func TestCreditsAcrossWireModesAndCompatibilityRetry(t *testing.T) {
	for _, mode := range []string{"blocking", "streaming", "compat-retry", "http-error", "stream-error"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if mode == "http-error" || (mode == "compat-retry" && n == 1) {
					status := 500
					if mode == "compat-retry" {
						status = 400
					}
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":{"message":"rejected"},"usage":{"credit":1.5}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
				// Cumulative snapshots must replace, not add.
				for i := 0; i < 2; i++ {
					fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"credit\":2.62}}\n\n")
				}
				if mode == "stream-error" {
					fmt.Fprint(w, "data: BROKEN\n\n")
				} else {
					fmt.Fprint(w, "data: [DONE]\n\n")
				}
			}))
			defer upstream.Close()
			t.Setenv("MOCK_BASE_URL", upstream.URL)
			t.Setenv("MOCK_API_KEY", "dummy")
			cfg := &config.Config{Server: config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "warn"}, Aliases: map[string]string{"fast": "mock/test"}}
			srv := New(config.NewStore("", cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
			ts := httptest.NewServer(srv.HTTP.Handler)
			defer ts.Close()
			streaming := mode != "blocking"
			resp := postChat(t, ts, fmt.Sprintf(`{"model":"fast","stream":%t,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, streaming))
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			if mode == "http-error" {
				require.Equal(t, 502, resp.StatusCode)
				require.Equal(t, "4.5", resp.Header.Get("X-Usage-Credit"))
				require.Equal(t, "3", resp.Header.Get("X-Usage-Credit-Attempts"))
				require.Equal(t, "true", resp.Header.Get("X-Usage-Credit-Complete"))
				require.NotEmpty(t, resp.Header.Get("X-Usage-Credit-Report-Id"))
				return
			}
			require.Equal(t, 200, resp.StatusCode)
			var u map[string]any
			if streaming {
				for _, line := range strings.Split(string(body), "\n") {
					if s, ok := strings.CutPrefix(line, "data: "); ok && s != "[DONE]" {
						var v map[string]any
						require.NoError(t, json.Unmarshal([]byte(s), &v))
						if x, ok := v["usage"].(map[string]any); ok {
							u = x
						}
					}
				}
			} else {
				var v map[string]any
				require.NoError(t, json.Unmarshal(body, &v))
				u = v["usage"].(map[string]any)
			}
			expected := 2.62
			attempts := 1.
			if mode == "compat-retry" {
				expected = 4.12
				attempts = 2
			}
			require.Equal(t, expected, u["credit"])
			require.Equal(t, true, u["credit_complete"])
			require.Equal(t, attempts, u["credit_attempts"])
			require.NotEmpty(t, u["credit_report_id"])
		})
	}
}

// The final wire frame must wait for the last attempt hook, not publish the
// preceding retry's apparently complete subtotal while the hook is still running.
func TestFinalUsageWaitsForDelayedAttemptHook(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"compat"},"usage":{"credit":1.5}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"credit\":2.62}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "dummy")
	b := new(creditTally)
	var hooks atomic.Int32
	opts := []smolllm.Option{smolllm.WithModel("mock/test"), smolllm.WithHook(func(e smolllm.RequestEvent) {
		if hooks.Add(1) == 2 {
			time.Sleep(50 * time.Millisecond)
		}
		b.record(e)
	})}
	w := httptest.NewRecorder()
	h := &handlers{}
	h.chatStream(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), smolllm.PromptFromString("hi"), opts, true, b)
	require.Equal(t, int32(2), hooks.Load())
	var usage map[string]any
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if s, ok := strings.CutPrefix(line, "data: "); ok && s != "[DONE]" {
			var v map[string]any
			require.NoError(t, json.Unmarshal([]byte(s), &v))
			if u, ok := v["usage"].(map[string]any); ok {
				usage = u
			}
		}
	}
	require.Equal(t, 4.12, usage["credit"])
	require.Equal(t, 2., usage["credit_attempts"])
	require.Equal(t, 2., usage["credit_known_attempts"])
	require.Equal(t, true, usage["credit_complete"])
}

func TestCreditTallyDecimalAccuracy(t *testing.T) {
	b := new(creditTally)
	for _, raw := range []string{`{"credit":0.1}`, `{"credit":0.2}`, `{"credit":1e-7}`} {
		b.record(smolllm.RequestEvent{Usage: smolllm.Usage{RawUsage: json.RawMessage(raw)}})
	}
	u := b.apply(usageFor(smolllm.Usage{}))
	require.Equal(t, "0.3000001", u.Credit.String())
	require.NotEmpty(t, u.CreditReportID)
	require.Equal(t, u.CreditReportID, b.apply(usageFor(smolllm.Usage{})).CreditReportID)
}
