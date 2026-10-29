# smolllm-server

OpenAI-compatible HTTP front-end for [smolllm-go](../smolllm-go). Routes requests
across 41 providers via short alias names. Run locally on macOS under launchd
or on Linux under systemd; the router does not require a GPU.

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

### Fallback semantics (what counts as "down")

- **429** falls through to the next leg immediately (no retry — the quota is
  exhausted, waiting helps nobody).
- **500/502/503/529**, and dead-on-arrival empty responses, get up to
  **3 total attempts** on the same model (1s/2s between attempts), then advance.
- Any other error (400s, network, timeout) advances the chain at once.
- A **hung** leg is bounded by `server.request_timeout` (per attempt; the
  client `timeout` field overrides it) — set this, or a dead-but-silent
  provider pins requests for the 600s library default before switching.
- An empty answer, a turn truncated mid-tool-call, and reasoning that ate the
  whole output budget all fail the leg too — the chain routes around them.
- Once streaming content or reasoning has reached the client, a later failure
  is surfaced rather than silently switching models and replaying output.
  Failover is safe before first usable output; tool-only streams are gated
  until their complete tool calls arrive.
- **Account quota exhaustion is shared across requests.** A 429 whose JSON
  body carries business code `14019` (the doom/aitoken account-quota code)
  marks that provider *credential* exhausted: later legs on the same account
  in the request are skipped, and one process-wide account breaker makes
  every chat request skip it until a cooldown (5 min, doubling to 1 h). After
  the cooldown one real request probes it; success closes it. If every leg is
  skipped or fails, the open accounts are tried anyway (fail-open). Any other
  429, including other structured codes, stays model-local. Embeddings do not
  use the breaker. `server.disable_account_breaker: true` (hot-reloads)
  restores plain per-request fallback.
- There is no separate first-byte timeout: `request_timeout` bounds the
  complete upstream attempt.
- What never triggers fallback: a leg that answers *plausibly but badly*
  (ignoring `tools`, dropping an image). Probe every leg before trusting it
  (see "Choosing alias chains").

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

### Build provenance without deployment

`python3 launch/build.py --output /tmp/smolllm-artifact` (Python 3.12+, Go)
archives two clean committed repositories and stamps their full revisions. The
library defaults to sibling `../smolllm-go`; `--library <root>` selects another
clean checkout. `--goos` / `--goarch` support cross-compilation. The script writes
only the new staging artifact and its private `.build.json` receipt, rejecting
existing output, source-root output and the standard live binary path. It never
installs or restarts anything. Run `just test-build` to test this helper offline.

Authenticated `/version` reports the running executable's revisions, source-clean
flag, platform, PID and per-process instance ID. Ordinary `go build` (including
legacy `just reload`) reports unverified/unknown provenance; it remains usable but
cannot satisfy a managed fleet release gate. Darwin/Linux artifact hashes differ;
compare the source revision pair and verify each artifact against its own receipt.
A receipt is build evidence, not proof that a running host loaded that artifact.
Build stamps are operational metadata, not signed supply-chain attestations.

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
| `GET /version`           | Running build/process identity (server + local library revisions). Auth required. |
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

### The deploy script needs ssh — a bare Mac is not reachable

`scripts/deploy-remote.sh` drives the target over ssh. Linux hosts (systemd)
work as documented below. A Mac does **not** by default: macOS ships with Remote
Login off, so `ssh $(hostname -s)` is refused and the script exits at step 1 with
`cannot reach ... over ssh`. Two supported routes:

```bash
# a) on the Mac itself — no ssh involved
./scripts/install-exporter-macos.sh reinstall   # metrics exporter (:9104)
./launch/service.sh reinstall                   # the server

# b) from elsewhere — enable System Settings > General > Sharing > Remote Login,
#    then run the normal deploy with the Mac's ssh Host
scripts/deploy-remote.sh <mac-ssh-host>
```

The launchd branch of the script calls `launchctl kickstart -k`, which kills the
running server immediately — in-flight streaming requests are cut. Deploy to a
laptop when you are not mid-request. If no `personal.smolllm-server` agent is
installed under `gui/<uid>`, the script installs the binary and restarts
nothing; read its final line rather than assuming success.

### Metrics

Both hosts expose Prometheus metrics through a small exporter
(`scripts/smolllm_exporter.py`, port 9104). Prometheus does **not** scrape
`:11435` directly: that port is loopback-only by design and needs a Bearer key
that also spends credits, so the exporter holds the key instead and re-serves
the numbers.

| metric | meaning |
| --- | --- |
| `smolllm_attempts_total{alias,provider,model,outcome,phase}` | outcome = `ok` / `first_output_timeout` / `error` / `canceled` |
| `smolllm_attempt_first_output_seconds` | time to first token, successful attempts only |
| `smolllm_account_circuit_transitions_total{provider,account,transition}` | `opened` / `closed` account circuits (`account` = 12-hex credential fingerprint, never the key) |
| `smolllm_account_circuit_skips_total{alias,provider,account,model,reason}` | legs skipped with no upstream call; reason = `circuit_open` / `exhausted_in_request` |
| `smolllm_account_circuit_probes_total{alias,provider,account,model}` | half-open probes after a cooldown |
| `smolllm_account_circuit_fail_open_total{alias,provider,account,model}` | open-account legs tried because nothing else succeeded |
| `smolllm_account_circuit_state{provider,account,state}` | one-hot `closed` / `open` / `half_open` per account seen since start |
| `smolllm_unrecognized_structured_429_total{alias,provider,account,model}` | JSON-bodied 429s without a known account code (model-local) |
| `smolllm_exporter_scrape_ok` | 1 if the last upstream scrape worked |

`first_output_timeout` is the signal that separates "the model is sick" from "the
proxy queued us and said nothing": a leg that never produced a first token is
counted here, not as an error, so it never gets demoted out of an alias chain for
something the transport did.

The exporter absorbs the counter resets that every deploy causes (the server's
counters live in RAM). Served raw, a deploy reads as a traffic collapse and
`increase()` over a window spanning it silently returns only the post-restart
half.

**`phase` semantics:** exactly two values, `headers` (no response headers) and
`stream` (headers arrived, no first token). A non-streaming request that stalls
reports `stream` too — one phenomenon, one label.

### Linux / systemd user service

Deploy with `scripts/deploy-remote.sh <ssh-host>` instead of copying a binary by
hand. It asks the target what it is, cross-compiles for that platform, rejects
an artifact whose `file` description does not match, proves the binary serves
`/healthz` on a spare port **before** replacing anything, and only then stops
the service to swap. A macOS binary copied to the Linux host fails with
`status=203/EXEC` and restart-loops; the script turns that into a build-time
error. Use `--build-only` or `--dry-run` to inspect without deploying.

Building by hand, for reference — build the binary for the target
architecture on a host with Go. Keep
`smolllm-go` beside the server checkout, as required by `go.mod`:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/smolllm-server-linux ./cmd/server
scp /tmp/smolllm-server-linux TARGET:/tmp/smolllm-server-linux
scp launch/smolllm-server.service TARGET:/tmp/smolllm-server.service
```

On the target, first create the configuration and provider environment file
following the configuration section. Bind to `127.0.0.1:11435`, generate a
**new local access key**, restrict configuration/credential files to mode
`0600`, and include only providers actually used by that host. Provider base
URLs and network access can differ between hosts; probe from the target
before copying an alias chain. Set a per-attempt `request_timeout` appropriate
for the consuming agent's total task deadline. A fallback list alone does not
bound how long a hung provider takes to fail.

```bash
mkdir -p ~/.local/bin ~/.config/systemd/user
install -m 755 /tmp/smolllm-server-linux ~/.local/bin/smolllm-server
install -m 644 /tmp/smolllm-server.service ~/.config/systemd/user/smolllm-server.service
systemctl --user daemon-reload
systemctl --user enable --now smolllm-server.service
curl --fail --max-time 5 http://127.0.0.1:11435/healthz
journalctl --user -u smolllm-server.service
```

For an unattended server, have its administrator enable user lingering
(`loginctl enable-linger "$USER"`) so the user service survives logout and
starts at boot. The supplied unit restarts a crashed process; it is not a
model-availability probe. Monitor both the router and real requests separately.

Connect Pi using a custom `smolllm` provider in `~/.pi/agent/models.json`:
`baseUrl: http://127.0.0.1:11435/v1`, `api: openai-completions`, the local
access key, and an `agent` model. The bot then selects `--model smolllm/agent`.
Keep tools, sessions and bot identity local to each bot host. `agent` is an
alias understood by the router, not an instruction to execute tasks elsewhere.
Verify streaming/non-streaming tools, tool-result replay and forced fallback
before changing the bot's active configuration; do not infer success from
`/healthz` alone.

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
