#!/usr/bin/env python3
"""Prepare one private, offline-capable community deployment directory."""
import argparse
import json
from pathlib import Path
import re
import secrets
import shutil
import tempfile
import uuid


def main():
    cell = Path(__file__).resolve().parent.parent
    release = json.loads((cell / "community-release.json").read_text())
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--version", default=(cell / "VERSION").read_text().strip())
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?", args.version):
        parser.error("version must be an explicit semantic version")
    destination = args.directory.resolve()
    if destination.exists():
        parser.error("destination already exists; reuse it to preserve credentials and identity")
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = Path(tempfile.mkdtemp(prefix=".tucunar-init-", dir=destination.parent))
    try:
        source = cell / "deploy/k0s"
        for file in source.rglob("*"):
            if file.is_file() and not file.is_symlink() and file.suffix in (".yaml", ".json", ".example"):
                target = temporary / "k0s" / file.relative_to(source)
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(file, target)
        profile = temporary / "k0s/overlays/community"
        for existing, name in (("single-node/resources-small.patch.yaml", "resources-small.patch.yaml"),
                               ("dev/canary-rules.patch.yaml", "canary-rules.patch.yaml")):
            shutil.copyfile(source / "overlays" / existing, profile / name)
        values = {key: secrets.token_hex(32) for key in (
            "CLICKHOUSE_PASSWORD", "SIGNAL_INGEST_TOKEN", "CANARY_NODE_API_TOKEN", "TUCUNAR_INTERNAL_TOKEN")}
        values.update(CANARY_ORGANIZATION_ID=str(uuid.uuid4()), CANARY_ID=str(uuid.uuid4()),
                      CANARY_NODE_VERSION=args.version)
        secret = profile / "secret.env"
        secret.touch(mode=0o600)
        secret.write_text("".join(f"{key}={value}\n" for key, value in values.items()))
        token = temporary / "operator.token"
        token.touch(mode=0o600)
        token.write_text(values["CANARY_NODE_API_TOKEN"] + "\n")
        manifest = profile / "kustomization.yaml"
        manifest.write_text(manifest.read_text().replace("../single-node/", "").replace("../dev/", ""))
        with manifest.open("a") as file:
            file.write("images:\n")
            for service in release["services"]:
                file.write(f"  - name: {release['image_prefix']}/{service}\n    newTag: {args.version}\n")
        (temporary / "instance.json").write_text(json.dumps({
            "version": args.version, "organization_id": values["CANARY_ORGANIZATION_ID"],
            "cell_id": values["CANARY_ID"], "namespace": "tucunar-cell"}, indent=2) + "\n")
        temporary.rename(destination)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)
    print(f"Prepared {destination} (private credentials, stable instance identity)")
    print(f"Apply with kubectl apply -k {destination}/k0s/overlays/community")
    print(f"Console token file: {destination}/operator.token")


if __name__ == "__main__":
    main()
