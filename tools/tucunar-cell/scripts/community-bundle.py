#!/usr/bin/env python3
"""Export, sign, verify and import offline Linux OCI bundles for Tucunar Cell Community."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

SEMVER_RE = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$")


def load_release_config():
    cell = Path(__file__).resolve().parent.parent
    config = json.loads((cell / "community-release.json").read_text())
    default_version = (cell / config["version_file"]).read_text().strip()
    return cell, config, default_version


def validate_version(version):
    if not SEMVER_RE.fullmatch(version):
        raise ValueError(f"version must be an explicit semantic version, got {version!r}")
    return version


def sha256_file(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def expected_images(config, version):
    prefix = config["image_prefix"]
    images = []
    for service in config["services"]:
        images.append({
            "role": "service",
            "service": service,
            "ref": f"{prefix}/{service}:{version}",
        })
    images.append({
        "role": "database",
        "service": "clickhouse",
        "ref": config["clickhouse_image"],
    })
    return images


def normalize_ref_candidates(ref):
    """Return containerd/OCI tag variants for matching exported image annotations."""
    candidates = {ref}
    if "/" not in ref.split(":")[0]:
        candidates.add(f"docker.io/library/{ref}")
    elif ref.count("/") == 1 and "." not in ref.split("/")[0]:
        candidates.add(f"docker.io/{ref}")
    return candidates


def inspect_tar_image_refs(archive_path):
    """Extract image references recorded in OCI index.json and/or Docker manifest.json."""
    found = set()
    with tarfile.open(archive_path, "r:*") as bundle:
        names = set(bundle.getnames())
        if "index.json" in names:
            index = json.loads(bundle.extractfile("index.json").read().decode())
            for desc in index.get("manifests", []):
                annotations = desc.get("annotations") or {}
                for key in ("io.containerd.image.name", "org.opencontainers.image.ref.name"):
                    value = annotations.get(key)
                    if value:
                        found.add(value)
        if "manifest.json" in names:
            entries = json.loads(bundle.extractfile("manifest.json").read().decode())
            for entry in entries:
                for tag in entry.get("RepoTags") or []:
                    found.add(tag)
    return found


def query_containerd_digests(k0s_bin, containerd_namespace):
    """Return {image_name: digest} from k0s containerd."""
    result = subprocess.run(
        [k0s_bin, "ctr", "--namespace", containerd_namespace, "images", "ls", "-q"],
        capture_output=True, text=True, check=True,
    )
    # Full table has columns: REF TYPE DIGEST SIZE PLATFORMS LABELS
    table = subprocess.run(
        [k0s_bin, "ctr", "--namespace", containerd_namespace, "images", "ls"],
        capture_output=True, text=True, check=True,
    )
    digests = {}
    for line in table.stdout.splitlines()[1:]:
        parts = line.split()
        if len(parts) >= 3:
            digests[parts[0]] = parts[2]
    for name in result.stdout.splitlines():
        digests.setdefault(name.strip(), "")
    return digests


def sign_file(file_path, key_path, namespace):
    sig_path = Path(str(file_path) + ".sig")
    subprocess.run(
        ["ssh-keygen", "-Y", "sign", "-f", str(key_path), "-n", namespace, str(file_path)],
        check=True,
    )
    if not sig_path.exists():
        raise RuntimeError(f"expected signature file was not created: {sig_path}")
    return sig_path


def verify_signature(file_path, sig_path, allowed_signers, namespace, principal=None):
    if not sig_path.exists():
        raise RuntimeError(f"missing signature file: {sig_path}")
    if not allowed_signers.exists():
        raise RuntimeError(f"missing allowed_signers file: {allowed_signers}")
    if not principal:
        for line in allowed_signers.read_text().splitlines():
            stripped = line.strip()
            if stripped and not stripped.startswith("#"):
                principal = stripped.split()[0]
                break
    if not principal:
        raise RuntimeError(f"no signer principal found in {allowed_signers}")
    with Path(file_path).open("rb") as payload:
        result = subprocess.run(
            [
                "ssh-keygen", "-Y", "verify",
                "-f", str(allowed_signers),
                "-I", principal,
                "-n", namespace,
                "-s", str(sig_path),
            ],
            stdin=payload, capture_output=True, text=True,
        )
    if result.returncode != 0:
        raise RuntimeError(
            f"signature verification failed for {file_path.name}: {result.stderr.strip() or result.stdout.strip()}"
        )


def write_sha256_file(target_path):
    digest = sha256_file(target_path)
    sha_path = target_path.with_name(target_path.name + ".sha256")
    sha_path.write_text(f"{digest}  {target_path.name}\n")
    return digest, sha_path


def verify_sha256_file(target_path, sha_path):
    if not sha_path.exists():
        raise RuntimeError(f"missing checksum file: {sha_path}")
    line = sha_path.read_text().strip()
    parts = line.split("  ", 1)
    if len(parts) != 2 or parts[1] != target_path.name:
        raise RuntimeError(f"invalid checksum file format in {sha_path}")
    actual = sha256_file(target_path)
    if actual != parts[0]:
        raise RuntimeError(f"checksum mismatch for {target_path.name}: expected {parts[0]}, got {actual}")
    return actual


def cmd_export(args):
    cell, config, default_version = load_release_config()
    version = validate_version(args.version or default_version)
    platform = args.platform
    if platform not in config["platforms"]:
        raise ValueError(f"unsupported platform {platform!r}; release manifest lists {config['platforms']}")
    output_dir = args.output.resolve()
    if output_dir.exists():
        raise FileExistsError(f"output directory already exists: {output_dir}")
    output_dir.parent.mkdir(parents=True, exist_ok=True)

    images = expected_images(config, version)
    present = query_containerd_digests(args.k0s_bin, args.namespace)
    resolved_refs = []
    manifest_images = []
    for entry in images:
        matched_name = None
        for candidate in normalize_ref_candidates(entry["ref"]):
            if candidate in present:
                matched_name = candidate
                break
        if not matched_name:
            raise RuntimeError(
                f"required image missing from k0s containerd ({args.namespace}): {entry['ref']}"
            )
        resolved_refs.append(matched_name)
        manifest_images.append({
            "role": entry["role"],
            "service": entry["service"],
            "ref": entry["ref"],
            "containerd_ref": matched_name,
            "digest": present[matched_name],
        })

    plat_slug = platform.replace("/", "-")
    archive_name = f"tucunar-cell-images-{plat_slug}-{version}.oci.tar"
    manifest_name = f"tucunar-cell-release-manifest-{version}.json"
    temporary = Path(tempfile.mkdtemp(prefix=".cell-oci-bundle-", dir=output_dir.parent))
    try:
        archive_path = temporary / archive_name
        subprocess.run(
            [args.k0s_bin, "ctr", "--namespace", args.namespace, "images", "export", str(archive_path), *resolved_refs],
            check=True,
        )
        archive_sha, archive_sha_path = write_sha256_file(archive_path)
        manifest = {
            "manifest_version": 1,
            "product": "tucunar-cell-community",
            "license": "Apache-2.0",
            "repository": config.get("repository", "tucunar-labs/cell"),
            "version": version,
            "source_commit": args.commit or "unreleased-local",
            "contract_major": config["contract_major"],
            "schema_compatibility": config.get("schema_compatibility", 1),
            "platform": platform,
            "validated_platforms": list(config["platforms"]),
            "signature_namespace": config.get("signature_namespace", "tucunar-cell-release"),
            "created_at": datetime.now(timezone.utc).isoformat(),
            "archive": {
                "filename": archive_name,
                "sha256": archive_sha,
                "size_bytes": archive_path.stat().st_size,
            },
            "images": manifest_images,
            "tools": {name: {"version": t["version"], "sha256": t["sha256"]} for name, t in config["tools"].items()},
            "desktop_tools": {
                name: {"version": t["version"], "sha256": t["sha256"]}
                for name, t in config.get("desktop_tools", {}).items()
            },
        }
        manifest_path = temporary / manifest_name
        manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")
        _, manifest_sha_path = write_sha256_file(manifest_path)

        if args.signing_key:
            sig_ns = manifest["signature_namespace"]
            sign_file(manifest_path, args.signing_key, sig_ns)
            sign_file(archive_sha_path, args.signing_key, sig_ns)
            if args.allowed_signers:
                shutil.copyfile(args.allowed_signers, temporary / "allowed_signers")
            elif Path(str(args.signing_key) + ".pub").exists():
                pub = Path(str(args.signing_key) + ".pub").read_text().strip()
                principal = args.signer_principal or "release@tucunar.com"
                (temporary / "allowed_signers").write_text(f"{principal} {pub}\n")

        temporary.rename(output_dir)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)

    print(f"Exported offline OCI bundle to {output_dir}")
    print(f"Archive: {output_dir / archive_name} (sha256={archive_sha})")
    print(f"Manifest: {output_dir / manifest_name}")


def verify_bundle(bundle_dir, allowed_signers=None, require_signature=False, principal=None, expected_version=None):
    _, config, _ = load_release_config()
    bundle_dir = Path(bundle_dir).resolve()
    manifests = sorted(bundle_dir.glob("tucunar-cell-release-manifest-*.json"))
    manifests = [m for m in manifests if not m.name.endswith(".sha256") and not m.name.endswith(".sig")]
    if len(manifests) != 1:
        raise RuntimeError(f"expected exactly 1 release manifest in {bundle_dir}, found {len(manifests)}")
    manifest_path = manifests[0]
    manifest_sha_path = manifest_path.with_name(manifest_path.name + ".sha256")
    verify_sha256_file(manifest_path, manifest_sha_path)

    manifest = json.loads(manifest_path.read_text())
    version = validate_version(manifest["version"])
    if expected_version and version != expected_version:
        raise RuntimeError(f"manifest version {version} does not match expected {expected_version}")
    if manifest.get("contract_major") != config["contract_major"]:
        raise RuntimeError("manifest contract_major mismatch")

    archive_info = manifest["archive"]
    archive_path = bundle_dir / archive_info["filename"]
    if not archive_path.exists():
        raise RuntimeError(f"missing OCI archive: {archive_path}")
    archive_sha_path = archive_path.with_name(archive_path.name + ".sha256")
    actual_sha = verify_sha256_file(archive_path, archive_sha_path)
    if actual_sha != archive_info["sha256"]:
        raise RuntimeError(
            f"manifest archive sha256 mismatch: manifest={archive_info['sha256']} actual={actual_sha}"
        )

    # Verify all required images exist in both manifest and the actual OCI archive.
    required = expected_images(config, version)
    manifest_refs = {entry["ref"] for entry in manifest.get("images", [])}
    for entry in required:
        if entry["ref"] not in manifest_refs:
            raise RuntimeError(f"manifest missing required image: {entry['ref']}")
        if entry["ref"].endswith(":latest"):
            raise RuntimeError(f"forbidden mutable :latest tag: {entry['ref']}")

    tar_refs = inspect_tar_image_refs(archive_path)
    for entry in required:
        if not (normalize_ref_candidates(entry["ref"]) & tar_refs):
            raise RuntimeError(
                f"OCI archive missing required image {entry['ref']} (archive contains {sorted(tar_refs)})"
            )

    sig_ns = manifest.get("signature_namespace", config.get("signature_namespace", "tucunar-cell-release"))
    signers_path = Path(allowed_signers) if allowed_signers else (bundle_dir / "allowed_signers")
    manifest_sig = manifest_path.with_name(manifest_path.name + ".sig")
    archive_sha_sig = archive_sha_path.with_name(archive_sha_path.name + ".sig")

    signature_verified = False
    if require_signature or allowed_signers or manifest_sig.exists() or archive_sha_sig.exists():
        verify_signature(manifest_path, manifest_sig, signers_path, sig_ns, principal=principal)
        verify_signature(archive_sha_path, archive_sha_sig, signers_path, sig_ns, principal=principal)
        signature_verified = True

    return manifest, archive_path, signature_verified


def cmd_verify(args):
    manifest, archive_path, sig_ok = verify_bundle(
        args.bundle_dir,
        allowed_signers=args.allowed_signers,
        require_signature=args.require_signature,
        principal=args.signer_principal,
        expected_version=args.version,
    )
    sig_note = "cryptographic signature VERIFIED" if sig_ok else "checksums only (unsigned)"
    print(
        f"PASS offline bundle verification: version={manifest['version']} "
        f"platform={manifest['platform']} images={len(manifest['images'])} "
        f"archive={archive_path.name} ({sig_note})"
    )


def cmd_import(args):
    manifest, archive_path, sig_ok = verify_bundle(
        args.bundle_dir,
        allowed_signers=args.allowed_signers,
        require_signature=args.require_signature,
        principal=args.signer_principal,
        expected_version=args.version,
    )
    subprocess.run(
        [args.k0s_bin, "ctr", "--namespace", args.namespace, "images", "import", str(archive_path)],
        check=True,
    )
    present = query_containerd_digests(args.k0s_bin, args.namespace)
    missing = []
    for entry in manifest["images"]:
        if not (normalize_ref_candidates(entry["ref"]) & set(present.keys())):
            missing.append(entry["ref"])
    if missing:
        raise RuntimeError(f"imported archive but images missing in containerd: {missing}")
    sig_note = "signature verified" if sig_ok else "unsigned"
    print(
        f"PASS offline OCI import into k0s ({args.namespace}): "
        f"{len(manifest['images'])} images verified for version {manifest['version']} ({sig_note})"
    )


def cmd_sign(args):
    _, config, _ = load_release_config()
    namespace = args.signature_namespace or config.get("signature_namespace", "tucunar-cell-release")
    for target in args.files:
        sig = sign_file(Path(target).resolve(), Path(args.signing_key).resolve(), namespace)
        print(f"Signed {target} -> {sig}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    p_exp = sub.add_parser("export", help="export the 4 core images + ClickHouse from k0s containerd")
    p_exp.add_argument("output", type=Path, help="new destination directory for the offline bundle")
    p_exp.add_argument("--version", help="explicit semantic version (defaults to VERSION)")
    p_exp.add_argument("--platform", default="linux/amd64")
    p_exp.add_argument("--commit", default="", help="source commit SHA recorded in manifest")
    p_exp.add_argument("--k0s-bin", default="k0s")
    p_exp.add_argument("--namespace", default="k8s.io")
    p_exp.add_argument("--signing-key", type=Path, help="optional Ed25519 private key for ssh-keygen -Y sign")
    p_exp.add_argument("--allowed-signers", type=Path, help="optional allowed_signers file to bundle")
    p_exp.add_argument("--signer-principal", default="release@tucunar.com")
    p_exp.set_defaults(func=cmd_export)

    p_ver = sub.add_parser("verify", help="verify bundle checksums, OCI contents and optional signature offline")
    p_ver.add_argument("bundle_dir", type=Path)
    p_ver.add_argument("--version", help="require exact manifest version")
    p_ver.add_argument("--allowed-signers", type=Path, help="trusted allowed_signers file")
    p_ver.add_argument("--require-signature", action="store_true")
    p_ver.add_argument("--signer-principal", default="")
    p_ver.set_defaults(func=cmd_verify)

    p_imp = sub.add_parser("import", help="verify offline bundle and import images into k0s containerd")
    p_imp.add_argument("bundle_dir", type=Path)
    p_imp.add_argument("--version", help="require exact manifest version")
    p_imp.add_argument("--allowed-signers", type=Path)
    p_imp.add_argument("--require-signature", action="store_true")
    p_imp.add_argument("--signer-principal", default="")
    p_imp.add_argument("--k0s-bin", default="k0s")
    p_imp.add_argument("--namespace", default="k8s.io")
    p_imp.set_defaults(func=cmd_import)

    p_sign = sub.add_parser("sign", help="sign individual release artifacts with ssh-keygen -Y")
    p_sign.add_argument("--signing-key", required=True, type=Path)
    p_sign.add_argument("--signature-namespace", default="")
    p_sign.add_argument("files", nargs="+", type=Path)
    p_sign.set_defaults(func=cmd_sign)

    args = parser.parse_args()
    try:
        args.func(args)
    except Exception as error:
        print(f"ERROR: {error}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
