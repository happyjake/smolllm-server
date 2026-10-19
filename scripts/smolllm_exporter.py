#!/usr/bin/env python3
"""smolllm-server Prometheus exporter (:9104) — scrapes the local :11435 /metrics.

Why an exporter instead of scraping smolllm-server directly:
  1. smolllm-server binds 127.0.0.1 only, and that is deliberate. Widening the
     bind would expose an authenticated LLM proxy on the office LAN to reach a
     monitoring stack. This process is the reach, not a border move.
  2. /metrics requires the access key. Prometheus should not hold it: the key
     also spends credits. It is read here at startup from the config and never
     logged.

Semantics — the part that decides whether the graphs mean anything:
  smolllm-server's counters live in RAM and restart at zero on every deploy.
  Exported verbatim as TYPE counter, Prometheus' reset detection would read a
  deploy as "half your traffic vanished" — increase() over a window spanning
  the restart silently returns only the post-restart part. So this exporter
  keeps its own baseline and serves:
    - smolllm_attempts_by_outcome_total   TYPE counter, monotonic ACROSS restarts
    - smolllm_first_output_seconds_*      histogram, converted from cumulative
                                          buckets to a restart-safe form too
  The cost, stated plainly: if this exporter restarts, its baseline goes too,
  undoing the protection for that one window. Deploys of smolllm-server are
  frequent, deploys of this file are not — that is the trade.

py3.6 stdlib only (rage is 3.6.8; this host is python3 too).
"""
import base64
import http.server
import json
import os
import re
import sys
import threading
import time
import urllib.request

# LISTEN: 0.0.0.0 so the office Prometheus (rage) can scrape directly. This is
# the same posture node_exporter :9100 already takes on these hosts. What makes
# it acceptable here is that the exporter never serves the access key or any
# credential: it serves counters that were already fetched from behind the key.
# If that ever changes, bind back to 127.0.0.1 and give Prometheus an ssh probe.
LISTEN = (os.environ.get("SMOLLLM_EXPORTER_BIND", "0.0.0.0"),
          int(os.environ.get("SMOLLLM_EXPORTER_PORT", "9104")))

# Remote mode: pull the payload over ssh instead of http. Used on rage to reach
# doom, whose exporter listens on loopback only (doom faces the internet, so a
# LAN-bound metrics port is not acceptable there). Reuses the read-only probe
# key the tgs exporter already uses -- no new credential, no new open port.
SSH_CMD = os.environ.get("SMOLLLM_EXPORTER_SSH", "")
UPSTREAM = os.environ.get("SMOLLLM_METRICS_URL", "http://127.0.0.1:11435/metrics")
CONFIG = os.path.expanduser("~/.config/smolllm-server/config.yaml")
INTERVAL = float(os.environ.get("SMOLLLM_EXPORTER_INTERVAL", "30"))
TIMEOUT = 10.0

# Restart-proof state: for each series key, the highest raw value seen so far
# and the accumulated value from before the last reset.
_lock = threading.Lock()
_baseline = {}   # key -> float, added to every raw value (survives upstream resets)
_last_raw = {}   # key -> float, the largest raw value seen for this incarnation
_payload = "# smolllm exporter warming up\n"
_scrape_ok = 0
_scrape_fail = 0
_last_error = ""


def read_access_key():
    """Pull the key out of the YAML by hand.

    PyYAML is not installed on these hosts and pulling a wheel in for one field
    is not worth it; the file has a single `access_key:` line under `server:`.
    """
    try:
        with open(CONFIG) as fh:
            for line in fh:
                m = re.match(r"\s*access_key:\s*(\S+)\s*$", line)
                if m:
                    return m.group(1).strip("'\"")
    except OSError as exc:
        print("warn: cannot read %s: %s" % (CONFIG, exc), file=sys.stderr)
    return ""


def fetch(key):
    if SSH_CMD:
        # The remote side runs a plain curl against its own loopback, so the
        # access key never leaves that host's config file.
        import subprocess
        out = subprocess.run(
            SSH_CMD.split() + ["curl -s -m 8 %s" % UPSTREAM],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=TIMEOUT + 10,
        )
        if out.returncode != 0:
            raise RuntimeError("ssh probe failed (%d): %s" % (
                out.returncode, out.stderr.decode("utf-8", "replace").strip()[:200]))
        body = out.stdout.decode("utf-8", "replace")
        if not body.strip():
            raise RuntimeError("ssh probe returned an empty body")
        return body
    req = urllib.request.Request(UPSTREAM)
    if key:
        req.add_header("Authorization", "Bearer " + key)
    with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
        return resp.read().decode("utf-8", "replace")


_COUNTER_RE = re.compile(r"^(smolllm_attempts_total)\{(.*)\}\s+(\d+(?:\.\d+)?)$")
_HIST_RE = re.compile(r"^(smolllm_attempt_first_output_seconds)_(bucket|sum|count)\{(.*)\}\s+(\d+(?:\.\d+)?)$")


def restart_safe_counter(key, raw):
    """Turn a raw counter into a monotonic one.

    A drop means the upstream restarted: keep everything counted before it by
    folding the last raw value into the baseline, then continue from zero.
    """
    prev_raw = _last_raw.get(key)
    if prev_raw is not None and raw < prev_raw:
        _baseline[key] = _baseline.get(key, 0.0) + prev_raw
    _last_raw[key] = raw
    return _baseline.get(key, 0.0) + raw


def transform(body):
    out = []
    # Cumulative-bucket bookkeeping per histogram series, so a restart resets
    # the whole histogram at once instead of producing a mixed-bucket artefact.
    hist_prev = {}
    in_hist = False
    for line in body.splitlines():
        if line.startswith("# TYPE smolllm_attempt_first_output_seconds histogram"):
            out.append("# TYPE smolllm_attempt_first_output_seconds histogram")
            in_hist = True
            continue
        if line.startswith("# TYPE smolllm_attempts_total counter"):
            out.append(line)
            continue
        if line.startswith("#"):
            # Drop the upstream HELP for rewritten counters: it would describe
            # the raw values, which is no longer what is served.
            if "smolllm_attempts_total" in line:
                continue
            if in_hist and line.startswith("# HELP"):
                out.append(line)
                continue
            out.append(line)
            continue

        m = _COUNTER_RE.match(line)
        if m:
            key = "attempt|" + m.group(2)
            out.append("%s{%s} %s" % (m.group(1), m.group(2),
                                      _fmt(restart_safe_counter(key, float(m.group(3))))))
            continue

        h = _HIST_RE.match(line)
        if h:
            name, kind, labels, val = h.groups()
            series = name + "{" + labels + "}"
            if kind == "bucket":
                # Keep the monotonicity of the cumulative buckets across a
                # restart: each le's cumulative count is itself restart-safe.
                key = "hist|" + series + "|" + labels
                out.append("%s_bucket{%s} %s" % (name, labels,
                                                _fmt(restart_safe_counter(key, float(val)))))
            elif kind == "sum":
                key = "sum|" + series
                out.append("%s_sum{%s} %s" % (name, labels,
                                              _fmt(restart_safe_counter(key, float(val)))))
            else:
                key = "count|" + series
                out.append("%s_count{%s} %s" % (name, labels,
                                                _fmt(restart_safe_counter(key, float(val)))))
            hist_prev[labels] = val
            continue

        out.append(line)
    return "\n".join(out) + "\n"


def _fmt(v):
    if v == int(v):
        return str(int(v))
    return repr(round(v, 6))


def poll_loop():
    global _payload, _scrape_ok, _scrape_fail, _last_error
    key = "" if SSH_CMD else read_access_key()
    if not key and not SSH_CMD:
        print("warn: no access_key found in %s; /metrics will 401" % CONFIG,
              file=sys.stderr)
    while True:
        try:
            body = fetch(key)
            with _lock:
                payload = transform(body)
                payload += (
                    "# HELP smolllm_exporter_scrape_ok Whether the last upstream scrape succeeded.\n"
                    "# TYPE smolllm_exporter_scrape_ok gauge\n"
                    "smolllm_exporter_scrape_ok 1\n"
                    "# HELP smolllm_exporter_upstream_age_seconds Seconds since the last successful scrape of smolllm-server.\n"
                    "# TYPE smolllm_exporter_upstream_age_seconds gauge\n"
                    "smolllm_exporter_upstream_age_seconds 0\n"
                )
                _payload = payload
                _scrape_ok += 1
                _last_error = ""
        except Exception as exc:  # noqa: BLE001 - a monitoring process must not die
            with _lock:
                _scrape_fail += 1
                _last_error = str(exc)
                # Serve the last good payload plus a down signal rather than
                # nothing: an empty response makes the target look UP with no
                # series, which hides the outage behind a green "up".
                _payload = (
                    "# HELP smolllm_exporter_scrape_ok Whether the last upstream scrape succeeded.\n"
                    "# TYPE smolllm_exporter_scrape_ok gauge\n"
                    "smolllm_exporter_scrape_ok 0\n"
                    "# HELP smolllm_exporter_scrape_failures_total Failed upstream scrapes since exporter start.\n"
                    "# TYPE smolllm_exporter_scrape_failures_total counter\n"
                    "smolllm_exporter_scrape_failures_total %d\n" % _scrape_fail
                )
            print("warn: scrape failed: %s" % exc, file=sys.stderr)
        time.sleep(INTERVAL)


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.split("?")[0] not in ("/metrics", "/"):
            self.send_error(404)
            return
        with _lock:
            body = _payload.encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass  # Prometheus scrapes every 30s; do not fill the journal


def main():
    threading.Thread(target=poll_loop, daemon=True).start()
    srv = http.server.HTTPServer(LISTEN, Handler)
    print("smolllm exporter serving on %s:%d -> %s" % (LISTEN[0], LISTEN[1], UPSTREAM))
    srv.serve_forever()


if __name__ == "__main__":
    main()
