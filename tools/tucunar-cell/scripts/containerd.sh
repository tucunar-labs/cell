#!/usr/bin/env bash
set -euo pipefail

# Build and move Tucunar cell images through the containerd owned by k0s.
# This script deliberately does not install or start a container runtime.

usage() {
  cat >&2 <<'EOF'
usage:
  containerd.sh check
  containerd.sh build SERVICE [VERSION]
  containerd.sh build-all [VERSION]
  containerd.sh push SERVICE [VERSION]
  containerd.sh load ARCHIVE
  containerd.sh images

Environment:
  TUCUNAR_NERDCTL             nerdctl executable (default: nerdctl)
  TUCUNAR_CONTAINERD_ADDRESS  k0s containerd socket
                              (default: /run/k0s/containerd.sock)
  TUCUNAR_CONTAINERD_NAMESPACE containerd namespace (default: k8s.io)
  TUCUNAR_IMAGE_PREFIX        image prefix (default: ghcr.io/tucunar)
  TUCUNAR_PLATFORM            target platform (default: linux/amd64)
  TUCUNAR_CONTAINERFILE       OCI build recipe relative to build context
                              (default: tools/tucunar-cell/Containerfile)
  TUCUNAR_BUILD_CONTEXT       Build context (default: repository root)
  TUCUNAR_K0S_CTR             k0s executable for imports (default: k0s)
EOF
  exit 2
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
node_dir="$(cd -- "$script_dir/.." && pwd)"
repo_root="$(cd -- "$node_dir/../.." && pwd)"
nerdctl_bin="${TUCUNAR_NERDCTL:-nerdctl}"
containerd_address="${TUCUNAR_CONTAINERD_ADDRESS:-/run/k0s/containerd.sock}"
containerd_namespace="${TUCUNAR_CONTAINERD_NAMESPACE:-k8s.io}"
image_prefix="${TUCUNAR_IMAGE_PREFIX:-ghcr.io/tucunar}"
platform="${TUCUNAR_PLATFORM:-linux/amd64}"
build_context="${TUCUNAR_BUILD_CONTEXT:-$repo_root}"
containerfile="${TUCUNAR_CONTAINERFILE:-tools/tucunar-cell/Containerfile}"
k0s_ctr="${TUCUNAR_K0S_CTR:-k0s}"
version_default="${TUCUNAR_VERSION:-dev}"

services=(
  swift canary-evaluator condor hummingbird
  kingfisher kingbird pigeon woodpecker heron eagle kestrel hawk
  raven phoenix lark owl toucan shiitake
)

valid_service() {
  local candidate="$1"
  local service
  for service in "${services[@]}"; do
    [[ "$candidate" == "$service" ]] && return 0
  done
  return 1
}

nerdctl() {
  command "$nerdctl_bin" \
    --address "$containerd_address" \
    --namespace "$containerd_namespace" \
    "$@"
}

check() {
  command -v "$nerdctl_bin" >/dev/null || {
    echo "missing $nerdctl_bin; install nerdctl with BuildKit support" >&2
    return 1
  }
  [[ -S "$containerd_address" ]] || {
    echo "missing k0s containerd socket: $containerd_address" >&2
    echo "install/start k0s on this host, then retry" >&2
    return 1
  }
  nerdctl info >/dev/null
  echo "containerd ready: $containerd_address (namespace=$containerd_namespace)"
}

build_one() {
  local service="$1"
  local version="${2:-$version_default}"
  valid_service "$service" || { echo "unknown service: $service" >&2; return 2; }
  nerdctl build \
    --file "$build_context/$containerfile" \
    --platform "$platform" \
    --build-arg "SERVICE=$service" \
    --build-arg "VERSION=$version" \
    --tag "$image_prefix/$service:$version" \
    "$build_context"
}

build_all() {
  local version="${1:-$version_default}"
  local service
  for service in "${services[@]}"; do
    build_one "$service" "$version"
  done
}

push_one() {
  local service="$1"
  local version="${2:-$version_default}"
  valid_service "$service" || { echo "unknown service: $service" >&2; return 2; }
  nerdctl push "$image_prefix/$service:$version"
}

load_archive() {
  local archive="$1"
  [[ -f "$archive" ]] || { echo "image archive not found: $archive" >&2; return 1; }
  if command -v "$k0s_ctr" >/dev/null; then
    "$k0s_ctr" ctr --namespace "$containerd_namespace" images import "$archive"
    return
  fi
  nerdctl load --input "$archive"
}

case "${1:-}" in
  check) check ;;
  build) [[ $# -ge 2 ]] || usage; check; build_one "$2" "${3:-$version_default}" ;;
  build-all) check; build_all "${2:-$version_default}" ;;
  push) [[ $# -ge 2 ]] || usage; check; push_one "$2" "${3:-$version_default}" ;;
  load) [[ $# -eq 2 ]] || usage; load_archive "$2" ;;
  images) check; nerdctl images "$image_prefix" ;;
  *) usage ;;
esac
