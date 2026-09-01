# smolllm-server

OpenAI-compatible HTTP front-end for [smolllm-go](../smolllm-go). Routes requests
across 41 providers via short alias names, runs locally on macOS under launchd.

## Layout

```
cmd/server/        entrypoint
internal/config/   YAML loader, env-file loader, alias resolution
internal/auth/     bearer token middleware
internal/llm/      OpenAI ↔ smolllm adapter (request build + response shapes)
internal/server/   HTTP wiring: chat / embeddings / models / health
internal/apierr/   OpenAI-style error envelope
launch/            LaunchAgent plist + install/reload/uninstall script
```

`go.mod` uses `replace github.com/rocry/smolllm-go => ../smolllm-go`, so local
edits to the library propagate without publishing.

## Configuration

Default config path: `~/.config/smolllm-server/config.yaml`. Override with
`--config <path>` or `$SMOLLLM_SERVER_CONFIG`.

```yaml
server:
  bind: 0.0.0.0:11435
  access_key: change-me       # REQUIRED. SMOLLLM_SERVER_ACCESS_KEY env wins if set
  env_file: ~/.env.smolllm    # provider keys: ${PROVIDER}_API_KEY etc.
  log_level: info

aliases:
  fast: cerebras/qwen-3-235b-a22b-instruct-2507,groq/qwen/qwen3-32b!none,gemini/gemini-flash-latest
  translator: ollama/frob/hy-mt1.5:latest,gemini/gemini-flash-lite-latest
```

Aliases pass straight through to smolllm-go's `WithModel("a,b,c")`, which tries
each in order and falls back on error.

## Install / run

```bash
just install      # build, seed config, render plist, bootstrap agent
just reload       # rebuild + kickstart (full restart; needed for bind changes)
just reinstall    # bootout + re-render plist + bootstrap (needed after plist edits)
just uninstall    # bootout + remove plist (binary & config preserved)
just logs         # tail ~/Library/Logs/personal.smolllm-server.log
```

The LaunchAgent plist is **rendered per machine** from
`launch/personal.smolllm-server.plist.template`, substituting `$HOME`, `$USER`,
the binary path, the repo path, and the config path. Nothing in this repo
hardcodes a username. The rendered file is written to
`~/Library/LaunchAgents/` as a real file rather than a symlink, so the loaded
job does not break when the repo moves; a legacy symlink left by an older
install is replaced automatically.

### Hosts without a Go toolchain

Cross-compile elsewhere and skip the build step — the service scripts then use
whatever binary is already at `~/.local/bin/smolllm-server`:

```bash
# on a machine with Go, same GOOS/GOARCH as the target
GOOS=darwin GOARCH=arm64 go build -o smolllm-server ./cmd/server
scp smolllm-server target:~/.local/bin/

# on the target (no Go, no just needed — service.sh is plain bash)
SMOLLLM_SKIP_BUILD=1 bash launch/service.sh install
```

`SMOLLLM_SKIP_BUILD=1` fails loudly if no executable is present, so it can
never bootstrap a job pointing at a missing binary.

### Hot reload

Just edit the YAML — the server picks it up automatically (~200 ms debounce
via fsnotify). Hot-reloadable: `aliases`, `server.access_key`,
`server.log_level`, and `server.env_file` contents (re-sourced with overwrite
so rotated provider keys take effect). Invalid YAML is rejected and the
previous snapshot is retained.

For `server.bind` changes, or to force a clean restart, run `just reload`.

The shipped example binds `127.0.0.1:11435`. The access key is the only gate
in front of every provider key on the box, so widen the bind to `0.0.0.0`
deliberately, not by inheriting a default. The agent reads `~/.env.smolllm`
itself on startup — no wrapper script.

The health probe used by `install`/`reload` reads `server.bind` out of the live
config (a wildcard bind is probed over loopback), so a non-default port needs no
script edit. `SMOLLLM_HEALTH_URL` overrides it outright.

## Endpoints

| | |
|---|---|
| `GET /healthz`           | Public liveness probe. |
| `GET /stats`             | In-memory per-attempt token usage for the last ~31 UTC days. Auth required. |
| `GET /v1/models`         | Lists configured aliases. Auth required. |
| `POST /v1/chat/completions` | OpenAI Chat Completions. Auth required. Streaming via `stream: true`. |
| `POST /v1/embeddings`    | OpenAI Embeddings. Auth required. |

Auth: `Authorization: Bearer <access_key>` (the bare `<access_key>` form is also
accepted for clients that omit `Bearer`).

### Examples

```bash
# Health (no auth)
curl -fsS http://127.0.0.1:11435/healthz

# Set your access key once (matches server.access_key in config.yaml)
export ACCESS_KEY=your-access-key

# Models
curl -fsS http://127.0.0.1:11435/v1/models -H "Authorization: Bearer $ACCESS_KEY"

# Non-streaming chat against an alias
curl -fsS http://127.0.0.1:11435/v1/chat/completions \
  -H "Authorization: Bearer $ACCESS_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"fast","messages":[{"role":"user","content":"say hi in 3 words"}]}'

# Streaming
curl -N http://127.0.0.1:11435/v1/chat/completions \
  -H "Authorization: Bearer $ACCESS_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"translator","stream":true,"messages":[{"role":"user","content":"translate: hello"}]}'

# Direct provider/model also works (bypasses aliases)
curl -fsS http://127.0.0.1:11435/v1/chat/completions \
  -H "Authorization: Bearer $ACCESS_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"gemini/gemini-flash-latest","messages":[{"role":"user","content":"hi"}]}'

# Embeddings
curl -fsS http://127.0.0.1:11435/v1/embeddings \
  -H "Authorization: Bearer $ACCESS_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"ollama/qwen3-embedding:0.6b","input":["hi","there"]}'
```

### Deploying to a second machine

The whole deploy is four files and one command. Nothing needs a Go toolchain on
the target if the binary is built elsewhere for the same `GOOS/GOARCH`.

```bash
# --- on a machine with Go ---
git clone https://github.com/happyjake/smolllm-server.git
git clone https://github.com/RoCry/smolllm-go.git   # sibling: go.mod has a replace directive
cd smolllm-server
GOOS=darwin GOARCH=arm64 go build -o /tmp/smolllm-server ./cmd/server
scp /tmp/smolllm-server TARGET:~/.local/bin/

# --- on the target ---
git clone https://github.com/happyjake/smolllm-server.git ~/w/smolllm-server
mkdir -p ~/.config/smolllm-server && chmod 700 ~/.config/smolllm-server

umask 077
cat > ~/.env.smolllm <<'EOF'
OPENROUTER_API_KEY=...
ZAI_API_KEY=...
ZAI_BASE_URL=https://api.z.ai/api/coding/paas/v4
EOF

cat > ~/.config/smolllm-server/config.yaml <<EOF
server:
  bind: 127.0.0.1:11435
  access_key: $(openssl rand -hex 24)
  env_file: ~/.env.smolllm
  log_level: info
aliases:
  fast: zai/glm-5.3-flash,openrouter/z-ai/glm-5.3-flash
  complex: zai/glm-5.3,zai/glm-5.2,openrouter/z-ai/glm-5.3
  vision: zai/glm-5.3-flash,openrouter/google/gemini-2.5-flash-lite
  agent: zai/glm-5.3,openrouter/z-ai/glm-5.3-flash
EOF

cd ~/w/smolllm-server && SMOLLLM_SKIP_BUILD=1 bash launch/service.sh install
```

`just` is optional — every recipe is a thin wrapper over `launch/service.sh`,
which is plain bash.

### Choosing alias chains

Two properties decide the order of a chain, and neither is model quality:

- **Marginal cost.** A flat-rate subscription endpoint (a coding plan, a local
  model) costs nothing per call, so it belongs first on every chain. A metered
  key belongs behind it, catching only what the first leg fails.
- **Whether the leg can do the job at all.** A chain falls through on *errors*.
  A leg that accepts a request and answers it badly — ignoring `tools`, ignoring
  an image — never triggers fallback, so it silently degrades the alias.

That second point is why `vision` and `agent` cannot simply reuse the `fast`
chain: every leg must be independently confirmed to honour images and tool
calls, because a leg that quietly ignores them is indistinguishable from one
that handled them.

**Providers not in the built-in table** need no code change — give the provider
segment any name and supply `${NAME}_BASE_URL` and `${NAME}_API_KEY`. A base URL
already ending in a version segment (`/v4`) is used as-is; otherwise `/v1` is
appended. A trailing `/` appends the endpoint directly, and a trailing `#` means
"use this URL literally".

**Verifying a chain end to end** — the alias is only as good as its worst
reachable leg, so probe each leg by its raw `provider/model` string, which
bypasses alias fallback and shows you which leg actually answered:

```bash
K=your-access-key
# does this leg honour tools, or does it answer in prose?
curl -s http://127.0.0.1:11435/v1/chat/completions \
  -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
  -d '{"model":"zai/glm-5.3","messages":[{"role":"user","content":"What is the weather in Paris?"}],
       "tools":[{"type":"function","function":{"name":"get_weather",
                 "parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}' \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); c=d["choices"][0]; print(c["finish_reason"], c["message"].get("tool_calls"))'
# want: tool_calls [...]      prose + "stop" means this leg ignored the tools

# does this leg accept an image?
curl -s http://127.0.0.1:11435/v1/chat/completions \
  -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
  -d '{"model":"zai/glm-5.3-flash","messages":[{"role":"user","content":[
        {"type":"text","text":"Describe this image in one word."},
        {"type":"image_url","image_url":{"url":"data:image/png;base64,PUT_A_REAL_PNG_HERE"}}]}]}'
```

Client-supplied `image_url` parts reach the provider verbatim — the server
models neither images nor tools, so what a leg does with them is entirely the
provider's behaviour, and only a live probe tells you which.

**A metered key with no spend cap will happily bill a runaway agent loop.** If a
chain has an OpenRouter leg, set a credit limit on the key itself; the server has
no budget enforcement and the ledger at `/stats` is after-the-fact and resets on
restart.

### Pointing tools at it

- **Cursor / Open WebUI / Aider**: set base URL to `http://127.0.0.1:11435/v1`,
  API key to your access key, and pick `fast`, `translator`, or any
  `provider/model` string.

## Tool calling

`tools`, `tool_choice`, `parallel_tool_calls` and `response_format` are
pass-through fields: forwarded to the provider verbatim, never modeled here, so
a provider error about one surfaces unchanged. Assistant messages carrying
`tool_calls` and `tool` role messages replay as sent.

Streaming emits the assembled `tool_calls` in one delta frame just before the
terminal frame — the library exposes complete calls only, so partial argument
JSON never reaches a client. Mainstream OpenAI clients reassemble it fine.

Two things worth knowing before pointing an agent at an alias:

- **`finish_reason` is OpenAI-shaped here.** A turn carrying tool calls always
  reports `tool_calls`, even when the provider said `stop` (Gemini does). The
  libraries still surface the provider's string verbatim; only this
  OpenAI-compatible surface normalizes it, so agent loops that branch on the
  field work unchanged.
- **Not every leg supports tools.** A leg that rejects them 400s and the chain
  advances; a leg that *ignores* them answers in prose, which no agent loop can
  detect. Use the `agent` alias, whose legs are all verified tool-capable.

Live probe, 2026-08-27 (`~/.env.smolllm` credentials, both modes):

| leg | tools |
|---|---|
| `deepseek/deepseek-v4-flash`, `groq/openai/gpt-oss-120b`, `omlx/Qwen3.8-27B-*`, `jake/glm` | honoured |
| `gemini/gemini-3.5-flash-lite`, `gemini/gemini-flash-latest` | honoured (streams `finish_reason: stop`) |
| `groq/groq/compound`, `groq/groq/compound-mini`, `ollama/frob/hy-mt1.5` | 400, chain advances |
| `codeagentlayer/antigravity/*` | **silently ignored** — answers in prose |

## Not yet supported

`functions` (the legacy function-calling API, superseded by `tools`), `n>1`,
and `/v1/completions` (legacy text completion). Requests using these get a 400.

## Development

```bash
just test    # go test ./... -race
just vet
just build   # writes binary to ~/.local/bin/smolllm-server
```

The chat handler test (`internal/server/chat_test.go`) spins up an
`httptest.Server` posing as an OpenAI-style provider, points smolllm-go at it
via `MOCK_BASE_URL` / `MOCK_API_KEY`, and exercises both streaming and
non-streaming end-to-end without touching the network.
