#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p .runtime
exec 9>.runtime/tucunar-bridge.lock
if ! flock -n 9; then
  echo "The Tucunar bridge is already running for this checkout." >&2
  exit 1
fi
forward_pid=""
mqtt_pid=""
protocols_pid=""
cleanup() {
  local pid
  for pid in "$mqtt_pid" "$protocols_pid" "$forward_pid"; do
    [[ -z "$pid" ]] || kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
K0S="${K0S_BIN:-/usr/local/bin/k0s}"
NAMESPACE="${TUCUNAR_NAMESPACE:-tucunar-cell}"
if [[ -z "${SIGNAL_INGEST_TOKEN:-}" ]]; then
  SIGNAL_INGEST_TOKEN="$("$K0S" kubectl -n "$NAMESPACE" get secret tucunar-cell-secrets -o 'jsonpath={.data.SIGNAL_INGEST_TOKEN}' | base64 -d)"
  export SIGNAL_INGEST_TOKEN
fi
: "${SIGNAL_INGEST_TOKEN:?Set SIGNAL_INGEST_TOKEN to the cell ingest token}"
if [[ "${TUCUNAR_SIGNAL_URL:-http://127.0.0.1:18080/v1/signals/batch}" == "http://127.0.0.1:18080/v1/signals/batch" ]] && ! (exec 3<>/dev/tcp/127.0.0.1/18080) 2>/dev/null; then
  "$K0S" kubectl -n "$NAMESPACE" port-forward --address=127.0.0.1 service/swift 18080:8080 >/tmp/tucunar-scada-forward.log 2>&1 &
  forward_pid=$!
  for _ in {1..30}; do
    (exec 3<>/dev/tcp/127.0.0.1/18080) 2>/dev/null && break
    kill -0 "$forward_pid" 2>/dev/null || break
    sleep 1
  done
  if ! (exec 3<>/dev/tcp/127.0.0.1/18080) 2>/dev/null; then
    kill "$forward_pid" 2>/dev/null || true
    wait "$forward_pid" 2>/dev/null || true
    forward_pid=""
    if command -v socat >/dev/null; then
      service_ip="$("$K0S" kubectl -n "$NAMESPACE" get service swift -o 'jsonpath={.spec.clusterIP}')"
      socat TCP-LISTEN:18080,bind=127.0.0.1,reuseaddr,fork "TCP:$service_ip:8080" >/tmp/tucunar-scada-forward.log 2>&1 &
      forward_pid=$!
      sleep 1
    fi
  fi
  if ! (exec 3<>/dev/tcp/127.0.0.1/18080) 2>/dev/null; then
    echo "Swift local port is unavailable; see /tmp/tucunar-scada-forward.log" >&2
    exit 1
  fi
fi
supervise_reader() {
  local script="$1" child_pid="" delay=1 started status
  stop_child() {
    if [[ -n "$child_pid" ]]; then
      kill "$child_pid" 2>/dev/null || true
      wait "$child_pid" 2>/dev/null || true
    fi
  }
  trap stop_child EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  while true; do
    started=$SECONDS
    ./.venv/bin/python "$script" &
    child_pid=$!
    status=0
    wait "$child_pid" || status=$?
    child_pid=""
    if (( SECONDS - started >= 60 )); then
      delay=1
    fi
    printf '%s exited with status %s; restarting in %ss\n' "$script" "$status" "$delay" >&2
    sleep "$delay" &
    child_pid=$!
    wait "$child_pid"
    child_pid=""
    delay=$((delay * 2))
    if (( delay > 30 )); then
      delay=30
    fi
  done
}
supervise_reader tucunar_bridge.py &
mqtt_pid=$!
supervise_reader tucunar_protocols.py &
protocols_pid=$!
wait -n "$mqtt_pid" "$protocols_pid"
