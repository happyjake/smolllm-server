#!/usr/bin/env bash
set -euo pipefail

# Manage the smolllm-server LaunchAgent.

readonly SESSION="gui/$(id -u)"
readonly PLIST="personal.smolllm-server.plist"
readonly LABEL="personal.smolllm-server"
# Every path below is derived from the invoking user's $HOME. Nothing in this
# repo may hardcode a username: the rendered plist is per-machine, and the
# template it comes from is checked in with @PLACEHOLDER@ markers instead.
readonly RUN_USER="${USER:-$(id -un)}"
# Must match StandardOutPath/StandardErrorPath rendered into the plist.
readonly LOG_FILE="${HOME}/Library/Logs/${LABEL}.log"
readonly BIN_DIR="${HOME}/.local/bin"
readonly BIN_PATH="${BIN_DIR}/smolllm-server"
readonly CONFIG_DIR="${HOME}/.config/smolllm-server"
readonly CONFIG_PATH="${CONFIG_DIR}/config.yaml"
readonly TARGET="${HOME}/Library/LaunchAgents/${PLIST}"
readonly TEMPLATE_NAME="${PLIST}.template"

# health_url reads server.bind out of the live config so a non-default port
# still gets probed. A wildcard bind is probed over loopback: 0.0.0.0 is not a
# connectable address on macOS.
health_url() {
    if [[ -n "${SMOLLLM_HEALTH_URL:-}" ]]; then
        printf '%s' "${SMOLLLM_HEALTH_URL}"
        return 0
    fi
    local bind="" host port
    if [[ -f "${CONFIG_PATH}" ]]; then
        bind="$(sed -nE 's/^[[:space:]]*bind:[[:space:]]*"?([^"[:space:]#]+)"?.*/\1/p' "${CONFIG_PATH}" | head -1)"
    fi
    bind="${bind:-127.0.0.1:11435}"
    port="${bind##*:}"
    host="${bind%:*}"
    case "${host}" in
    "" | "0.0.0.0" | "::" | "[::]") host="127.0.0.1" ;;
    esac
    printf 'http://%s:%s/healthz' "${host}" "${port}"
}

usage() {
    cat >&2 <<EOF
Usage: $0 {install|reinstall|reload|uninstall|start|stop|status|logs|build}

  install     Build the binary, seed the config, symlink the plist, and bootstrap the agent.
  reinstall   Tear down and reinstall the agent (use after editing the plist).
  reload      Rebuild the binary, kickstart the service, and wait for /healthz.
  uninstall   Stop the agent and remove the plist symlink. Binary and config are kept.
  start       Bootstrap the agent (no rebuild).
  stop        Bootout the agent.
  status      Show running state.
  logs        Tail the StandardOut/Err log.
  build       Just build the binary (no service action).
EOF
    exit 64
}

resolve_repo_root() {
    local script_dir
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
    cd "${script_dir}/.." && pwd -P
}

build_binary() {
    local repo
    # Hosts without a Go toolchain (or with the binary cross-compiled elsewhere
    # and copied in) skip the build and use whatever is already at BIN_PATH.
    if [[ "${SMOLLLM_SKIP_BUILD:-0}" == "1" ]]; then
        if [[ ! -x "${BIN_PATH}" ]]; then
            echo "SMOLLLM_SKIP_BUILD=1 but no executable at ${BIN_PATH}" >&2
            exit 1
        fi
        echo "  SMOLLLM_SKIP_BUILD=1: using existing ${BIN_PATH}"
        return 0
    fi
    repo="$(resolve_repo_root)"
    mkdir -p "${BIN_DIR}"
    echo "→ building ${BIN_PATH} (from ${repo})"
    (cd "${repo}" && go build -o "${BIN_PATH}" ./cmd/server)
    echo "✓ built ${BIN_PATH}"
}

seed_config() {
    local repo example
    repo="$(resolve_repo_root)"
    example="${repo}/config.example.yaml"
    if [[ -f "${CONFIG_PATH}" ]]; then
        echo "  config exists: ${CONFIG_PATH}"
        return 0
    fi
    if [[ ! -f "${example}" ]]; then
        echo "  config.example.yaml not found at ${example}; skipping seed" >&2
        return 0
    fi
    mkdir -p "${CONFIG_DIR}"
    cp "${example}" "${CONFIG_PATH}"
    echo "✓ seeded ${CONFIG_PATH} from config.example.yaml"
}

# render_plist substitutes the current machine's paths into the checked-in
# template and writes a REAL file to ~/Library/LaunchAgents.
#
# It is not a symlink: a symlinked plist ties the loaded job to the repo's
# location on disk, and the previous checked-in plist hardcoded one developer's
# home directory, so `install` on any other account bootstrapped a job pointing
# at a binary that did not exist.
#
# Substitution is bash parameter expansion, not sed: home directories and repo
# paths may contain characters (&, \, |) that sed would interpret in the
# replacement text.
render_plist() {
    local repo template rendered line
    repo="$(resolve_repo_root)"
    template="${repo}/launch/${TEMPLATE_NAME}"
    if [[ ! -f "${template}" ]]; then
        echo "Plist template not found: ${template}" >&2
        exit 1
    fi
    mkdir -p "${HOME}/Library/LaunchAgents"
    # launchd does not create parents for StandardOutPath: a missing directory
    # makes the job fail to spawn with no log to explain why.
    mkdir -p "$(dirname "${LOG_FILE}")"

    rendered="$(mktemp "${TMPDIR:-/tmp}/smolllm-plist.XXXXXX")"
    while IFS= read -r line || [[ -n "${line}" ]]; do
        line="${line//@BIN_PATH@/${BIN_PATH}}"
        line="${line//@WORKING_DIR@/${repo}}"
        line="${line//@CONFIG_PATH@/${CONFIG_PATH}}"
        line="${line//@LOG_FILE@/${LOG_FILE}}"
        line="${line//@HOME@/${HOME}}"
        line="${line//@USER@/${RUN_USER}}"
        printf '%s\n' "${line}" >>"${rendered}"
    done <"${template}"

    if ! plutil -lint "${rendered}" >/dev/null 2>&1; then
        echo "Rendered plist is not valid: ${rendered}" >&2
        exit 1
    fi

    if [[ -f "${TARGET}" && ! -L "${TARGET}" ]] && cmp -s "${rendered}" "${TARGET}"; then
        rm -f "${rendered}"
        echo "  plist already current: ${TARGET}"
        return 0
    fi
    if [[ -L "${TARGET}" ]]; then
        echo "  replacing legacy plist symlink with a rendered file"
    fi
    rm -f "${TARGET}"
    mv "${rendered}" "${TARGET}"
    chmod 644 "${TARGET}"
    echo "✓ rendered ${TARGET} (user ${RUN_USER}, bin ${BIN_PATH})"
}

is_loaded() {
    launchctl print "${SESSION}/${LABEL}" &>/dev/null
}

bootstrap() {
    echo "→ bootstrapping ${LABEL}"
    launchctl bootstrap "${SESSION}" "${TARGET}"
    verify_healthy
}

bootout() {
    if ! is_loaded; then
        echo "  ${LABEL} not loaded"
        return 0
    fi
    echo "→ booting out ${LABEL}"
    launchctl bootout "${SESSION}" "${TARGET}" 2>/dev/null || true
}

# A pid alone proves nothing (a dying or hung process still has one); only a
# 200 from /healthz counts. The 20s window covers the two legal slow paths:
# graceful shutdown (≤5s + 10s watchdog) and launchd's spawn throttle (10s).
verify_healthy() {
    local pid
    for _ in $(seq 1 20); do
        if curl -fsS -m 1 -o /dev/null "$(health_url)" 2>/dev/null; then
            pid=$(launchctl print "${SESSION}/${LABEL}" 2>/dev/null | grep -oE 'pid = [0-9]+' | grep -oE '[0-9]+' || true)
            echo "✓ ${LABEL} healthy (pid ${pid:-unknown})"
            return 0
        fi
        sleep 1
    done
    echo "✗ ${LABEL} not healthy after 20s ($(health_url))" >&2
    local exit_code
    exit_code=$(launchctl print "${SESSION}/${LABEL}" 2>/dev/null | grep -oE 'last exit code = [0-9-]+' | grep -oE '[0-9-]+$' || echo unknown)
    echo "  last exit code: ${exit_code}" >&2
    if [[ -f "${LOG_FILE}" ]]; then
        echo "  recent log:" >&2
        tail -20 "${LOG_FILE}" | sed 's/^/    /' >&2
    fi
    return 1
}

cmd_install() {
    build_binary
    seed_config
    render_plist
    if is_loaded; then
        echo "  ${LABEL} already loaded; use 'reload' to apply binary changes"
        return 0
    fi
    bootstrap
}

cmd_reinstall() {
    build_binary
    seed_config
    bootout
    rm -f "${TARGET}"
    render_plist
    bootstrap
}

cmd_reload() {
    build_binary
    if ! is_loaded; then
        echo "  ${LABEL} not loaded; bootstrapping"
        render_plist
        bootstrap
        return 0
    fi
    echo "→ kickstarting ${LABEL}"
    launchctl kickstart -k "${SESSION}/${LABEL}"
    if verify_healthy; then
        return 0
    fi
    # The kill already happened; the old process is gone either way. A second
    # kickstart is the recovery that fixed the 2026-07-20 incident by hand:
    # -k restarts a hung instance and plain-starts a dead one.
    echo "→ retrying kickstart ${LABEL}" >&2
    launchctl kickstart -k "${SESSION}/${LABEL}"
    verify_healthy
}

cmd_uninstall() {
    bootout
    if [[ -L "${TARGET}" || -e "${TARGET}" ]]; then
        rm -f "${TARGET}"
        echo "✓ removed ${TARGET}"
    fi
    echo "  binary kept at ${BIN_PATH}"
    echo "  config kept at ${CONFIG_PATH}"
}

cmd_start() {
    if is_loaded; then
        echo "${LABEL} already loaded"
        return 0
    fi
    render_plist
    bootstrap
}

cmd_stop() {
    if ! is_loaded; then
        echo "${LABEL} not loaded; nothing to stop"
        return 1
    fi
    bootout
}

cmd_status() {
    if is_loaded; then
        echo "${LABEL}: running"
        launchctl print "${SESSION}/${LABEL}" 2>/dev/null | grep -E "pid|state|last exit" | head -10
    else
        echo "${LABEL}: stopped"
    fi
}

cmd_logs() {
    local log="${LOG_FILE}"
    if [[ ! -f "${log}" ]]; then
        echo "log not found: ${log}" >&2
        exit 1
    fi
    tail -f "${log}"
}

main() {
    case "${1-}" in
    install) cmd_install ;;
    reinstall) cmd_reinstall ;;
    reload) cmd_reload ;;
    uninstall) cmd_uninstall ;;
    start) cmd_start ;;
    stop) cmd_stop ;;
    status) cmd_status ;;
    logs) cmd_logs ;;
    build) build_binary ;;
    *) usage ;;
    esac
}

main "$@"
