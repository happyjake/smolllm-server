# Agent guidance

Repository architecture and checks: [README.md](README.md), `just --list`.

**Dual-host maintenance:** runtime changes consumed by Pio/Jake-doom require the
`pi-proxy-ops` skill's maintenance runbook and running-version/tool gate before
rollout completion. Code landing alone is not deployment; obtain separate approval
for live service changes.
