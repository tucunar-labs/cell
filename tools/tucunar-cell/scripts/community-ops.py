#!/usr/bin/env python3
"""Consistent quiesced backup, preflighted restore, upgrade and rollback for Community cells."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import shutil
import socket
import subprocess
import sys
import tarfile
import time
from urllib.request import urlopen

SEMVER_RE = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$")
CORE_TABLES = (
    "canary_signal_events",
    "canary_incidents",
    "canary_incident_deliveries",
    "canary_cloud_outbox",
)


def load_release_config():
    cell = Path(__file__).resolve().parent.parent
    return json.loads((cell / "community-release.json").read_text())


def read_env_file(path):
    values = {}
    for line in Path(path).read_text().splitlines():
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            values[key] = value
    return values


def write_env_file(path, values):
    path = Path(path)
    path.touch(mode=0o600)
    path.chmod(0o600)
    path.write_text("".join(f"{k}={v}\n" for k, v in values.items()))


def sha256_file(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def run_cmd(cmd, capture=False, check=True):
    return subprocess.run(cmd, capture_output=capture, text=True, check=check)


def current_kube_context(kubectl_bin):
    res = run_cmd([kubectl_bin, "config", "current-context"], capture=True)
    return res.stdout.strip()


def load_instance(instance_dir):
    instance_dir = Path(instance_dir).resolve()
    instance_json = instance_dir / "instance.json"
    overlay_dir = instance_dir / "k0s/overlays/community"
    secret_env = overlay_dir / "secret.env"
    kustomization = overlay_dir / "kustomization.yaml"
    for required in (instance_json, secret_env, kustomization):
        if not required.exists():
            raise FileNotFoundError(f"missing expected Community instance file: {required}")
    meta = json.loads(instance_json.read_text())
    env = read_env_file(secret_env)
    return {
        "root": instance_dir,
        "instance_json": instance_json,
        "overlay_dir": overlay_dir,
        "secret_env": secret_env,
        "kustomization": kustomization,
        "meta": meta,
        "env": env,
    }


def query_table_counts(kubectl_bin, namespace, ch_user, ch_password, ch_db="default"):
    counts = {}
    for table in CORE_TABLES:
        query = f"SELECT count() FROM {ch_db}.{table} FORMAT TabSeparated"
        res = run_cmd(
            [
                kubectl_bin, "-n", namespace, "exec", "clickhouse-0", "--",
                "clickhouse-client", "--user", ch_user, "--password", ch_password,
                "--query", query,
            ],
            capture=True, check=False,
        )
        if res.returncode == 0:
            counts[table] = int(res.stdout.strip() or "0")
        else:
            # Table may not exist on a brand new empty DB.
            if "UNKNOWN_TABLE" in res.stderr or "doesn't exist" in res.stderr:
                counts[table] = 0
            else:
                raise RuntimeError(f"ClickHouse count query failed for {table}: {res.stderr.strip()}")
    return counts


def scale_services(kubectl_bin, namespace, services, replicas, timeout="180s"):
    for service in services:
        run_cmd([kubectl_bin, "-n", namespace, "scale", f"deployment/{service}", f"--replicas={replicas}"])
    if replicas == 0:
        for service in services:
            run_cmd([
                kubectl_bin, "-n", namespace, "wait",
                "--for=delete", "pod", "-l", f"app.kubernetes.io/name={service}",
                f"--timeout={timeout}",
            ], check=False)
    else:
        for service in services:
            run_cmd([kubectl_bin, "-n", namespace, "rollout", "status", f"deployment/{service}", f"--timeout={timeout}"])


def scale_clickhouse(kubectl_bin, namespace, replicas, timeout="180s"):
    run_cmd([kubectl_bin, "-n", namespace, "scale", "statefulset/clickhouse", f"--replicas={replicas}"])
    if replicas == 0:
        run_cmd([kubectl_bin, "-n", namespace, "wait", "--for=delete", "pod/clickhouse-0", f"--timeout={timeout}"], check=False)
    else:
        run_cmd([kubectl_bin, "-n", namespace, "rollout", "status", "statefulset/clickhouse", f"--timeout={timeout}"])


def cmd_backup(args):
    config = load_release_config()
    inst = load_instance(args.instance)
    namespace = inst["meta"].get("namespace", "tucunar-cell")
    services = config["services"]
    ch_password = inst["env"]["CLICKHOUSE_PASSWORD"]
    data_dir = Path(args.data_dir).resolve()
    if not data_dir.is_dir():
        raise FileNotFoundError(f"ClickHouse host data directory not found: {data_dir}")

    output_root = Path(args.output).resolve()
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    backup_dir = output_root / f"tucunar-cell-backup-{stamp}"
    backup_dir.mkdir(parents=True, exist_ok=False)

    pre_counts = query_table_counts(args.kubectl_bin, namespace, args.clickhouse_user, ch_password, args.clickhouse_db)
    quiesce_start = time.monotonic()
    try:
        scale_services(args.kubectl_bin, namespace, services, 0, timeout=args.timeout)
        run_cmd([
            args.kubectl_bin, "-n", namespace, "exec", "clickhouse-0", "--",
            "clickhouse-client", "--user", args.clickhouse_user, "--password", ch_password,
            "--query", "SYSTEM FLUSH LOGS",
        ])
        scale_clickhouse(args.kubectl_bin, namespace, 0, timeout=args.timeout)

        archive_start = time.monotonic()
        archive_path = backup_dir / f"clickhouse-data-{stamp}.tar.gz"
        run_cmd(["tar", "-czf", str(archive_path), "-C", str(data_dir), "."])
        archive_seconds = round(time.monotonic() - archive_start, 2)
    finally:
        # Always bring ClickHouse and core services back online.
        scale_clickhouse(args.kubectl_bin, namespace, 1, timeout=args.timeout)
        scale_services(args.kubectl_bin, namespace, services, 1, timeout=args.timeout)
    interruption_seconds = round(time.monotonic() - quiesce_start, 2)

    archive_sha = sha256_file(archive_path)
    (backup_dir / (archive_path.name + ".sha256")).write_text(f"{archive_sha}  {archive_path.name}\n")
    manifest = {
        "manifest_version": 1,
        "method": "quiesced-statefulset-snapshot",
        "created_at": datetime.now(timezone.utc).isoformat(),
        "version": inst["meta"]["version"],
        "schema_compatibility": config.get("schema_compatibility", 1),
        "organization_id": inst["meta"]["organization_id"],
        "cell_id": inst["meta"]["cell_id"],
        "namespace": namespace,
        "archive": archive_path.name,
        "archive_sha256": archive_sha,
        "archive_size_bytes": archive_path.stat().st_size,
        "row_counts": pre_counts,
        "archive_duration_seconds": archive_seconds,
        "write_interruption_seconds": interruption_seconds,
        "credentials_included": False,
    }
    manifest_path = backup_dir / "backup-manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")
    manifest_sha = sha256_file(manifest_path)
    (backup_dir / "backup-manifest.json.sha256").write_text(f"{manifest_sha}  backup-manifest.json\n")

    print(f"PASS consistent backup written to {backup_dir}")
    print(f"Row counts: {json.dumps(pre_counts)}")
    print(f"Observed archive_duration={archive_seconds}s write_interruption={interruption_seconds}s")


def verify_backup_dir(backup_dir):
    backup_dir = Path(backup_dir).resolve()
    manifest_path = backup_dir / "backup-manifest.json"
    manifest_sha_path = backup_dir / "backup-manifest.json.sha256"
    if not manifest_path.exists() or not manifest_sha_path.exists():
        raise FileNotFoundError(f"missing backup-manifest.json or checksum in {backup_dir}")
    expected_m_sha, m_name = manifest_sha_path.read_text().strip().split("  ", 1)
    if m_name != "backup-manifest.json" or sha256_file(manifest_path) != expected_m_sha:
        raise RuntimeError("backup-manifest.json checksum mismatch")
    manifest = json.loads(manifest_path.read_text())
    archive_path = backup_dir / manifest["archive"]
    archive_sha_path = backup_dir / (manifest["archive"] + ".sha256")
    if not archive_path.exists() or not archive_sha_path.exists():
        raise FileNotFoundError(f"missing backup archive or checksum in {backup_dir}")
    expected_a_sha, a_name = archive_sha_path.read_text().strip().split("  ", 1)
    actual_a_sha = sha256_file(archive_path)
    if a_name != archive_path.name or actual_a_sha != expected_a_sha or actual_a_sha != manifest["archive_sha256"]:
        raise RuntimeError("backup archive checksum mismatch")
    with tarfile.open(archive_path, "r:gz") as bundle:
        for member in bundle.getmembers():
            if member.name.startswith("/") or ".." in Path(member.name).parts:
                raise RuntimeError(f"unsafe path in backup archive: {member.name}")
    return manifest, archive_path


def preflight_restore(manifest, inst, actual_context, expect_context, expect_namespace, confirm_cell_id, allow_cross_cell=False):
    target_ns = inst["meta"].get("namespace", "tucunar-cell")
    target_cell_id = inst["meta"]["cell_id"]
    if actual_context != expect_context:
        raise RuntimeError(
            f"restore preflight refused: active kubectl context is {actual_context!r}, expected {expect_context!r}"
        )
    if target_ns != expect_namespace:
        raise RuntimeError(
            f"restore preflight refused: instance namespace is {target_ns!r}, expected {expect_namespace!r}"
        )
    if target_cell_id != confirm_cell_id:
        raise RuntimeError(
            f"restore preflight refused: target instance cell_id is {target_cell_id!r}, --confirm-cell-id was {confirm_cell_id!r}"
        )
    if manifest["cell_id"] != target_cell_id and not allow_cross_cell:
        raise RuntimeError(
            f"restore preflight refused: backup cell_id {manifest['cell_id']!r} differs from target {target_cell_id!r} "
            "(pass --allow-cross-cell only when intentionally restoring onto a separate instance with matching org/cell or after adopting identity)"
        )


def cmd_restore(args):
    config = load_release_config()
    manifest, archive_path = verify_backup_dir(args.backup_dir)
    inst = load_instance(args.instance)
    actual_context = current_kube_context(args.kubectl_bin)
    preflight_restore(
        manifest, inst, actual_context,
        expect_context=args.expect_context,
        expect_namespace=args.expect_namespace,
        confirm_cell_id=args.confirm_cell_id,
        allow_cross_cell=args.allow_cross_cell,
    )

    namespace = inst["meta"].get("namespace", "tucunar-cell")
    services = config["services"]
    ch_password = inst["env"]["CLICKHOUSE_PASSWORD"]
    data_dir = Path(args.data_dir).resolve()
    if not data_dir.is_dir():
        raise FileNotFoundError(f"target ClickHouse data directory not found: {data_dir}")

    # Refuse to clobber a non-empty database unless explicitly authorized.
    existing = query_table_counts(args.kubectl_bin, namespace, args.clickhouse_user, ch_password, args.clickhouse_db)
    if any(v > 0 for v in existing.values()) and not args.allow_nonempty:
        raise RuntimeError(
            f"restore preflight refused: target ClickHouse is not empty ({existing}); "
            "restore to a separate clean host or pass --allow-nonempty explicitly"
        )

    restore_start = time.monotonic()
    scale_services(args.kubectl_bin, namespace, services, 0, timeout=args.timeout)
    scale_clickhouse(args.kubectl_bin, namespace, 0, timeout=args.timeout)

    for child in data_dir.iterdir():
        if child.is_dir() and not child.is_symlink():
            shutil.rmtree(child)
        else:
            child.unlink()
    run_cmd(["tar", "-xzf", str(archive_path), "-C", str(data_dir)])

    scale_clickhouse(args.kubectl_bin, namespace, 1, timeout=args.timeout)
    scale_services(args.kubectl_bin, namespace, services, 1, timeout=args.timeout)
    restore_seconds = round(time.monotonic() - restore_start, 2)

    recovered = query_table_counts(args.kubectl_bin, namespace, args.clickhouse_user, ch_password, args.clickhouse_db)
    if recovered != manifest["row_counts"]:
        raise RuntimeError(f"restored row counts {recovered} do not match backup manifest {manifest['row_counts']}")

    print(f"PASS verified restore into {namespace} (cell_id={inst['meta']['cell_id']}) in {restore_seconds}s")
    print(f"Recovered row counts match backup manifest: {json.dumps(recovered)}")


def rewrite_kustomization_tags(kustomization_text, image_prefix, services, target_version):
    lines = kustomization_text.splitlines()
    out = []
    i = 0
    updated = set()
    service_images = {f"{image_prefix}/{s}": s for s in services}
    while i < len(lines):
        line = lines[i]
        out.append(line)
        stripped = line.strip()
        if stripped.startswith("- name: "):
            img_name = stripped[len("- name: "):].strip()
            if img_name in service_images and i + 1 < len(lines) and lines[i + 1].strip().startswith("newTag:"):
                indent = lines[i + 1][: len(lines[i + 1]) - len(lines[i + 1].lstrip())]
                out.append(f"{indent}newTag: {target_version}")
                updated.add(service_images[img_name])
                i += 2
                continue
        i += 1
    missing = set(services) - updated
    if missing:
        raise RuntimeError(f"kustomization.yaml did not contain image tag overrides for: {sorted(missing)}")
    return "\n".join(out) + "\n"


def save_instance_snapshot(inst, schema_compatibility):
    history_root = inst["root"] / "history"
    history_root.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    prev_version = inst["meta"]["version"]
    snap_dir = history_root / f"{stamp}-{prev_version}"
    snap_dir.mkdir(parents=True, exist_ok=False)
    shutil.copy2(inst["kustomization"], snap_dir / "kustomization.yaml")
    shutil.copy2(inst["secret_env"], snap_dir / "secret.env")
    (snap_dir / "secret.env").chmod(0o600)
    shutil.copy2(inst["instance_json"], snap_dir / "instance.json")
    (snap_dir / "snapshot-meta.json").write_text(json.dumps({
        "saved_at": datetime.now(timezone.utc).isoformat(),
        "version": prev_version,
        "schema_compatibility": schema_compatibility,
        "organization_id": inst["meta"]["organization_id"],
        "cell_id": inst["meta"]["cell_id"],
    }, indent=2) + "\n")
    return snap_dir


def verify_local_images_present(k0s_bin, containerd_namespace, image_prefix, services, version):
    res = run_cmd([k0s_bin, "ctr", "--namespace", containerd_namespace, "images", "ls", "-q"], capture=True)
    present = {line.strip() for line in res.stdout.splitlines() if line.strip()}
    missing = [f"{image_prefix}/{s}:{version}" for s in services if f"{image_prefix}/{s}:{version}" not in present]
    if missing:
        raise RuntimeError(
            f"target images not present in k0s containerd ({containerd_namespace}): {missing}; "
            "build or import the OCI bundle before applying"
        )


def apply_version_to_instance(inst, config, target_version):
    if not SEMVER_RE.fullmatch(target_version):
        raise ValueError(f"target version must be an explicit semantic version, got {target_version!r}")
    new_kust = rewrite_kustomization_tags(
        inst["kustomization"].read_text(),
        config["image_prefix"],
        config["services"],
        target_version,
    )
    env = dict(inst["env"])
    env["CANARY_NODE_VERSION"] = target_version
    meta = dict(inst["meta"])
    meta["version"] = target_version
    meta["schema_compatibility"] = config.get("schema_compatibility", 1)

    inst["kustomization"].write_text(new_kust)
    write_env_file(inst["secret_env"], env)
    inst["instance_json"].write_text(json.dumps(meta, indent=2) + "\n")


def pick_free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def probe_service_via_port_forward(kubectl_bin, namespace, service):
    """Query /version and /readyz via loopback port-forward (works under default-deny NetworkPolicy)."""
    port = pick_free_port()
    proc = subprocess.Popen(
        [kubectl_bin, "-n", namespace, "port-forward", "--address=127.0.0.1", f"service/{service}", f"{port}:8080"],
        stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True,
    )
    try:
        deadline = time.monotonic() + 15
        last_error = None
        while time.monotonic() < deadline:
            if proc.poll() is not None:
                stderr = proc.stderr.read() if proc.stderr else ""
                raise RuntimeError(f"port-forward for {service} exited early: {stderr.strip()}")
            try:
                with urlopen(f"http://127.0.0.1:{port}/version", timeout=3) as resp:
                    version_payload = json.loads(resp.read().decode())
                with urlopen(f"http://127.0.0.1:{port}/readyz", timeout=3) as resp:
                    ready_body = resp.read().decode().strip()
                return version_payload, ready_body
            except Exception as err:
                last_error = err
                time.sleep(0.5)
        raise RuntimeError(f"timed out probing {service} via port-forward: {last_error}")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)


def check_live_service_versions(kubectl_bin, namespace, services, expected_version):
    """Verify each Deployment runs the expected container image tag and reports expected_version on /version."""
    for service in services:
        image_res = run_cmd([
            kubectl_bin, "-n", namespace, "get", f"deployment/{service}",
            "-o", "jsonpath={.spec.template.spec.containers[0].image}",
        ], capture=True)
        actual_image = image_res.stdout.strip()
        if not actual_image.endswith(f":{expected_version}"):
            raise RuntimeError(f"deployment/{service} image {actual_image!r} does not match :{expected_version}")
        payload, ready_body = probe_service_via_port_forward(kubectl_bin, namespace, service)
        reported = payload.get("version") or payload.get("node_version")
        if reported != expected_version:
            raise RuntimeError(f"service {service} /version reported {reported!r}, expected {expected_version!r}")
        if not ready_body:
            raise RuntimeError(f"service {service} /readyz returned empty response")


def cmd_upgrade(args):
    config = load_release_config()
    inst = load_instance(args.instance)
    if not SEMVER_RE.fullmatch(args.target_version):
        raise ValueError(f"invalid target version: {args.target_version}")
    if args.require_local_images:
        verify_local_images_present(
            args.k0s_bin, args.containerd_namespace,
            config["image_prefix"], config["services"], args.target_version,
        )
    snap_dir = save_instance_snapshot(inst, inst["meta"].get("schema_compatibility", config.get("schema_compatibility", 1)))
    apply_version_to_instance(inst, config, args.target_version)

    namespace = inst["meta"].get("namespace", "tucunar-cell")
    run_cmd([args.kubectl_bin, "apply", "-k", str(inst["overlay_dir"])])
    for service in config["services"]:
        run_cmd([args.kubectl_bin, "-n", namespace, "rollout", "status", f"deployment/{service}", f"--timeout={args.timeout}"])
    check_live_service_versions(args.kubectl_bin, namespace, config["services"], args.target_version)
    print(f"PASS upgraded cell to {args.target_version} (previous state preserved at {snap_dir})")


def validate_rollback_compatibility(snapshot_meta, current_schema_compat, after_restore=False):
    snap_compat = snapshot_meta.get("schema_compatibility", 1)
    if snap_compat != current_schema_compat and not after_restore:
        raise RuntimeError(
            f"binary-only rollback refused: snapshot schema_compatibility={snap_compat} != current {current_schema_compat}. "
            "Restore the pre-upgrade ClickHouse backup first, then re-run rollback with --after-restore."
        )


def cmd_rollback(args):
    config = load_release_config()
    inst = load_instance(args.instance)
    snap_dir = Path(args.to_snapshot).resolve()
    snap_meta = json.loads((snap_dir / "snapshot-meta.json").read_text())
    current_compat = inst["meta"].get("schema_compatibility", config.get("schema_compatibility", 1))
    validate_rollback_compatibility(snap_meta, current_compat, after_restore=args.after_restore)

    target_version = snap_meta["version"]
    if args.require_local_images:
        verify_local_images_present(
            args.k0s_bin, args.containerd_namespace,
            config["image_prefix"], config["services"], target_version,
        )
    shutil.copy2(snap_dir / "kustomization.yaml", inst["kustomization"])
    shutil.copy2(snap_dir / "secret.env", inst["secret_env"])
    inst["secret_env"].chmod(0o600)
    shutil.copy2(snap_dir / "instance.json", inst["instance_json"])

    namespace = inst["meta"].get("namespace", "tucunar-cell")
    run_cmd([args.kubectl_bin, "apply", "-k", str(inst["overlay_dir"])])
    for service in config["services"]:
        run_cmd([args.kubectl_bin, "-n", namespace, "rollout", "status", f"deployment/{service}", f"--timeout={args.timeout}"])
    check_live_service_versions(args.kubectl_bin, namespace, config["services"], target_version)
    print(f"PASS rolled back cell to {target_version} from snapshot {snap_dir}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    p_bak = sub.add_parser("backup", help="take a consistent quiesced ClickHouse backup with row-count manifest")
    p_bak.add_argument("--instance", required=True, type=Path)
    p_bak.add_argument("--output", required=True, type=Path)
    p_bak.add_argument("--data-dir", default="/var/lib/tucunar/clickhouse/data", type=Path)
    p_bak.add_argument("--kubectl-bin", default="kubectl")
    p_bak.add_argument("--clickhouse-user", default="default")
    p_bak.add_argument("--clickhouse-db", default="default")
    p_bak.add_argument("--timeout", default="180s")
    p_bak.set_defaults(func=cmd_backup)

    p_res = sub.add_parser("restore", help="preflight and restore a verified ClickHouse backup")
    p_res.add_argument("--backup-dir", required=True, type=Path)
    p_res.add_argument("--instance", required=True, type=Path)
    p_res.add_argument("--expect-context", required=True)
    p_res.add_argument("--expect-namespace", required=True)
    p_res.add_argument("--confirm-cell-id", required=True)
    p_res.add_argument("--allow-cross-cell", action="store_true")
    p_res.add_argument("--allow-nonempty", action="store_true")
    p_res.add_argument("--data-dir", default="/var/lib/tucunar/clickhouse/data", type=Path)
    p_res.add_argument("--kubectl-bin", default="kubectl")
    p_res.add_argument("--clickhouse-user", default="default")
    p_res.add_argument("--clickhouse-db", default="default")
    p_res.add_argument("--timeout", default="180s")
    p_res.set_defaults(func=cmd_restore)

    p_upg = sub.add_parser("upgrade", help="snapshot current instance, update to target version and verify /version")
    p_upg.add_argument("--instance", required=True, type=Path)
    p_upg.add_argument("--target-version", required=True)
    p_upg.add_argument("--require-local-images", action="store_true")
    p_upg.add_argument("--k0s-bin", default="k0s")
    p_upg.add_argument("--containerd-namespace", default="k8s.io")
    p_upg.add_argument("--kubectl-bin", default="kubectl")
    p_upg.add_argument("--timeout", default="180s")
    p_upg.set_defaults(func=cmd_upgrade)

    p_rb = sub.add_parser("rollback", help="roll back to a saved instance snapshot after checking schema compatibility")
    p_rb.add_argument("--instance", required=True, type=Path)
    p_rb.add_argument("--to-snapshot", required=True, type=Path)
    p_rb.add_argument("--after-restore", action="store_true")
    p_rb.add_argument("--require-local-images", action="store_true")
    p_rb.add_argument("--k0s-bin", default="k0s")
    p_rb.add_argument("--containerd-namespace", default="k8s.io")
    p_rb.add_argument("--kubectl-bin", default="kubectl")
    p_rb.add_argument("--timeout", default="180s")
    p_rb.set_defaults(func=cmd_rollback)

    args = parser.parse_args()
    try:
        args.func(args)
    except Exception as error:
        print(f"ERROR: {error}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
