#!/usr/bin/env python3
"""Export the public core from an explicit allowlist, without private history."""
import argparse
import hashlib
from pathlib import Path
import shutil
import tarfile
import tempfile


def main():
    root = Path(__file__).resolve().parents[3]
    cell = root / "tools/tucunar-cell"
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    destination = args.output.resolve()
    if destination.with_name(destination.name + ".tar.gz").exists():
        parser.error("archive already exists; choose a new output name")
    if destination.exists():
        parser.error("output already exists; choose a new directory")
    if destination.is_relative_to(root):
        parser.error("output must be outside the private workspace")
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = Path(tempfile.mkdtemp(prefix=".tucunar-core-", dir=destination.parent))

    def copy(file, target=None):
        if file.is_symlink():
            raise ValueError(f"Symlinks are excluded: {file.relative_to(root)}")
        relative = target or file.relative_to(root)
        output = temporary / relative
        output.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(file, output)

    try:
        for file in cell.iterdir():
            if file.is_file() and (file.suffix in (".go", ".sql", ".json", ".html") or file.name in
                    ("go.mod", "go.sum", "Makefile", "Containerfile", "README.md", "LICENSE", "VERSION")):
                copy(file)
        subtrees = {
            "tools/tucunar-cell/cmd": {".go"},
            "tools/tucunar-cell/internal": {".go"},
            "tools/tucunar-cell/assets": {".svg", ".html"},
            "tools/tucunar-cell/deploy/k0s": {".yaml", ".json", ".example", ".md"},
            "tools/tucunar-cell/docs": {".md"},
            "tools/tucunar-cell/examples/scada": {".py", ".sh", ".txt", ".md"},
            "tools/tucunar-cell/benchmark": {".go"},
            "tools/tucunar-cell/traffic-generator": {".go"},
            "tools/tucunar-cell-console/src": {".rs"},
            "tools/tucunar-cell-console/ui": {".xml"},
            "tools/tucunar-cell-console/dist": {".svg", ".in"},
            "contracts/gen": {".go"},
            "contracts/proto": {".proto"},
            "contracts/fixtures": {".json"},
        }
        for subtree, suffixes in subtrees.items():
            for file in (root / subtree).rglob("*"):
                if file.is_file() and file.suffix in suffixes and not any(
                        part.startswith(".") for part in file.relative_to(root / subtree).parts):
                    copy(file)
        copy(cell / "deploy/install.sh")
        for name in (
            "community-init.py", "community-smoke.py", "community-tools.py",
            "community-bundle.py", "community-ops.py",
            "test-community-installer.py", "test-community-bundle.py", "test-community-ops.py",
            "export-core.py", "verify-export.py",
            "containerd.sh", "diagnostics.sh", "validate.sh",
        ):
            copy(cell / "scripts" / name)
        for name in ("Cargo.toml", "Cargo.lock", "build.rs", "README.md", "LICENSE", "install.sh", "dist/tucunar-cell-console-run"):
            copy(root / "tools/tucunar-cell-console" / name)
        for name in ("go.mod", "go.sum", "LICENSE", "contract_compatibility_test.go",
                     "tucunar-capabilities.openapi.yaml", "tucunar-app-canary-node.openapi.yaml",
                     "tucunar-app-canary-node.md", "scada-topology.schema.json", "scada-console-events.schema.json", "tucunar-cell-console.openapi.yaml"):
            copy(root / "contracts" / name)
        # Public repository metadata is explicit too: never sweep arbitrary files
        # (including credentials or maintainer notes) from distribution/.
        public_files = (
            "README.md", "CONTRIBUTING.md", "SECURITY.md", "NOTICE",
            "allowed_signers",
            ".github/workflows/community-export.yml",
            ".github/workflows/community-release.yml",
            ".github/ISSUE_TEMPLATE/bug_report.md",
            ".github/pull_request_template.md",
        )
        for name in public_files:
            file = cell / "distribution" / name
            copy(file, Path(name))
            # Preserve the inputs required to re-export a public checkout.
            copy(file)
        # Private operational links are not part of the Community distribution.
        runtime_readme = temporary / "tools/tucunar-cell/README.md"
        runtime_readme.write_text("\n".join(
            line.replace("(distribution/README.md)", "(../../README.md)")
            for line in runtime_readme.read_text().splitlines()
            if "../../GTM-TODO.md" not in line and "deploy/CLOUD.md" not in line
        ) + "\n")
        copy(cell / "LICENSE", Path("LICENSE"))
        (temporary / ".gitignore").write_text("**/target/\n**/.venv/\n**/__pycache__/\n**/.runtime/\n**/secret.env\n*.token\n.env*\n")
        manifest = []
        for file in sorted(temporary.rglob("*")):
            if file.is_file():
                manifest.append(f"{hashlib.sha256(file.read_bytes()).hexdigest()}  {file.relative_to(temporary)}\n")
        (temporary / "SHA256SUMS").write_text("".join(manifest))
        temporary.rename(destination)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)
    archive = destination.with_name(destination.name + ".tar.gz")
    if archive.exists():
        raise FileExistsError(archive)
    with tarfile.open(archive, "w:gz") as bundle:
        bundle.add(destination, arcname=destination.name)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_name(archive.name + ".sha256").write_text(f"{digest}  {archive.name}\n")
    print(f"Exported {len(manifest)} allowlisted files to {destination}")
    print(f"Archive {archive}")


if __name__ == "__main__":
    main()
