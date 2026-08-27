# Domain context

- **Ledger** — Process-local, token-only aggregation of every LLM request attempt. Records successes, failures, reported or estimated token usage; resets on restart.
- **Stats bucket** — Ledger row keyed by UTC day × requested alias (or raw model string) × served provider/model.
- **Reload** — Redeploy of the service process: rebuild, restart, and accept only on a passing health probe. Required for binary or bind changes; resets the Ledger.
- **Config hot-reload** — In-process adoption of config.yaml edits (aliases, keys, log level) with no restart; a Reload is never needed for these.
- **Pass-through field** — A client request field the server forwards to the provider verbatim without modeling it (`tools`, `tool_choice`, `parallel_tool_calls`, `response_format`); provider errors about it surface unchanged. Any other field the server does not model is not forwarded.
