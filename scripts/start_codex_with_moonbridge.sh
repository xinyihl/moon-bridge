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
PROMPT="${1:-}"

require_command() {
  local command_name="$1"
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "missing required command: ${command_name}" >&2
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
      echo "Moon Bridge exited before it became ready on ${ADDR}" >&2
      echo "See Moon Bridge log: ${LOG_FILE}" >&2
      return 1
    fi
    if (echo > "/dev/tcp/${HOST}/${PORT}") >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  echo "Moon Bridge did not start on ${ADDR}" >&2
  echo "See Moon Bridge log: ${LOG_FILE}" >&2
  return 1
}

ensure_port_free() {
  if (echo > "/dev/tcp/${HOST}/${PORT}") >/dev/null 2>&1; then
    echo "port already in use: ${ADDR}" >&2
    echo "change server.addr in config.yml, or stop the process using ${ADDR}" >&2
    echo "Moon Bridge log: ${LOG_FILE}" >&2
    exit 1
  fi
}

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" >/dev/null 2>&1; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
    wait "$SERVER_PID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

require_command go
require_command codex

if [[ ! -f "$CONFIG_FILE" ]]; then
  echo "missing config file: ${CONFIG_FILE}" >&2
  echo "copy config.example.yml to config.yml and fill provider settings" >&2
  exit 1
fi

mkdir -p "$CODEX_HOME_DIR" "${ROOT_DIR}/.cache/go-build" "$(dirname "$SERVER_BIN")" "$(dirname "$LOG_FILE")"
: > "$LOG_FILE"

export MOONBRIDGE_CONFIG="$CONFIG_FILE"
export CGO_ENABLED="${CGO_ENABLED:-0}"
export GOCACHE="${GOCACHE:-"${ROOT_DIR}/.cache/go-build"}"

echo "Building Moon Bridge"
(
  cd "$ROOT_DIR"
  go build -o "$SERVER_BIN" ./cmd/moonbridge
)

MODE="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-mode)"
case "$MODE" in
  Transform|CaptureResponse)
    ;;
  *)
    echo "config.yml mode must be Transform or CaptureResponse for Codex, got: ${MODE}" >&2
    exit 1
    ;;
esac

MODEL_ALIAS="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-codex-model)"
if [[ -z "$MODEL_ALIAS" ]]; then
  echo "provider.default_model or developer.proxy.response.model is required for Codex" >&2
  exit 1
fi

ADDR="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-addr)"
parse_addr
ensure_port_free

"$SERVER_BIN" \
  --config "$CONFIG_FILE" \
  --print-codex-config "$MODEL_ALIAS" \
  --codex-base-url "http://${BASE_ADDR}/v1" \
  > "${CODEX_HOME_DIR}/config.toml"

echo "Starting Moon Bridge on ${ADDR}"
echo "Moon Bridge log: ${LOG_FILE}"
(
  cd "$ROOT_DIR"
  "$SERVER_BIN"
) > "$LOG_FILE" 2>&1 &
SERVER_PID="$!"
wait_for_server

export CODEX_HOME="$CODEX_HOME_DIR"
export MOONBRIDGE_CLIENT_API_KEY="${MOONBRIDGE_CLIENT_API_KEY:-local-dev}"

echo "Starting Codex with CODEX_HOME=${CODEX_HOME_DIR}"
echo "Workspace: ${ROOT_DIR}"
echo "Mode: ${MODE}"
echo "Model: ${MODEL_ALIAS}"

codex_args=(
  --sandbox workspace-write
  --ask-for-approval on-request
  --cd "$ROOT_DIR"
)

if [[ -n "$PROMPT" ]]; then
  codex_args+=("$PROMPT")
fi

codex "${codex_args[@]}"
