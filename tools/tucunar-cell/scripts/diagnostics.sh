#!/usr/bin/env bash
set -euo pipefail

url="${1:-http://127.0.0.1:18080/api/diagnostics}"
args=(--fail --silent --show-error --max-time "${CANARY_DIAGNOSTICS_TIMEOUT_SECONDS:-10}" "$url")
if [[ -n "${CANARY_NODE_API_TOKEN:-}" ]]; then
  args=(--header "Authorization: Bearer ${CANARY_NODE_API_TOKEN}" "${args[@]}")
fi
curl "${args[@]}"
echo
