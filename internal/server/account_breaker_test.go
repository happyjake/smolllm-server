package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/rocry/smolllm-server/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The structured doom/aitoken account-quota rejection. The message is neutral
// on purpose: only the nested business code may drive the breaker.
const quotaExhaustedBody = `{"error":{"data":{"code":14019,"msg":"irrelevant","requestId":"r1"}}}`

const (
	doomKey = "sk-doom-account-secret-0001"
	zaiKey  = "sk-zai-account-secret-0002"
)

// fakeProvider is one OpenAI-compatible upstream whose reply is scripted per
// model name; it counts every request each model receives.
type fakeProvider struct {
	mu      sync.Mutex
	scripts map[string]func(http.ResponseWriter)
	calls   map[string]int
}

func newFakeProvider(t *testing.T, envPrefix, key string) *fakeProvider {
	t.Helper()
	p := &fakeProvider{scripts: map[string]func(http.ResponseWriter){}, calls: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if r.Header.Get("Authorization") != "Bearer "+key || json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.calls[body.Model]++
		script := p.scripts[body.Model]
		p.mu.Unlock()
		if script == nil {
			http.Error(w, "unexpected model", http.StatusBadRequest)
			return
		}
		script(w)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(envPrefix+"_BASE_URL", srv.URL)
	t.Setenv(envPrefix+"_API_KEY", key)
	return p
}

func (p *fakeProvider) script(model string, fn func(http.ResponseWriter)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts[model] = fn
}

func (p *fakeProvider) callsTo(model string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[model]
}

func quotaExhausted(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, quotaExhaustedBody)
}

func answers(text string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]string{"content": text})
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", delta)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func newBreakerRouter(t *testing.T, aliases map[string]string, disableBreaker bool) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind: "127.0.0.1:0", AccessKey: "local-test", DisableAccountBreaker: disableBreaker,
		},
		Aliases: aliases,
	}
	srv := New(config.NewStore("", cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := httptest.NewServer(srv.HTTP.Handler)
	t.Cleanup(router.Close)
	return router
}

// chatText sends one chat request and returns the status, served model and
// assembled text, reading either the blocking body or the SSE stream.
func chatText(t *testing.T, router *httptest.Server, alias string, stream bool) (int, string, string) {
	t.Helper()
	payload := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, alias, stream)
	req, err := http.NewRequest(http.MethodPost, router.URL+"/v1/chat/completions", strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer local-test")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, "", ""
	}
	if !stream {
		var out struct {
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		require.Len(t, out.Choices, 1)
		return resp.StatusCode, out.Model, out.Choices[0].Message.Content
	}
	var model, text string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		require.NoError(t, json.Unmarshal([]byte(data), &chunk))
		model = chunk.Model
		for _, c := range chunk.Choices {
			text += c.Delta.Content
		}
	}
	require.NoError(t, scanner.Err())
	return resp.StatusCode, model, text
}

func scrapeMetrics(t *testing.T, router *httptest.Server) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, router.URL+"/metrics", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer local-test")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(raw)
}

var openedDoom = regexp.MustCompile(
	`smolllm_account_circuit_transitions_total\{provider="doom",account="([0-9a-f]{12})",model="a",transition="opened"\} 1\n`)

// One process-wide breaker serves every chat call: a 14019 on doom/a opens
// only the doom account+model-a pair, the sibling doom/b keeps answering
// every call (streaming or not, any alias), and the metrics say so without
// touching attempt counts.
func TestAccountBreakerIsSharedAcrossChatCallsAndAliases(t *testing.T) {
	doom := newFakeProvider(t, "DOOM", doomKey)
	zai := newFakeProvider(t, "ZAI", zaiKey)
	doom.script("a", quotaExhausted)
	doom.script("b", answers("same account"))
	zai.script("c", answers("other provider"))
	router := newBreakerRouter(t, map[string]string{
		"bot":   "doom/a,doom/b,zai/c",
		"other": "doom/b,zai/c",
	}, false)

	for i, call := range []struct {
		alias  string
		stream bool
	}{{"bot", false}, {"bot", true}, {"other", false}, {"other", true}} {
		status, model, text := chatText(t, router, call.alias, call.stream)
		require.Equal(t, http.StatusOK, status, "call %d", i)
		assert.Equal(t, "doom/b", model, "call %d", i)
		assert.Equal(t, "same account", text, "call %d", i)
	}
	assert.Equal(t, 1, doom.callsTo("a"), "only the quota-rejected model is circuit-open")
	assert.Equal(t, 4, doom.callsTo("b"), "the sibling model on the same account keeps serving")
	assert.Equal(t, 0, zai.callsTo("c"))

	out := scrapeMetrics(t, router)
	m := openedDoom.FindStringSubmatch(out)
	require.NotNil(t, m, "no opened transition for doom in:\n%s", out)
	acct := m[1]
	for _, want := range []string{
		"# TYPE smolllm_account_circuit_transitions_total counter",
		"# TYPE smolllm_account_circuit_skips_total counter",
		"# TYPE smolllm_account_circuit_state gauge",
		`smolllm_account_circuit_skips_total{alias="bot",provider="doom",account="` + acct + `",model="a",reason="circuit_open"} 1`,
		`smolllm_account_circuit_state{provider="doom",account="` + acct + `",model="a",state="open"} 1`,
		`smolllm_account_circuit_state{provider="doom",account="` + acct + `",model="a",state="half_open"} 0`,
		`smolllm_account_circuit_state{provider="doom",account="` + acct + `",model="a",state="closed"} 0`,
		// Attempts stay attempts: one real doom/a failure and four doom/b
		// successes split by transport; skips never touch this ledger.
		`smolllm_attempts_total{alias="bot",provider="doom",model="a",outcome="error",phase="",streaming="true"} 1`,
		`smolllm_attempts_total{alias="bot",provider="doom",model="b",outcome="ok",phase="",streaming="true"} 2`,
		`smolllm_attempts_total{alias="other",provider="doom",model="b",outcome="ok",phase="",streaming="true"} 2`,
	} {
		assert.Contains(t, out, want+"\n")
	}
	assert.NotContains(t, out, `reason="exhausted_in_request"`, "the sibling model is attempted, not skipped")
	assert.NotContains(t, out, "smolllm_account_circuit_fail_open_total{")
	for _, secret := range []string{doomKey, zaiKey, "account-secret"} {
		assert.NotContains(t, out, secret)
	}
}

// When every pair is open the chain fails open; a leg that then succeeds
// closes its circuit, and the state gauge follows.
func TestAccountBreakerFailOpenAndCloseAreObservable(t *testing.T) {
	doom := newFakeProvider(t, "DOOM", doomKey)
	zai := newFakeProvider(t, "ZAI", zaiKey)
	doom.script("a", quotaExhausted)
	zai.script("c", quotaExhausted)
	router := newBreakerRouter(t, map[string]string{"bot": "doom/a,zai/c"}, false)

	status, _, _ := chatText(t, router, "bot", true)
	require.Equal(t, http.StatusBadGateway, status)

	zai.script("c", answers("recovered early"))
	status, model, text := chatText(t, router, "bot", false)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "zai/c", model)
	assert.Equal(t, "recovered early", text)
	assert.Equal(t, 2, doom.callsTo("a"))
	assert.Equal(t, 2, zai.callsTo("c"))

	out := scrapeMetrics(t, router)
	m := openedDoom.FindStringSubmatch(out)
	require.NotNil(t, m, "no opened transition for doom in:\n%s", out)
	zaiOpened := regexp.MustCompile(
		`smolllm_account_circuit_transitions_total\{provider="zai",account="([0-9a-f]{12})",model="c",transition="opened"\} 1\n`)
	z := zaiOpened.FindStringSubmatch(out)
	require.NotNil(t, z, "no opened transition for zai in:\n%s", out)
	doomAcct, zaiAcct := m[1], z[1]
	for _, want := range []string{
		"# TYPE smolllm_account_circuit_fail_open_total counter",
		`smolllm_account_circuit_fail_open_total{alias="bot",provider="doom",account="` + doomAcct + `",model="a"} 1`,
		`smolllm_account_circuit_fail_open_total{alias="bot",provider="zai",account="` + zaiAcct + `",model="c"} 1`,
		`smolllm_account_circuit_transitions_total{provider="zai",account="` + zaiAcct + `",model="c",transition="closed"} 1`,
		`smolllm_account_circuit_state{provider="doom",account="` + doomAcct + `",model="a",state="open"} 1`,
		`smolllm_account_circuit_state{provider="zai",account="` + zaiAcct + `",model="c",state="closed"} 1`,
		`smolllm_account_circuit_state{provider="zai",account="` + zaiAcct + `",model="c",state="open"} 0`,
	} {
		assert.Contains(t, out, want+"\n")
	}
	// A fail-open re-confirmation of an already-open pair is not a new open.
	assert.NotContains(t, out, `provider="doom",account="`+doomAcct+`",model="a",transition="opened"} 2`)
}

// A JSON-bodied 429 with no recognized account code stays model-local (the
// next leg on the same account still runs) but is counted, so an unknown
// account-scoped code becomes visible before anyone parses message text.
func TestStructured429WithoutRecognizedCodeIsCountedButModelLocal(t *testing.T) {
	zai := newFakeProvider(t, "ZAI", zaiKey)
	zai.script("limited", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"1302","message":"rate limited"}}`)
	})
	zai.script("c", answers("same account"))
	router := newBreakerRouter(t, map[string]string{"bot": "zai/limited,zai/c"}, false)

	status, model, _ := chatText(t, router, "bot", true)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "zai/c", model)
	assert.Equal(t, 1, zai.callsTo("c"))

	out := scrapeMetrics(t, router)
	assert.Regexp(t,
		`smolllm_unrecognized_structured_429_total\{alias="bot",provider="zai",account="[0-9a-f]{12}",model="limited"\} 1\n`, out)
	assert.NotContains(t, out, "smolllm_account_circuit_transitions_total{")
}

// server.disable_account_breaker restores plain per-request fallback.
func TestAccountBreakerKillSwitchRestoresPlainFallback(t *testing.T) {
	doom := newFakeProvider(t, "DOOM", doomKey)
	newFakeProvider(t, "ZAI", zaiKey)
	doom.script("a", quotaExhausted)
	doom.script("b", answers("same account"))
	router := newBreakerRouter(t, map[string]string{"bot": "doom/a,doom/b,zai/c"}, true)

	for _, stream := range []bool{false, true} {
		status, model, _ := chatText(t, router, "bot", stream)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, "doom/b", model)
	}
	assert.Equal(t, 2, doom.callsTo("a"))
	assert.Equal(t, 2, doom.callsTo("b"))
	assert.NotContains(t, scrapeMetrics(t, router), "smolllm_account_circuit")
}
