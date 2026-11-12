#!/usr/bin/env bash
# Runs v2-server against DeepSeek Flash with the key from ~/ora-demo/.env. The file lives outside the
# repository so every checkout and worktree shares it and worktree cleanup never deletes it.
# `--check` only validates the configuration, so `task v2:deepseek` can stop before starting anything.
set -euo pipefail
export V2_LOGIN=email
cd "$(dirname "$0")/.."

env_file=$HOME/ora-demo/.env
[[ -f $env_file ]] || { echo "Missing $env_file: run \`mkdir -p ~/ora-demo && cp v2-server/.env.example ~/ora-demo/.env\` and fill in DEEPSEEK_API_KEY." >&2; exit 1; }

# Read KEY=VALUE lines without sourcing, so the file is never executed as shell.
read_value() {
  local line value
  line=$(grep -E "^[[:space:]]*$1[[:space:]]*=" "$env_file" | tail -1) || true
  value=${line#*=}
  value=${value%$'\r'}
  value=$(printf '%s' "$value" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//; s/^"(.*)"$/\1/; s/^'\''(.*)'\''$/\1/')
  printf '%s' "$value"
}

key=$(read_value DEEPSEEK_API_KEY)
[[ -n $key ]] || { echo "DEEPSEEK_API_KEY is empty in $env_file." >&2; exit 1; }

assets=$(read_value V2_NODE_ASSETS_DIR)
assets=${assets:-${V2_NODE_ASSETS_DIR:-$HOME/.cache/ora-v2-node}}
assets=${assets/#\~/$HOME}
# The chat page only offers Agent executors, so a demo without Node assets cannot start a conversation.
[[ -f $assets/assets.json ]] || { echo "No prepared Node assets at $assets: run \`task v2:node:prepare\` first (or set V2_NODE_ASSETS_DIR in $env_file)." >&2; exit 1; }

# Optional Claude provider, read by the same launcher as the OpenCode provider.
for name in V2_ANTHROPIC_MODEL_BASE_URL V2_ANTHROPIC_MODEL_NAME V2_ANTHROPIC_MODEL_API_KEY V2_ANTHROPIC_MODEL_AUTH_TOKEN; do
  value=$(read_value "$name")
  [[ -z $value ]] || export "$name=$value"
done
if [[ -n ${V2_ANTHROPIC_MODEL_BASE_URL:-}${V2_ANTHROPIC_MODEL_NAME:-}${V2_ANTHROPIC_MODEL_API_KEY:-}${V2_ANTHROPIC_MODEL_AUTH_TOKEN:-} ]]; then
  if [[ -z ${V2_ANTHROPIC_MODEL_BASE_URL:-} || -z ${V2_ANTHROPIC_MODEL_NAME:-} ||
        ( -z ${V2_ANTHROPIC_MODEL_API_KEY:-} && -z ${V2_ANTHROPIC_MODEL_AUTH_TOKEN:-} ) ||
        ( -n ${V2_ANTHROPIC_MODEL_API_KEY:-} && -n ${V2_ANTHROPIC_MODEL_AUTH_TOKEN:-} ) ]]; then
    echo "Claude Code requires a base URL, model name, and exactly one API key or auth token." >&2
    exit 1
  fi
fi

[[ ${1:-} == --check ]] && exit 0

export V2_MODEL_BASE_URL=https://api.deepseek.com
export V2_MODEL_NAME=deepseek-flash
export V2_MODEL_API_KEY=$key
export V2_NODE_ASSETS_DIR=$assets
# scripts/demo.sh passes a prebuilt binary, so stopping a demo stops the server rather than `go run`.
[[ -n ${V2_SERVER_BIN:-} ]] && exec "$V2_SERVER_BIN"
exec go run .
