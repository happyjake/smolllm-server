#!/usr/bin/env bash
set -euo pipefail

# Install/remove the smolllm-server metrics exporter as a LaunchAgent.
#
# Kept out of launch/service.sh: that script owns the server lifecycle (build,
# config seeding, health wait) and bolting a second, unrelated unit onto it
# would make `just reinstall` mean two different things. This one installs a
# scraper that never builds anything.
#
# macOS only. Linux hosts use scripts/deploy-exporter-remote.sh + the systemd
# unit in launch/smolllm-exporter.service.

readonly SESSION="gui/$(id -u)"
readonly LABEL="personal.smolllm-exporter"
readonly PLIST="${LABEL}.plist"
readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEMPLATE="${REPO_ROOT}/launch/${PLIST}.template"
readonly EXPORTER_PATH="${REPO_ROOT}/scripts/smolllm_exporter.py"
readonly TARGET="${HOME}/Library/LaunchAgents/${PLIST}"
readonly LOG_FILE="${HOME}/Library/Logs/${LABEL}.log"
readonly RUN_USER="${USER:-$(id -un)}"

usage() {
    cat >&2 <<EOF
Usage: $0 {install|reinstall|stop|status|logs}

  install     Render the plist and bootstrap the agent (idempotent).
  reinstall   Bootout, re-render, bootstrap. Use after editing the template.
  stop        Bootout the agent.
  status      Show launchd state and whether :9104 answers.
  logs        Tail the exporter log.
EOF
    exit 64
}

python_bin() {
    # Prefer a real interpreter path: launchd has no shell to resolve one, and
    # /usr/bin/python3 on macOS is a stub that prompts for Command Line Tools.
    local candidate
    for candidate in "${HOME}/.local/bin/python3" /opt/homebrew/bin/python3 /usr/local/bin/python3; do
        if [[ -x "${candidate}" ]]; then
            printf '%s' "${candidate}"
            return 0
        fi
    done
    echo "error: no python3 found (looked in ~/.local/bin, /opt/homebrew/bin, /usr/local/bin)" >&2
    return 1
}

render() {
    local py
    py="$(python_bin)"
    [[ -f "${TEMPLATE}" ]] || { echo "error: missing template ${TEMPLATE}" >&2; exit 1; }
    mkdir -p "$(dirname "${TARGET}")" "$(dirname "${LOG_FILE}")"
    sed -e "s|@PYTHON@|${py}|g" \
        -e "s|@EXPORTER_PATH@|${EXPORTER_PATH}|g" \
        -e "s|@WORKING_DIR@|${REPO_ROOT}|g" \
        -e "s|@HOME@|${HOME}|g" \
        -e "s|@USER@|${RUN_USER}|g" \
        -e "s|@LOG_FILE@|${LOG_FILE}|g" \
        "${TEMPLATE}" >"${TARGET}"
    echo "rendered ${TARGET} (python: ${py})"
}

bootstrap() {
    launchctl bootstrap "${SESSION}" "${TARGET}" 2>/dev/null ||
        launchctl kickstart -k "${SESSION}/${LABEL}"
    echo "bootstrapped ${LABEL}"
}

bootout() {
    launchctl bootout "${SESSION}/${LABEL}" 2>/dev/null || true
}

case "${1:-}" in
install)
    render
    bootstrap
    sleep 2
    http_code="$(curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:9104/metrics || true)"
    echo "exporter /metrics -> HTTP ${http_code}"
    [[ "${http_code}" == "200" ]] || { echo "warning: exporter did not answer 200 yet; check $0 logs" >&2; exit 1; }
    ;;
reinstall)
    bootout
    render
    bootstrap
    ;;
stop)
    bootout
    echo "stopped ${LABEL}"
    ;;
status)
    launchctl print "${SESSION}/${LABEL}" 2>/dev/null | sed -n '1,12p' || echo "${LABEL} is not loaded"
    echo "---"
    echo "/metrics -> HTTP $(curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:9104/metrics || echo 000)"
    ;;
logs)
    exec tail -n 50 -f "${LOG_FILE}"
    ;;
*)
    usage
    ;;
esac
