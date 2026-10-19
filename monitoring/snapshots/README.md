# Deployed snapshots

Copies of what is actually running on `rage`, committed so a reader can diff
intent against reality without an SSH session. These are **snapshots, not
sources** — the live files are edited in place on rage, and a mismatch here is
expected the moment someone changes one there without refreshing these.

Check whether a snapshot is stale before trusting it:

```bash
ssh rage 'md5sum /data/monitoring/prometheus/rules-smolllm.yml \
                  /data/monitoring/grafana-data/dashboards/smolllm.json'
md5 -q monitoring/snapshots/rules-smolllm.yml
md5 -q monitoring/snapshots/smolllm-dashboard.json
```

## Files

| Snapshot | Live location on rage | Notes |
|---|---|---|
| `rules-smolllm.yml` | `/data/monitoring/prometheus/rules-smolllm.yml` (mounted at `/etc/prometheus/` in the `prometheus` container) | Three alert rules. Validate with `promtool check rules`; behaviour is pinned by `../smolllm_rules_test.yml`. |
| `smolllm-dashboard.json` | `/data/monitoring/grafana-data/dashboards/smolllm.json` | Provisioned by the `local` file provider (`grafana/provisioning/dashboards/local.yml`, 60s poll). Panels deliberately leave `datasource` unset and inherit the default — a hardcoded uid silently yields "No data". |

`smolllm-dashboard.json` is also the same content as `../grafana/smolllm.json`,
which is the file that gets copied to the host. They are duplicated rather than
symlinked because the provisioner needs a plain file and the snapshot is meant
to stay readable on its own if the two ever diverge.

## Why the alert pins `streaming="true"`

Both the p90 and its volume gate select `streaming="true"` rather than
aggregating over the label. A streaming attempt's TTFT is time to first byte; a
blocking attempt's TTFT is its entire duration (`ask.go` pins `ttft = total`).
Averaging them would produce a number describing neither population. Prometheus
also still holds unlabelled series from before the label existed, and those
would otherwise join the average.

Note that on this host the label only ever takes the value `true`: the server
never sets `WithDisableStreaming`, so `stream:false` is served by streaming
upstream and buffering. See `../WITH_DISABLE_STREAMING_ASSESSMENT.md`.
