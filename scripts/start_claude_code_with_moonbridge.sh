#!/usr/bin/env bash
set -euo pipefail

if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  echo "Do not source this script; run it as ./scripts/start_claude_code_with_moonbridge.sh to avoid polluting your shell." >&2
  return 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_FILE="${MOONBRIDGE_CONFIG:-"${ROOT_DIR}/config.yml"}"
CLAUDE_CONFIG_DIR_VALUE="${ROOT_DIR}/FakeHome/ClaudeCode"
GLOBAL_CLAUDE_SETTINGS="${MOONBRIDGE_CLAUDE_SETTINGS:-"${HOME}/.claude/settings.json"}"
SERVER_BIN="${ROOT_DIR}/.cache/start-claude/moonbridge"
LOG_FILE="${ROOT_DIR}/logs/moonbridge-claude-code.log"
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

prepare_claude_settings() {
  local target_settings="${CLAUDE_CONFIG_DIR_VALUE}/settings.json"
  local env_file="${CLAUDE_CONFIG_DIR_VALUE}/moonbridge-env.sh"
  local base_url="http://${BASE_ADDR}"

  python3 - "$GLOBAL_CLAUDE_SETTINGS" "$target_settings" "$env_file" "$base_url" "$MODEL" <<'PY'
import json
import os
import shlex
import sys
from pathlib import Path

source_path = Path(sys.argv[1])
target_path = Path(sys.argv[2])
env_path = Path(sys.argv[3])
base_url = sys.argv[4]
model = sys.argv[5]
model_placeholders = {
    "",
    "provider-model-name",
    "replace-with-provider-model-name",
    "replace-with-real-model-name",
}

settings = {}
loaded_source = False
if source_path.exists():
    try:
        settings = json.loads(source_path.read_text())
        if not isinstance(settings, dict):
            settings = {}
        loaded_source = True
    except json.JSONDecodeError as exc:
        raise SystemExit(f"failed to parse {source_path}: {exc}") from exc

env = settings.get("env")
if not isinstance(env, dict):
    env = {}
else:
    env = {str(key): str(value) for key, value in env.items()}

env["ANTHROPIC_BASE_URL"] = base_url
env["ANTHROPIC_AUTH_TOKEN"] = "moonbridge-proxy-placeholder"
env.pop("ANTHROPIC_API_KEY", None)
env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
settings["includeCoAuthoredBy"] = False

if model and model not in model_placeholders:
    env["ANTHROPIC_MODEL"] = model
    env["ANTHROPIC_CUSTOM_MODEL_OPTION"] = model
    settings["model"] = model
elif "model" not in settings:
    env.pop("ANTHROPIC_MODEL", None)
    env.pop("ANTHROPIC_CUSTOM_MODEL_OPTION", None)

settings["env"] = env
target_path.parent.mkdir(parents=True, exist_ok=True)
target_path.write_text(json.dumps(settings, ensure_ascii=False, indent=2) + "\n")
os.chmod(target_path, 0o600)

export_keys = [
    "ANTHROPIC_AUTH_TOKEN",
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_MODEL",
    "ANTHROPIC_CUSTOM_MODEL_OPTION",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
]
lines = []
for key in export_keys:
    value = env.get(key)
    if value is not None:
        lines.append(f"export {key}={shlex.quote(value)}")
effective_model = env.get("ANTHROPIC_MODEL") or settings.get("model") or ""
if effective_model:
    lines.append(f"export MOONBRIDGE_EFFECTIVE_CLAUDE_MODEL={shlex.quote(str(effective_model))}")
env_path.write_text("\n".join(lines) + "\n")
os.chmod(env_path, 0o600)

if loaded_source:
    print(f"Seeded Claude Code settings from {source_path} with placeholder ANTHROPIC_AUTH_TOKEN")
else:
    print(f"No global Claude Code settings found at {source_path}; using placeholder ANTHROPIC_AUTH_TOKEN")
if model and model in model_placeholders:
    print(f"Ignoring placeholder developer.proxy.anthropic.model={model!r}; using Claude Code settings/default model")
PY

  # shellcheck source=/dev/null
  source "$env_file"
}

require_command go
require_command claude
require_command python3

if [[ ! -f "$CONFIG_FILE" ]]; then
  log_error "missing config file: ${CONFIG_FILE}"
  log_error "copy config.example.yml to config.yml and fill developer.proxy.anthropic settings"
  exit 1
fi

mkdir -p "$CLAUDE_CONFIG_DIR_VALUE" "${ROOT_DIR}/.cache/go-build" "$(dirname "$SERVER_BIN")"

export MOONBRIDGE_CONFIG="$CONFIG_FILE"
export CGO_ENABLED="${CGO_ENABLED:-0}"
export GOCACHE="${GOCACHE:-"${ROOT_DIR}/.cache/go-build"}"

log "Building Moon Bridge"
(
  cd "$ROOT_DIR"
  go build -o "$SERVER_BIN" ./cmd/moonbridge
) 2>&1 | tee -a "$LOG_FILE"

MODE="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-mode 2>>"$LOG_FILE")"
if [[ "$MODE" != "CaptureAnthropic" ]]; then
  log_error "config.yml mode must be CaptureAnthropic for Claude Code, got: ${MODE}"
  exit 1
fi

ADDR="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-addr 2>>"$LOG_FILE")"
MODEL="$("$SERVER_BIN" --config "$CONFIG_FILE" --print-claude-model 2>>"$LOG_FILE")"
parse_addr
ensure_port_free
prepare_claude_settings > >(tee -a "$LOG_FILE") 2>&1

log "Starting Moon Bridge on ${ADDR}"
log "Moon Bridge log: ${LOG_FILE}"
(
  cd "$ROOT_DIR"
  "$SERVER_BIN"
) >> "$LOG_FILE" 2>&1 &
SERVER_PID="$!"
wait_for_server

export CLAUDE_CONFIG_DIR="$CLAUDE_CONFIG_DIR_VALUE"

log "Starting Claude Code with CLAUDE_CONFIG_DIR=${CLAUDE_CONFIG_DIR}"
log "Workspace: ${ROOT_DIR}"
log "Anthropic base URL: ${ANTHROPIC_BASE_URL}"
if [[ -n "${MOONBRIDGE_EFFECTIVE_CLAUDE_MODEL:-}" ]]; then
  log "Model: ${MOONBRIDGE_EFFECTIVE_CLAUDE_MODEL}"
fi

set +e
if [[ -n "$PROMPT" ]]; then
  claude "$PROMPT"
else
  claude
fi
CLAUDE_STATUS=$?
set -e

log "Claude Code exited with status ${CLAUDE_STATUS}"
exit "$CLAUDE_STATUS"
