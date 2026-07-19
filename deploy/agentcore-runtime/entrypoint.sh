#!/usr/bin/env bash
# Copyright (c) 2026 panlm
# SPDX-License-Identifier: MIT
#
# Container entrypoint for the lark-cli MCP wrapper on AgentCore Runtime.
# Starts the AgentCore Identity sidecar (loopback-only) in the background,
# waits for it to accept connections, wires the sidecar env vars lark-cli
# expects, then execs the MCP wrapper in the foreground so it becomes PID 1's
# replacement and receives container signals directly.
#
# lark-cli itself is not a long-running process here: the MCP wrapper shells
# out to the lark-cli binary per tool call (see mcp-wrapper/exec_tool.go),
# inheriting the environment this script exports.
set -euo pipefail

SIDECAR_LISTEN="${SIDECAR_LISTEN:-127.0.0.1:16385}"
SIDECAR_CALLBACK_LISTEN="${SIDECAR_CALLBACK_LISTEN:-127.0.0.1:3000}"
SIDECAR_KEY_FILE="${SIDECAR_KEY_FILE:-/tmp/lark-sidecar-agentcore/proxy.key}"
SIDECAR_READY_TIMEOUT_S="${SIDECAR_READY_TIMEOUT_S:-10}"

/usr/local/bin/sidecar-agentcore-identity \
  --listen "$SIDECAR_LISTEN" \
  --callback-listen "$SIDECAR_CALLBACK_LISTEN" \
  --key-file "$SIDECAR_KEY_FILE" &
sidecar_pid=$!

deadline=$(( $(date +%s) + SIDECAR_READY_TIMEOUT_S ))
until curl -sS -o /dev/null "http://${SIDECAR_LISTEN}/"; do
  if ! kill -0 "$sidecar_pid" 2>/dev/null; then
    echo "entrypoint: sidecar process exited before becoming ready" >&2
    exit 1
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "entrypoint: sidecar did not become ready on ${SIDECAR_LISTEN} within ${SIDECAR_READY_TIMEOUT_S}s" >&2
    exit 1
  fi
  sleep 0.2
done

export LARKSUITE_CLI_AUTH_PROXY="http://${SIDECAR_LISTEN}"
export LARKSUITE_CLI_PROXY_KEY="$(cat "$SIDECAR_KEY_FILE")"
export LARKSUITE_CLI_APP_ID="${LARKSUITE_CLI_APP_ID:-cli_a93bd889cfb91cc9}"
export LARKSUITE_CLI_BRAND="${LARKSUITE_CLI_BRAND:-feishu}"
export LARKSUITE_CLI_DEFAULT_AS="${LARKSUITE_CLI_DEFAULT_AS:-user}"
export LARK_CLI_BIN_PATH="${LARK_CLI_BIN_PATH:-/usr/local/bin/lark-cli}"
export AGENTCORE_RUNTIME=1

exec /usr/local/bin/mcp-wrapper
