#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$script_dir/../deploy"
profile="${CANARY_K0S_PROFILE:-single-node}"
env_file="${CANARY_ENV_FILE:-k0s/overlays/single-node/secret.env}"

if [[ "$profile" != "dev" ]]; then
  if [[ ! -f "$env_file" ]]; then
    echo "missing $env_file; copy secret.env.example to secret.env first" >&2
    exit 1
  fi

  required=(CLICKHOUSE_PASSWORD SIGNAL_INGEST_TOKEN CANARY_NODE_API_TOKEN TUCUNAR_INTERNAL_TOKEN)
  for key in "${required[@]}"; do
    value="$(sed -n "s/^${key}=//p" "$env_file" | tail -n 1)"
    if [[ -z "$value" || "$value" == CHANGE_ME* || "$value" == REQUIRED_* ]]; then
      echo "$key is missing or still a placeholder" >&2
      exit 1
    fi
  done

  bind="$(sed -n 's/^CANARY_NODE_BIND=//p' "$env_file" | tail -n 1)"
  operator_token="$(sed -n 's/^CANARY_NODE_API_TOKEN=//p' "$env_file" | tail -n 1)"
  if [[ -n "$bind" && "$bind" != "127.0.0.1" && "$bind" != "localhost" && "$bind" != "::1" && "$bind" != "[::1]" ]]; then
    if [[ ${#operator_token} -lt 16 ]]; then
      echo "CANARY_NODE_API_TOKEN must be set for a non-loopback bind" >&2
      exit 1
    fi
  fi

  if [[ "$(stat -c '%a' "$env_file" 2>/dev/null || true)" != "600" ]]; then
    echo "warning: $env_file should have mode 600" >&2
  fi
fi

node_dir="$(cd -- "$script_dir/.." && pwd)"
if command -v kubectl >/dev/null 2>&1; then
  kubectl kustomize "$node_dir/deploy/k0s/overlays/$profile" >/dev/null
elif command -v kustomize >/dev/null 2>&1; then
  kustomize build "$node_dir/deploy/k0s/overlays/$profile" >/dev/null
elif command -v yq >/dev/null 2>&1; then
  while IFS= read -r -d '' manifest; do
    yq eval '.' "$manifest" >/dev/null
  done < <(find "$node_dir/deploy/k0s" -type f -name '*.yaml' -print0)
  echo "warning: kubectl/kustomize unavailable; YAML syntax only was validated" >&2
else
  echo "missing kubectl, kustomize or yq; cannot validate k0s manifests" >&2
  exit 1
fi
echo "configuration valid: k0s $profile overlay"
