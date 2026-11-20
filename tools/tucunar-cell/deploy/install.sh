#!/usr/bin/env bash
set -euo pipefail

usage() {
  local status=2
  local stream=/dev/stderr
  if [[ "${1:-}" == "0" ]]; then
    status=0
    stream=/dev/stdout
    shift
  fi
  cat >"$stream" <<'EOF'
usage: install.sh [options]

Install the Tucunar cell files and, unless skipped, bootstrap a k0s
single-node controller using k0s' embedded containerd.

Options:
  --source DIR          Cell checkout or offline bundle (default: parent dir)
  --target DIR          Install directory (default: /opt/tucunar-cell)
  --profile NAME        k0s overlay: single-node or dev (default: single-node)
  --k0s-version VERSION Version to install when k0s is missing
  --k0s-bin PATH        k0s executable (default: k0s)
  --kubectl-bin PATH    kubectl executable (default: kubectl)
  --skip-k0s-install    Fail instead of downloading k0s when it is missing
  --skip-apply          Prepare files and k0s, but do not apply the overlay
  --dry-run             Validate and print actions without changing the host
  -h, --help            Show this help

Environment:
  TUCUNAR_INSTALL_TIMEOUT  kubectl wait timeout (default: 180s)
  TUCUNAR_K0S_INSTALL_URL   k0s installer URL (default: https://get.k0s.sh)
EOF
  exit "$status"
}

log() { printf '[tucunar-install] %s\n' "$*"; }
warn() { printf '[tucunar-install] warning: %s\n' "$*" >&2; }
die() { printf '[tucunar-install] error: %s\n' "$*" >&2; exit 1; }

source_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
target_dir="/opt/tucunar-cell"
profile="single-node"
k0s_version="${K0S_VERSION:-}"
k0s_bin="${K0S_BIN:-k0s}"
kubectl_bin="${KUBECTL_BIN:-kubectl}"
skip_k0s_install=0
skip_apply=0
dry_run=0
install_timeout="${TUCUNAR_INSTALL_TIMEOUT:-180s}"
k0s_install_url="${TUCUNAR_K0S_INSTALL_URL:-https://get.k0s.sh}"

while (($#)); do
  case "$1" in
    --source) [[ $# -ge 2 ]] || usage; source_dir="$2"; shift 2 ;;
    --target) [[ $# -ge 2 ]] || usage; target_dir="$2"; shift 2 ;;
    --profile) [[ $# -ge 2 ]] || usage; profile="$2"; shift 2 ;;
    --k0s-version) [[ $# -ge 2 ]] || usage; k0s_version="$2"; shift 2 ;;
    --k0s-bin) [[ $# -ge 2 ]] || usage; k0s_bin="$2"; shift 2 ;;
    --kubectl-bin) [[ $# -ge 2 ]] || usage; kubectl_bin="$2"; shift 2 ;;
    --skip-k0s-install) skip_k0s_install=1; shift ;;
    --skip-apply) skip_apply=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    -h|--help) usage 0 ;;
    *) usage ;;
  esac
done

source_dir="$(cd -- "$source_dir" 2>/dev/null && pwd)" || die "source directory not found"
target_parent="$(dirname -- "$target_dir")"
if [[ -d "$target_parent" ]]; then
  target_dir="$(cd -- "$target_parent" && pwd)/$(basename -- "$target_dir")"
else
  ((dry_run)) && die "target parent does not exist in dry-run mode: $target_parent"
  target_dir="$target_parent/$(basename -- "$target_dir")"
fi
profile_dir="$source_dir/deploy/k0s/overlays/$profile"
target_profile_dir="$target_dir/deploy/k0s/overlays/$profile"
config_source="$source_dir/deploy/k0s/k0s.yaml"
config_target="/etc/k0s/tucunar-cell.yaml"

run() {
  if ((dry_run)); then
    printf '[dry-run]'
    printf ' %q' "$@"
    printf '\n'
  else
    "$@"
  fi
}

require_file() { [[ -f "$1" ]] || die "missing required file: $1"; }
require_dir() { [[ -d "$1" ]] || die "missing required directory: $1"; }
require_command() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

copy_file() {
  local mode="$1" source="$2" destination="$3"
  if [[ "$source" == "$destination" ]]; then
    run chmod "$mode" "$destination"
  else
    run install -m "$mode" "$source" "$destination"
  fi
}

copy_tree() {
  local source="$1" destination_dir="$2"
  [[ "$source" == "$destination_dir/$(basename -- "$source")" ]] ||
    run cp -a "$source" "$destination_dir/"
}

validate_source() {
  [[ "$profile" == "dev" || "$profile" == "single-node" ]] || die "unsupported profile: $profile"
  require_file "$source_dir/Containerfile"
  require_file "$source_dir/go.mod"
  require_file "$source_dir/deploy/k0s/k0s.yaml"
  require_file "$source_dir/deploy/k0s/base/kustomization.yaml"
  require_dir "$profile_dir"
  require_file "$profile_dir/kustomization.yaml"
  require_file "$source_dir/scripts/validate.sh"
}

check_host() {
  [[ "$(uname -s)" == "Linux" ]] || die "Tucunar Cell requires Linux"
  if ((dry_run == 0 && EUID != 0)); then
    die "run the installer as root (for example: sudo $0 ...)"
  fi

  local cpu_count memory_kib available_kib target_parent network_state
  cpu_count="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0)"
  if [[ "$cpu_count" =~ ^[0-9]+$ ]] && ((cpu_count < 2)); then
    die "at least 2 CPUs are required; found $cpu_count"
  fi
  memory_kib="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  if [[ "$memory_kib" =~ ^[0-9]+$ ]] && ((memory_kib < 3500000)); then
    die "at least 4 GiB RAM is required; found $((memory_kib / 1024)) MiB"
  fi
  target_parent="$(dirname -- "$target_dir")"
  if [[ -d "$target_parent" ]]; then
    available_kib="$(df -Pk "$target_parent" | awk 'NR==2 {print $4}')"
    if [[ "$available_kib" =~ ^[0-9]+$ ]] && ((available_kib < 25000000)); then
      die "at least 25 GiB free disk is required at $target_parent"
    fi
  fi
  network_state="unknown"
  if [[ -r /proc/net/route ]]; then
    if awk '$2 == "00000000" { found = 1 } END { exit(found ? 0 : 1) }' /proc/net/route; then
      network_state="default-route"
    else
      network_state="no-default-route"
      if ((skip_k0s_install == 0)) && ! command -v "$k0s_bin" >/dev/null 2>&1; then
        warn "no default route detected; downloading k0s or pulling images may fail"
      fi
    fi
  fi
  log "host preflight passed (cpu=${cpu_count:-unknown}, memory_mib=$((memory_kib / 1024)), network=$network_state)"
}

copy_payload() {
  log "installing cell payload at $target_dir"
  run install -d -m 0755 "$target_dir" "$target_dir/deploy" "$target_dir/scripts" "$target_dir/docs"
  local file directory
  for file in Containerfile schema.sql go.mod go.sum dashboard.html; do
    [[ -f "$source_dir/$file" ]] && copy_file 0644 "$source_dir/$file" "$target_dir/$file"
  done
  for file in "$source_dir"/*.go "$source_dir"/*.json; do
    [[ -f "$file" ]] && copy_file 0644 "$file" "$target_dir/$(basename -- "$file")"
  done
  for directory in traffic-generator benchmark; do
    if [[ -d "$source_dir/$directory" ]]; then
      run install -d -m 0755 "$target_dir/$directory"
      for file in "$source_dir/$directory"/*.go; do
        [[ -f "$file" ]] && copy_file 0644 "$file" "$target_dir/$directory/$(basename -- "$file")"
      done
    fi
  done
  if [[ -d "$source_dir/cmd" ]]; then
    copy_tree "$source_dir/cmd" "$target_dir"
  fi
  for file in "$source_dir"/deploy/*.md "$source_dir"/deploy/*.sh; do
    [[ -f "$file" ]] || continue
    local mode=0644
    [[ "$file" == *.sh ]] && mode=0755
    copy_file "$mode" "$file" "$target_dir/deploy/$(basename -- "$file")"
  done
  if [[ -f "$source_dir/deploy/.env.example" ]]; then
    copy_file 0644 "$source_dir/deploy/.env.example" "$target_dir/deploy/.env.example"
  elif [[ -f "$source_dir/.env.example" ]]; then
    copy_file 0644 "$source_dir/.env.example" "$target_dir/deploy/.env.example"
  fi
  if [[ -d "$source_dir/deploy/k0s" ]]; then
    copy_tree "$source_dir/deploy/k0s" "$target_dir/deploy"
  fi
  for file in "$source_dir"/deploy/*.service.example "$source_dir"/deploy/*.timer.example; do
    [[ -f "$file" ]] && copy_file 0644 "$file" "$target_dir/deploy/$(basename -- "$file")"
  done
  for file in "$source_dir"/scripts/*.sh; do
    [[ -f "$file" ]] && copy_file 0755 "$file" "$target_dir/scripts/$(basename -- "$file")"
  done
  for file in "$source_dir"/docs/*.md; do
    [[ -f "$file" ]] && copy_file 0644 "$file" "$target_dir/docs/$(basename -- "$file")"
  done
}

prepare_secrets() {
  [[ "$profile" == "dev" ]] && return 0
  local secret_file="$target_profile_dir/secret.env"
  local example_file="$profile_dir/secret.env.example"
  require_file "$example_file"
  if [[ ! -f "$secret_file" ]]; then
    log "creating $secret_file from the example; fill it before applying the cell"
    run install -m 0600 "$example_file" "$secret_file"
  else
    run chmod 0600 "$secret_file"
  fi
  if ((dry_run == 0 && skip_apply == 0)); then
    local key value
    for key in CLICKHOUSE_PASSWORD SIGNAL_INGEST_TOKEN CANARY_NODE_API_TOKEN TUCUNAR_INTERNAL_TOKEN; do
      value="$(sed -n "s/^${key}=//p" "$secret_file" | tail -n 1)"
      [[ -n "$value" && "$value" != replace-* && "$value" != CHANGE_ME* ]] ||
        die "$key is missing or still a placeholder in $secret_file"
    done
  fi
}

install_k0s() {
  if command -v "$k0s_bin" >/dev/null 2>&1; then
    log "using existing k0s: $(command -v "$k0s_bin")"
    return 0
  fi
  ((skip_k0s_install == 0)) || die "k0s is missing; provide --k0s-bin or remove --skip-k0s-install"
  [[ -n "$k0s_version" ]] || die "k0s is missing; provide --k0s-version for a reproducible install"
  require_command curl
  if ((dry_run)); then
    log "would install k0s $k0s_version from $k0s_install_url"
    return 0
  fi
  local temp_dir installer
  temp_dir="$(mktemp -d)"
  trap 'rm -rf -- "$temp_dir"' RETURN
  installer="$temp_dir/get-k0s.sh"
  curl --proto '=https' --tlsv1.2 --fail --silent --show-error \
    --location "$k0s_install_url" --output "$installer"
  chmod 0755 "$installer"
  K0S_VERSION="$k0s_version" "$installer"
  command -v "$k0s_bin" >/dev/null 2>&1 || die "k0s installer completed but k0s is not on PATH"
  trap - RETURN
  rm -rf -- "$temp_dir"
}

install_k0s_config() {
  require_file "$config_source"
  if ((dry_run)); then
    log "would install k0s config at $config_target"
    return 0
  fi
  install -d -m 0755 "$(dirname -- "$config_target")"
  if [[ -f "$config_target" ]] && ! cmp -s "$config_source" "$config_target"; then
    die "$config_target already exists with different contents; review it or move it aside explicitly"
  fi
  install -m 0644 "$config_source" "$config_target"
}

bootstrap_controller() {
  if ((dry_run)); then
    log "would ensure k0s controller is installed and running"
    return 0
  fi
  if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files k0scontroller.service >/dev/null 2>&1; then
    run systemctl enable --now k0scontroller
  else
    run "$k0s_bin" install controller --config "$config_target" --enable-worker --no-taints --start
  fi
}

configure_kubeconfig() {
  local operator_user operator_home kubeconfig temp_file uid gid deadline
  operator_user="${SUDO_USER:-${USER:-root}}"
  operator_home="$(getent passwd "$operator_user" 2>/dev/null | cut -d: -f6)"
  [[ -n "$operator_home" ]] || operator_home="/root"
  kubeconfig="${KUBECONFIG:-$operator_home/.kube/config}"
  if ((dry_run)); then
    log "would write admin kubeconfig to $kubeconfig"
    return 0
  fi
  require_command "$k0s_bin"
  uid="$(id -u "$operator_user")"
  gid="$(id -g "$operator_user")"
  install -d -m 0700 -o "$uid" -g "$gid" "$(dirname -- "$kubeconfig")"
  temp_file="$(mktemp)"
  deadline=$((SECONDS + 180))
  until "$k0s_bin" kubeconfig admin > "$temp_file" 2>/dev/null; do
    if ((SECONDS >= deadline)); then
      rm -f -- "$temp_file"
      die "k0s admin kubeconfig unavailable after 180s; inspect k0s status and controller logs"
    fi
    sleep 2
  done
  if [[ -f "$kubeconfig" ]] && ! cmp -s "$temp_file" "$kubeconfig"; then
    rm -f -- "$temp_file"
    die "$kubeconfig already exists with different contents; set KUBECONFIG to a new path explicitly"
  fi
  install -m 0600 -o "$uid" -g "$gid" "$temp_file" "$kubeconfig"
  rm -f -- "$temp_file"
  export KUBECONFIG="$kubeconfig"
}

validate_and_apply() {
  local overlay="$target_dir/deploy/k0s/overlays/$profile" kubectl_path kubectl_dir
  ((skip_apply == 0)) || { log "overlay application skipped by request"; return 0; }
  if ((dry_run)); then
    log "would validate and apply k0s overlay $overlay"
    log "would wait for statefulset/clickhouse and deployments swift, canary-evaluator, condor, hummingbird"
    return 0
  fi
  require_command "$kubectl_bin"
  kubectl_path="$(command -v "$kubectl_bin")"
  kubectl_dir="$(dirname -- "$kubectl_path")"
  CANARY_K0S_PROFILE="$profile" CANARY_ENV_FILE="$overlay/secret.env" \
    PATH="$kubectl_dir:$PATH" "$target_dir/scripts/validate.sh"
  run "$kubectl_bin" apply -k "$overlay"
  run "$kubectl_bin" -n tucunar-cell rollout status statefulset/clickhouse --timeout="$install_timeout"
  local deployment
  for deployment in swift canary-evaluator condor hummingbird; do
    run "$kubectl_bin" -n tucunar-cell rollout status "deployment/$deployment" --timeout="$install_timeout"
  done
  run "$kubectl_bin" -n tucunar-cell get pods
}

write_state() {
  ((dry_run)) && return 0
  install -m 0644 /dev/null "$target_dir/install-state.env"
  printf 'profile=%s\nsupervisor=k0s\ncontainerd_socket=/run/k0s/containerd.sock\ncloud_direction=outbound-only\nsource=%s\nconfig=%s\ninstalled_at=%s\n' \
    "$profile" "$source_dir" "$config_target" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$target_dir/install-state.env"
}

main() {
  validate_source
  check_host
  if ((dry_run == 0)); then
    require_command install
    require_command cp
    require_command chmod
  fi
  copy_payload
  prepare_secrets
  install_k0s
  install_k0s_config
  bootstrap_controller
  configure_kubeconfig
  validate_and_apply
  write_state
  log "installation complete: $target_dir (profile=$profile)"
  if [[ "$profile" != "dev" && -f "$target_profile_dir/secret.env" ]]; then
    log "secret file: $target_profile_dir/secret.env (mode 600; never commit it)"
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
