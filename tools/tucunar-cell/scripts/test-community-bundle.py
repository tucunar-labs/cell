#!/usr/bin/env python3
"""Offline tests for OCI bundle manifest verification and OpenSSH signature checks."""
import importlib.util
import io
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parent / "community-bundle.py"
SPEC = importlib.util.spec_from_file_location("community_bundle", SCRIPT)
BUNDLE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BUNDLE)


def make_synthetic_oci_tar(path, refs):
    index = {
        "schemaVersion": 2,
        "manifests": [
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "digest": f"sha256:{idx:064x}",
                "size": 128,
                "annotations": {"io.containerd.image.name": ref},
            }
            for idx, ref in enumerate(refs, start=1)
        ],
    }
    raw = json.dumps(index).encode()
    with tarfile.open(path, "w") as archive:
        info = tarfile.TarInfo("index.json")
        info.size = len(raw)
        archive.addfile(info, io.BytesIO(raw))


class CommunityBundleTests(unittest.TestCase):
    def _build_signed_bundle(self, root, version="0.1.0-alpha.1", drop_service=None):
        _, config, _ = BUNDLE.load_release_config()
        bundle_dir = root / "bundle"
        bundle_dir.mkdir()
        refs = [e["ref"] for e in BUNDLE.expected_images(config, version) if e["service"] != drop_service]
        archive_path = bundle_dir / f"tucunar-cell-images-linux-amd64-{version}.oci.tar"
        make_synthetic_oci_tar(archive_path, refs)
        archive_sha, archive_sha_path = BUNDLE.write_sha256_file(archive_path)

        manifest = {
            "manifest_version": 1,
            "product": "tucunar-cell-community",
            "license": "Apache-2.0",
            "repository": "tucunar-labs/cell",
            "version": version,
            "source_commit": "0123456789abcdef",
            "contract_major": config["contract_major"],
            "schema_compatibility": config.get("schema_compatibility", 1),
            "platform": "linux/amd64",
            "validated_platforms": ["linux/amd64"],
            "signature_namespace": config["signature_namespace"],
            "archive": {
                "filename": archive_path.name,
                "sha256": archive_sha,
                "size_bytes": archive_path.stat().st_size,
            },
            "images": [
                {"role": e["role"], "service": e["service"], "ref": e["ref"], "digest": "sha256:test"}
                for e in BUNDLE.expected_images(config, version)
            ],
        }
        manifest_path = bundle_dir / f"tucunar-cell-release-manifest-{version}.json"
        manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")
        BUNDLE.write_sha256_file(manifest_path)

        key_path = root / "release_ed25519"
        subprocess.run(
            ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key_path)],
            check=True,
        )
        sig_ns = config["signature_namespace"]
        BUNDLE.sign_file(manifest_path, key_path, sig_ns)
        BUNDLE.sign_file(archive_sha_path, key_path, sig_ns)
        pub = Path(str(key_path) + ".pub").read_text().strip()
        allowed = bundle_dir / "allowed_signers"
        allowed.write_text(f"release@tucunar.com {pub}\n")
        return bundle_dir, archive_path, manifest_path, allowed

    def test_valid_signed_bundle_passes_and_tampering_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            bundle_dir, archive_path, manifest_path, allowed = self._build_signed_bundle(root)
            manifest, verified_archive, sig_ok = BUNDLE.verify_bundle(
                bundle_dir, allowed_signers=allowed, require_signature=True, expected_version="0.1.0-alpha.1"
            )
            self.assertEqual(manifest["version"], "0.1.0-alpha.1")
            self.assertEqual(verified_archive, archive_path)
            self.assertTrue(sig_ok)

            # Corrupt archive bytes -> must fail checksum check.
            original = archive_path.read_bytes()
            archive_path.write_bytes(original + b"tampered")
            with self.assertRaises(RuntimeError):
                BUNDLE.verify_bundle(bundle_dir, allowed_signers=allowed, require_signature=True)
            archive_path.write_bytes(original)

            # Replace allowed_signers with an unrelated key -> must reject publisher.
            other_key = root / "untrusted_ed25519"
            subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(other_key)], check=True)
            untrusted = root / "untrusted_signers"
            untrusted.write_text(f"release@tucunar.com {Path(str(other_key) + '.pub').read_text().strip()}\n")
            with self.assertRaises(RuntimeError):
                BUNDLE.verify_bundle(bundle_dir, allowed_signers=untrusted, require_signature=True)

            # Remove signatures -> --require-signature must fail.
            for sig in bundle_dir.glob("*.sig"):
                sig.unlink()
            with self.assertRaises(RuntimeError):
                BUNDLE.verify_bundle(bundle_dir, allowed_signers=allowed, require_signature=True)

    def test_missing_service_image_in_tar_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            bundle_dir, _, _, allowed = self._build_signed_bundle(root, drop_service="hummingbird")
            with self.assertRaises(RuntimeError) as ctx:
                BUNDLE.verify_bundle(bundle_dir, allowed_signers=allowed, require_signature=True)
            self.assertIn("hummingbird", str(ctx.exception))

    def test_package_archive_and_verify_file_cli(self):
        import sys
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            _, config, _ = BUNDLE.load_release_config()
            refs = [e["ref"] for e in BUNDLE.expected_images(config, "0.1.0-alpha.1")]
            raw_tar = root / "combined.tar"
            make_synthetic_oci_tar(raw_tar, refs)
            digests = root / "digests.json"
            digests.write_text(json.dumps({r: "sha256:abc123" for r in refs}))
            key = root / "key_ed25519"
            subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)], check=True)
            allowed = root / "allowed_signers"
            allowed.write_text(f"release@tucunar.com {Path(str(key) + '.pub').read_text().strip()}\n")
            out_dir = root / "packaged"
            subprocess.run([
                sys.executable, str(SCRIPT), "package-archive",
                str(raw_tar), str(out_dir),
                "--version", "0.1.0-alpha.1",
                "--commit", "deadbeef",
                "--image-digests-json", str(digests),
                "--signing-key", str(key),
                "--allowed-signers", str(allowed),
            ], check=True)
            sha_file = out_dir / "tucunar-cell-images-linux-amd64-0.1.0-alpha.1.oci.tar.sha256"
            subprocess.run([
                sys.executable, str(SCRIPT), "verify-file",
                "--allowed-signers", str(allowed),
                str(sha_file),
            ], check=True)


if __name__ == "__main__":
    unittest.main()
