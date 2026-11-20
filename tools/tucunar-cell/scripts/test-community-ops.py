#!/usr/bin/env python3
"""Unit tests for Community backup preflight, restore verification, upgrade snapshots and rollback guards."""
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("community_ops", SCRIPTS / "community-ops.py")
OPS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OPS)


class CommunityOpsTests(unittest.TestCase):
    def _init_instance(self, root):
        inst_dir = root / "instance"
        subprocess.run([sys.executable, str(SCRIPTS / "community-init.py"), str(inst_dir)], check=True)
        return OPS.load_instance(inst_dir)

    def test_upgrade_snapshot_preserves_credentials_and_updates_all_four_tags(self):
        with tempfile.TemporaryDirectory() as directory:
            inst = self._init_instance(Path(directory))
            config = OPS.load_release_config()
            orig_env = dict(inst["env"])
            snap_dir = OPS.save_instance_snapshot(inst, 1)
            OPS.apply_version_to_instance(inst, config, "0.1.0-alpha.2")

            reloaded = OPS.load_instance(inst["root"])
            self.assertEqual(reloaded["meta"]["version"], "0.1.0-alpha.2")
            self.assertEqual(reloaded["env"]["CANARY_NODE_VERSION"], "0.1.0-alpha.2")
            for key in ("CLICKHOUSE_PASSWORD", "SIGNAL_INGEST_TOKEN", "CANARY_NODE_API_TOKEN",
                        "TUCUNAR_INTERNAL_TOKEN", "CANARY_ORGANIZATION_ID", "CANARY_ID"):
                self.assertEqual(reloaded["env"][key], orig_env[key])
            kust = reloaded["kustomization"].read_text()
            self.assertEqual(kust.count("newTag: 0.1.0-alpha.2"), 4)
            self.assertNotIn("newTag: 0.1.0-alpha.1", kust)

            snap_meta = json.loads((snap_dir / "snapshot-meta.json").read_text())
            self.assertEqual(snap_meta["version"], "0.1.0-alpha.1")
            # Schema incompatibility must block binary-only rollback unless --after-restore is set.
            with self.assertRaises(RuntimeError):
                OPS.validate_rollback_compatibility(snap_meta, current_schema_compat=2, after_restore=False)
            OPS.validate_rollback_compatibility(snap_meta, current_schema_compat=2, after_restore=True)

    def test_restore_preflight_and_checksum_verification(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inst = self._init_instance(root)
            backup_dir = root / "backup"
            backup_dir.mkdir()
            sample = root / "sample.txt"
            sample.write_text("ledger-data\n")
            archive = backup_dir / "clickhouse-data-test.tar.gz"
            with tarfile.open(archive, "w:gz") as bundle:
                bundle.add(sample, arcname="sample.txt")
            a_sha = OPS.sha256_file(archive)
            (backup_dir / (archive.name + ".sha256")).write_text(f"{a_sha}  {archive.name}\n")
            manifest = {
                "manifest_version": 1,
                "method": "quiesced-statefulset-snapshot",
                "version": inst["meta"]["version"],
                "schema_compatibility": 1,
                "organization_id": inst["meta"]["organization_id"],
                "cell_id": inst["meta"]["cell_id"],
                "namespace": "tucunar-cell",
                "archive": archive.name,
                "archive_sha256": a_sha,
                "row_counts": {"canary_signal_events": 10},
                "credentials_included": False,
            }
            m_path = backup_dir / "backup-manifest.json"
            m_path.write_text(json.dumps(manifest) + "\n")
            (backup_dir / "backup-manifest.json.sha256").write_text(f"{OPS.sha256_file(m_path)}  backup-manifest.json\n")

            verified_manifest, _ = OPS.verify_backup_dir(backup_dir)
            OPS.preflight_restore(
                verified_manifest, inst,
                actual_context="default", expect_context="default",
                expect_namespace="tucunar-cell", confirm_cell_id=inst["meta"]["cell_id"],
            )

            # Wrong context -> refused.
            with self.assertRaises(RuntimeError):
                OPS.preflight_restore(
                    verified_manifest, inst,
                    actual_context="prod-cluster", expect_context="default",
                    expect_namespace="tucunar-cell", confirm_cell_id=inst["meta"]["cell_id"],
                )
            # Wrong cell_id -> refused.
            with self.assertRaises(RuntimeError):
                OPS.preflight_restore(
                    verified_manifest, inst,
                    actual_context="default", expect_context="default",
                    expect_namespace="tucunar-cell", confirm_cell_id="wrong-cell",
                )
            # Corrupted archive -> refused.
            archive.write_bytes(archive.read_bytes() + b"corrupt")
            with self.assertRaises(RuntimeError):
                OPS.verify_backup_dir(backup_dir)


if __name__ == "__main__":
    unittest.main()
