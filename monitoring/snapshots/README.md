# Deployed snapshots

What is running on `rage`, committed so a reader can diff intent against
reality without an SSH session. Copies, not sources: the live files are edited
on rage, so a mismatch is expected the moment someone changes one there.

Check before trusting a snapshot:

```bash
ssh rage 'md5sum /data/monitoring/prometheus/rules-smolllm.yml \
                  /data/monitoring/grafana-data/dashboards/smolllm.json'
md5 -q monitoring/snapshots/rules-smolllm.yml
md5 -q monitoring/snapshots/smolllm-dashboard.json
```

| Snapshot | Live location on rage | Deploy |
|---|---|---|
| `rules-smolllm.yml` | `/data/monitoring/prometheus/rules-smolllm.yml`, mounted at `/etc/prometheus/` in the `prometheus` container | copy, then `promtool check rules`; behaviour is pinned by `../smolllm_rules_test.yml` |
| `smolllm-dashboard.json` | `/data/monitoring/grafana-data/dashboards/smolllm.json` | copy; the `local` file provider polls every 60s. Owner must be `root:root` or the provider ignores it silently |

Write both as `root:root` 644. `/var/lib/grafana` is a bind mount, so
`docker cp` into the container lands in the overlay and the provisioner never
sees it — write the host path.

## Why the alert pins `streaming="true"`

Both the p90 and its volume gate select `streaming="true"` instead of
aggregating. Streaming TTFT is time to first byte; blocking TTFT is the whole
call (`ask.go` pins `ttft = total`), so an average describes neither. Prometheus
also still holds unlabelled series from before the label existed.

On this host the label only ever takes `true`: the server never sets
`WithDisableStreaming`, so `stream:false` streams upstream and buffers. See
`../WITH_DISABLE_STREAMING_ASSESSMENT.md`.
