#!/usr/bin/env bash
# Start Codex CLI using an already-running Moon Bridge server.
# Requires: start_moonbridge.sh to have been run first (or .moonbridge.env present).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ROOT_DIR}/.moonbridge.env"
CODEX_HOME_DIR="${ROOT_DIR}/FakeHome/Codex"
GLOBAL_CODEX_CONFIG="${MOONBRIDGE_CODEX_CONFIG:-"${HOME}/.codex/config.toml"}"
LOG_FILE="${ROOT_DIR}/logs/codex.log"
PROMPT="${1:-}"

if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  echo "Do not source this script; run it directly." >&2
  return 1
fi

log() { printf '%s\n' "$*" | tee -a "$LOG_FILE"; }
log_error() { printf '%s\n' "$*" | tee -a "$LOG_FILE" >&2; }

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    log_error "missing required command: $1"
    exit 1
  fi
}

require_command codex
mkdir -p "$CODEX_HOME_DIR" "$(dirname "$LOG_FILE")"

# Load moonbridge connection info.
if [[ -f "$ENV_FILE" ]]; then
  source "$ENV_FILE"
else
  log_error "moonbridge env file not found at ${ENV_FILE}"
  log_error "run scripts/start_moonbridge.sh first"
  exit 1
fi

if [[ -z "${MOONBRIDGE_ADDR:-}" ]]; then
  log_error "moonbridge not configured in ${ENV_FILE}"
  exit 1
fi

# Validate mode.
case "${MOONBRIDGE_MODE:-}" in
  Transform|CaptureResponse)
    ;;
  "")
    log_error "moonbridge mode not found"
    exit 1
    ;;
  *)
    log_error "moonbridge mode ${MOONBRIDGE_MODE} is not suitable for Codex (need Transform or CaptureResponse)"
    exit 1
    ;;
esac

# Build Codex config.
MODEL_ALIAS="${MOONBRIDGE_CODEX_MODEL:-${MOONBRIDGE_DEFAULT_MODEL:-}}"
if [[ -z "$MODEL_ALIAS" ]]; then
  log_error "no model alias configured for Codex"
  exit 1
fi

BASE_ADDR="${MOONBRIDGE_ADDR}"
HOST="${BASE_ADDR%:*}"
PORT="${BASE_ADDR##*:}"

# Verify moonbridge is still alive.
if ! (echo > "/dev/tcp/${HOST}/${PORT}") >/dev/null 2>&1; then
  log_error "Moon Bridge not reachable on ${MOONBRIDGE_ADDR}"
  log_error "restart it with scripts/start_moonbridge.sh"
  exit 1
fi

# Generate Codex config via the server binary.
SERVER_BIN="${MOONBRIDGE_SERVER_BIN}"
CONFIG_FILE="${MOONBRIDGE_CONFIG_FILE}"

if [[ -x "$SERVER_BIN" && -f "$CONFIG_FILE" ]]; then
  "$SERVER_BIN" \
    --config "$CONFIG_FILE" \
    --print-codex-config "$MODEL_ALIAS" \
    --codex-base-url "http://${HOST}:${PORT}/v1" \
    --codex-home "$CODEX_HOME_DIR" \
    2>>"$LOG_FILE" \
    > "${CODEX_HOME_DIR}/config.toml"

  # Copy status_line from global config.
  if [[ -f "$GLOBAL_CODEX_CONFIG" ]]; then
    status_line="$(
      awk '
        /^\[/ { in_tui = ($0 == "[tui]"); capture = 0 }
        in_tui && /^[[:space:]]*status_line[[:space:]]*=/ { capture = 1; print; if ($0 ~ /\]/) capture = 0; next }
        in_tui && capture { print; if ($0 ~ /\]/) capture = 0; next }
      ' "$GLOBAL_CODEX_CONFIG"
    )"
    if [[ -n "$status_line" ]]; then
      { printf '\n[tui]\n'; printf '%s\n' "$status_line"; } >> "${CODEX_HOME_DIR}/config.toml"
      log "Copied Codex status_line from ${GLOBAL_CODEX_CONFIG}"
    fi
  fi
else
  log_error "cannot generate Codex config: server binary or config not found"
  exit 1
fi

export CODEX_HOME="$CODEX_HOME_DIR"
export MOONBRIDGE_CLIENT_API_KEY="${MOONBRIDGE_CLIENT_API_KEY:-local-dev}"

log "Starting Codex with CODEX_HOME=${CODEX_HOME_DIR}"
log "Workspace: ${ROOT_DIR}"
log "Mode: ${MOONBRIDGE_MODE}"
log "Model: ${MODEL_ALIAS}"

codex_args=(
  --sandbox workspace-write
  --ask-for-approval on-request
  --cd "$ROOT_DIR"
)

if [[ -n "$PROMPT" ]]; then
  codex_args+=("$PROMPT")
fi

set +e
codex "${codex_args[@]}"
CODEX_STATUS=$?
set -e

log "Codex exited with status ${CODEX_STATUS}"
exit "$CODEX_STATUS"
