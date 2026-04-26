#!/usr/bin/env bash
set -euo pipefail

if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  echo "Do not source this script; run it as ./scripts/start_codex_with_moonbridge.sh to avoid polluting your shell." >&2
  return 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_FILE="${MOONBRIDGE_CONFIG:-"${ROOT_DIR}/config.yml"}"
CODEX_HOME_DIR="${ROOT_DIR}/FakeHome/Codex"
SERVER_BIN="${ROOT_DIR}/.cache/start-codex/moonbridge"
LOG_FILE="${ROOT_DIR}/logs/moonbridge-codex.log"
GLOBAL_CODEX_CONFIG="${MOONBRIDGE_CODEX_CONFIG:-"${HOME}/.codex/config.toml"}"
PROMPT="${1:-}"

mkdir -p "$(dirname "$LOG_FILE")"
: > "$LOG_FILE"

log() {
  printf '%s\n' "$*" | tee -a "$LOG_FILE"
}

log_error() {
  printf '%s\n' "$*" | tee -a "$LOG_FILE" >&2
}

require_command() {
  local command_name="$1"
  if ! command -v "$command_name" >/dev/null 2>&1; then
    log_error "missing required command: ${command_name}"
    exit 1
  fi
}

parse_addr() {
  if [[ "$ADDR" == :* ]]; then
    HOST="127.0.0.1"
    PORT="${ADDR#:}"
    BASE_ADDR="${HOST}:${PORT}"
    return
  fi
  HOST="${ADDR%:*}"
  PORT="${ADDR##*:}"
  BASE_ADDR="$ADDR"
}

wait_for_server() {
  local deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    if ! kill -0 "$SERVER_PID" >/dev/null 2>&1; then
      log_error "Moon Bridge exited before it became ready on ${ADDR}"
      log_error "See Moon Bridge log: ${LOG_FILE}"
      return 1
    fi
    if (echo > "/dev/tcp/${HOST}/${PORT}") >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  log_error "Moon Bridge did not start on ${ADDR}"
  log_error "See Moon Bridge log: ${LOG_FILE}"
  return 1
}

ensure_port_free() {
  if (echo > "/dev/tcp/${HOST}/${PORT}") >/dev/null 2>&1; then
    log_error "port already in use: ${ADDR}"
    log_error "change server.addr in config.yml, or stop the process using ${ADDR}"
    log_error "Moon Bridge log: ${LOG_FILE}"
    exit 1
  fi
}

append_codex_status_line() {
  local target_config="$1"
  local status_line=""

  if [[ ! -f "$GLOBAL_CODEX_CONFIG" ]]; then
    log "No global Codex config found at ${GLOBAL_CODEX_CONFIG}; status_line not copied"
    return
  fi

  status_line="$(
    awk '
      /^\[/ {
        in_tui = ($0 == "[tui]")
        capture = 0
      }
      in_tui && /^[[:space:]]*status_line[[:space:]]*=/ {
        capture = 1
        print
        if ($0 ~ /\]/) {
          capture = 0
        }
        next
      }
      in_tui && capture {
        print
        if ($0 ~ /\]/) {
          capture = 0
        }
        next
      }
    ' "$GLOBAL_CODEX_CONFIG"
  )"

  if [[ -z "$status_line" ]]; then
    log "No [tui].status_line found in ${GLOBAL_CODEX_CONFIG}; status_line not copied"
    return
  fi

  {
    printf '\n[tui]\n'
    printf '%s\n' "$status_line"
  } >> "$target_config"
  log "Copied Codex status_line from ${GLOBAL_CODEX_CONFIG}"
}

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" >/dev/null 2>&1; then
    log "Stopping Moon Bridge"
    kill "$SERVER_PID" >/dev/null 2>&1 || true
    wait "$SERVER_PID" >/dev/null 2>&1 || true
    log "Moon Bridge stopped"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

require_command go
require_command codex

if [[ ! -f "$CONFIG_FILE" ]]; then
  log_error "missing config file: ${CONFIG_FILE}"
  log_error "copy config.example.yml to config.yml and fill provider settings"
  exit 1
fi

mkdir -p "$CODEX_HOME_DIR" "${ROOT_DIR}/.cache/go-build" "$(dirname "$SERVER_BIN")"

export MOONBRIDGE_CONFIG="$CONFIG_FILE"
export CGO_ENABLED="${CGO_ENABLED:-0}"
export GOCACHE="${GOCACHE:-"${ROOT_DIR}/.cache/go-build"}"

log "Building Moon Bridge"
(
  cd "$ROOT_DIR"
  go build -o "$SERVER_BIN" ./cmd/moonbridge
) 2>&1 | tee -a "$LOG_FILE"

MODE="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-mode 2>>"$LOG_FILE")"
case "$MODE" in
  Transform|CaptureResponse)
    ;;
  *)
    log_error "config.yml mode must be Transform or CaptureResponse for Codex, got: ${MODE}"
    exit 1
    ;;
esac

MODEL_ALIAS="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-codex-model 2>>"$LOG_FILE")"
if [[ -z "$MODEL_ALIAS" ]]; then
  log_error "provider.default_model or developer.proxy.response.model is required for Codex"
  exit 1
fi

ADDR="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-addr 2>>"$LOG_FILE")"
parse_addr
ensure_port_free

"$SERVER_BIN" \
  --config "$CONFIG_FILE" \
  --print-codex-config "$MODEL_ALIAS" \
  --codex-base-url "http://${BASE_ADDR}/v1" \
  2>>"$LOG_FILE" \
  > "${CODEX_HOME_DIR}/config.toml"
append_codex_status_line "${CODEX_HOME_DIR}/config.toml"

log "Starting Moon Bridge on ${ADDR}"
log "Moon Bridge log: ${LOG_FILE}"
(
  cd "$ROOT_DIR"
  "$SERVER_BIN"
) >> "$LOG_FILE" 2>&1 &
SERVER_PID="$!"
wait_for_server

export CODEX_HOME="$CODEX_HOME_DIR"
export MOONBRIDGE_CLIENT_API_KEY="${MOONBRIDGE_CLIENT_API_KEY:-local-dev}"

log "Starting Codex with CODEX_HOME=${CODEX_HOME_DIR}"
log "Workspace: ${ROOT_DIR}"
log "Mode: ${MODE}"
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
