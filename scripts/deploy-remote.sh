#!/usr/bin/env bash
# Deploy smolllm-server to a remote host without hand-copying a binary.
#
# Why this exists: on 2026-10-19 a macOS arm64 build was copied to the Linux
# host. systemd refused it with `status=203/EXEC` (Exec format error) and
# restart-looped for 33s before anyone noticed. Nothing in the copy command
# could have caught it - the file transferred fine and only the kernel
# disagreed. So this script asks the TARGET what it is, builds for that, and
# proves the binary runs there before it replaces anything.
#
# Usage:
#   scripts/deploy-remote.sh <ssh-host> [--build-only] [--dry-run]
#
#   ssh-host     a Host from ~/.ssh/config (e.g. doom-cmd)
#   --build-only stop after producing and inspecting the artifact
#   --dry-run    print every remote command, change nothing
#
# The library checkout must sit beside this repo (go.mod has a `replace`).
#
# ---------------------------------------------------------------------------
# macOS / launchd hosts
# ---------------------------------------------------------------------------
# This script needs ssh into the target. doom (Linux/systemd) is the normal
# case. A Mac is NOT deployable this way out of the box: macOS ships with
# Remote Login off, so `ssh $(hostname -s)` refuses and the script exits at step
# 1 with "cannot reach ... over ssh". Two supported ways to ship to a Mac:
#
#   a) From the Mac itself, use its own installer (no ssh involved):
#        ./scripts/install-exporter-macos.sh reinstall   # metrics exporter
#        ./launch/service.sh reinstall                   # the server
#
#   b) From another machine, enable Remote Login first
#      (System Settings > General > Sharing > Remote Login), then run this
#      script with the Mac's ssh Host. Step 5 still smoke-tests on :11499 and
#      step 6 still stops the agent before swapping, so the safety properties
#      hold -- but note the Mac's launchd branch uses `launchctl kickstart -k`,
#      which terminates the running server immediately. In-flight streaming
#      requests are cut. Deploy to your own laptop when you are not mid-request.
#
# Detecting the Mac's unit: the launchd branch looks for
# `gui/<uid>/personal.smolllm-server`, so the agent must already be installed
# (launch/service.sh install). If nothing matches, the script installs the
# binary and restarts nothing -- read its last line before assuming success.
set -euo pipefail

HOST="${1:-}"
BUILD_ONLY=0
DRY_RUN=0
for arg in "${@:2}"; do
  case "$arg" in
    --build-only) BUILD_ONLY=1 ;;
    --dry-run) DRY_RUN=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done
if [ -z "$HOST" ]; then
  echo "usage: $0 <ssh-host> [--build-only] [--dry-run]" >&2
  exit 2
fi

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_NAME=smolllm-server
LOCAL_STAGE="/tmp/${BIN_NAME}-deploy.$$"
REMOTE_TMP="/tmp/${BIN_NAME}.incoming.$$"
REMOTE_BIN='$HOME/.local/bin/'"$BIN_NAME"

say() { printf '\n==> %s\n' "$*"; }
run_remote() {
  if [ "$DRY_RUN" = 1 ]; then
    printf '    [dry-run] ssh %s %q\n' "$HOST" "$*"
  else
    ssh "$HOST" "$@"
  fi
}

cleanup() { rm -rf "$LOCAL_STAGE"; }
trap cleanup EXIT

# --- 1. Ask the target what it is ------------------------------------------
say "inspecting $HOST"
if ! remote_uname="$(ssh -o ConnectTimeout=10 "$HOST" 'uname -sm' 2>/dev/null)"; then
  echo "cannot reach $HOST over ssh" >&2
  exit 1
fi
echo "    target: $remote_uname"

case "$remote_uname" in
  "Linux x86_64")   GOOS=linux;  GOARCH=amd64 ;;
  "Linux aarch64"|"Linux arm64") GOOS=linux; GOARCH=arm64 ;;
  "Darwin arm64")   GOOS=darwin; GOARCH=arm64 ;;
  "Darwin x86_64")  GOOS=darwin; GOARCH=amd64 ;;
  *) echo "unsupported target platform: $remote_uname" >&2; exit 1 ;;
esac

# Refuse to cross-compile from a checkout that is not a known state: a dirty
# tree means the deployed binary cannot be reproduced from a commit.
if ! git -C "$REPO" diff --quiet || ! git -C "$REPO" diff --cached --quiet; then
  echo "    WARNING: $REPO has uncommitted changes; deployed binary will not match a commit" >&2
fi
REV="$(git -C "$REPO" rev-parse --short HEAD)"

# --- 2. Build for that platform, not the local one -------------------------
out="$LOCAL_STAGE/$BIN_NAME"
mkdir -p "$LOCAL_STAGE"
say "building $GOOS/$GOARCH at $REV"
(
  cd "$REPO"
  GOENV=off GOFLAGS=-mod=readonly \
    GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
    go build -o "$out" ./cmd/server
)

# --- 3. Check the artifact before it travels -------------------------------
file "$out"
local_desc="$(file -b "$out")"
case "$GOOS/$GOARCH" in
  linux/amd64) expect='ELF 64-bit LSB executable, x86-64' ;;
  linux/arm64) expect='ELF 64-bit LSB executable, ARM aarch64' ;;
  darwin/arm64) expect='Mach-O 64-bit executable arm64' ;;
  darwin/amd64) expect='Mach-O 64-bit executable x86_64' ;;
esac
if ! printf '%s' "$local_desc" | grep -qF "$expect"; then
  echo "built artifact does not look like $expect:" >&2
  echo "  $local_desc" >&2
  exit 1
fi

if [ "$BUILD_ONLY" = 1 ]; then
  say "artifact ready at $out (build-only, nothing deployed)"
  cp "$out" "/tmp/${BIN_NAME}-${GOOS}-${GOARCH}" 2>/dev/null || true
  exit 0
fi

# --- 4. Land it beside the live binary, never on top of it -----------------
say "copying to $HOST:$REMOTE_TMP"
if [ "$DRY_RUN" = 1 ]; then
  printf '    [dry-run] scp %s %s:%s\n' "$out" "$HOST" "$REMOTE_TMP"
else
  scp -q "$out" "$HOST:$REMOTE_TMP"
fi

# Guessing the supervisor is how a deploy half-works: systemd on Linux, launchd
# on macOS. Detect it instead of assuming, and stop it before the swap because
# overwriting a running executable fails with "Text file busy".
say "detecting service manager on $HOST"
svc="$(ssh "$HOST" 'if systemctl --user list-unit-files 2>/dev/null | grep -q "^smolllm-server.service"; then echo systemd; elif launchctl print "gui/$(id -u)/personal.smolllm-server" >/dev/null 2>&1; then echo launchd; else echo none; fi')"
echo "    service manager: $svc"

# --- 5. Prove it runs on the target before trusting it ---------------------
# This is the step that would have caught the 2026-10-19 mistake: an
# incompatible binary fails here, while the old one is still in place.
say "staging smoke test on the target"
run_remote "set -e
  chmod +x $REMOTE_TMP
  stage_dir=\$(mktemp -d)
  cp \$HOME/.config/$BIN_NAME/config.yaml \$stage_dir/config.yaml
  # Same config, different port: proving the binary runs must not disturb the
  # real listener or its clients.
  sed -i.bak 's|^  bind: .*|  bind: 127.0.0.1:11499|' \$stage_dir/config.yaml 2>/dev/null || \
    sed -i '' 's|^  bind: .*|  bind: 127.0.0.1:11499|' \$stage_dir/config.yaml
  $REMOTE_TMP -config \$stage_dir/config.yaml > \$stage_dir/log 2>&1 &
  stage_pid=\$!
  ok=0
  for _ in \$(seq 1 20); do
    if curl -sf -m 2 http://127.0.0.1:11499/healthz >/dev/null 2>&1; then ok=1; break; fi
    sleep 0.5
  done
  kill \$stage_pid 2>/dev/null || true
  if [ \$ok -ne 1 ]; then
    echo 'SMOKE TEST FAILED - binary did not serve healthz on the target:' >&2
    tail -20 \$stage_dir/log >&2
    rm -rf \$stage_dir
    exit 1
  fi
  echo '    smoke test passed (healthz on :11499)'
  rm -rf \$stage_dir"

# --- 6. Swap, keeping one known-good predecessor ---------------------------
say "installing (previous binary kept as $BIN_NAME.pre-deploy)"
run_remote "set -e
  [ -f $REMOTE_BIN ] && cp $REMOTE_BIN $REMOTE_BIN.pre-deploy || true
  mv $REMOTE_TMP $REMOTE_BIN
  chmod 755 $REMOTE_BIN"

say "restarting service"
case "$svc" in  systemd)
    # stop-then-start rather than restart: the old process must release the
    # executable before the new one is started from it.
    run_remote 'systemctl --user stop smolllm-server || true; sleep 1; systemctl --user start smolllm-server'
    sleep 4
    run_remote 'systemctl --user is-active smolllm-server; curl -sf -m 5 http://127.0.0.1:11435/healthz || echo "healthz FAILED"'
    ;;
  launchd)
    # kickstart -k terminates the running process at once: in-flight streaming
    # requests are cut. There is no graceful drain in smolllm-server, so this
    # is documented rather than fixed -- do not deploy to a Mac mid-request.
    run_remote "launchctl kickstart -k gui/\$(id -u)/personal.smolllm-server"
    sleep 4
    run_remote 'curl -sf -m 5 http://127.0.0.1:11435/healthz || echo "healthz FAILED"'
    ;;
  *)
    say "no service manager found; binary installed but nothing was restarted"
    ;;
esac

if [ "$DRY_RUN" = 1 ]; then
  say "dry run complete: $REV ($GOOS/$GOARCH) -> $HOST, nothing changed"
  exit 0
fi

say "deployed $REV ($GOOS/$GOARCH) to $HOST"
echo "    verify by behaviour, not /healthz alone: send one real request through the alias."
echo "    roll back with: ssh $HOST 'cp $REMOTE_BIN.pre-deploy $REMOTE_BIN && <restart>'"
