#!/usr/bin/env python3
"""Verify the actual Community export outside its source checkout."""
import hashlib
from pathlib import Path
import subprocess
import sys
import tempfile


def run(*args, cwd=None):
    subprocess.run(args, cwd=cwd, check=True)


def verify(root):
    for line in (root / 'SHA256SUMS').read_text().splitlines():
        digest, name = line.split('  ', 1)
        assert hashlib.sha256((root / name).read_bytes()).hexdigest() == digest, name
    for path in root.rglob('*'):
        relative = path.relative_to(root)
        assert not path.is_symlink(), relative
        assert not any(part in {'.git', 'tucunar-app', 'node_modules', 'target', '__pycache__'}
                       for part in relative.parts), relative
        assert path.name not in {'secret.env', 'GTM-TODO.md', 'release-plan.md'}, relative
        assert not path.name.startswith('.env'), relative
    # The entry documentation must work without the private monorepo.
    import re
    for document in root.rglob('*.md'):
        # distribution/ contains root-relative packaging templates, not served docs.
        if 'distribution' in document.relative_to(root).parts:
            continue
        for target in re.findall(r'\]\(([^)]+)\)', document.read_text()):
            if '://' not in target and not target.startswith('#'):
                assert (document.parent / target.split('#')[0]).exists(), (document, target)


def main():
    source = Path(__file__).resolve().parents[3]
    with tempfile.TemporaryDirectory(prefix='cell-export-check-') as directory:
        destination = Path(directory) / 'community'
        run(sys.executable, str(source / 'tools/tucunar-cell/scripts/export-core.py'), str(destination))
        verify(destination)
        readme = destination / 'README.md'
        original_readme = readme.read_bytes()
        readme.write_bytes(original_readme + b'\nsynthetic corruption\n')
        try:
            verify(destination)
        except AssertionError:
            pass
        else:
            raise RuntimeError('Corrupted artifact was accepted')
        finally:
            readme.write_bytes(original_readme)
        run(sys.executable, str(destination / "tools/tucunar-cell/scripts/test-community-installer.py"))
        run(sys.executable, str(destination / "tools/tucunar-cell/scripts/test-community-bundle.py"))
        run(sys.executable, str(destination / "tools/tucunar-cell/scripts/test-community-ops.py"))
        # Re-exporting must not require any file from the private checkout.
        sentinel = destination / 'tools/tucunar-cell/.env.export-test'
        sentinel.write_text('SYNTHETIC_EXPORT_SENTINEL=not-a-secret\n')
        (destination / 'tools/tucunar-cell/distribution/.env.export-test').write_text('SYNTHETIC=not-a-secret\n')
        second = Path(directory) / 'reexport'
        run(sys.executable, str(destination / 'tools/tucunar-cell/scripts/export-core.py'), str(second))
        verify(second)
        instance = Path(directory) / 'instance'
        initializer = destination / 'tools/tucunar-cell/scripts/community-init.py'
        run(sys.executable, str(initializer), str(instance))
        import json
        import stat
        identity = json.loads((instance / 'instance.json').read_text())
        assert identity['organization_id'] and identity['cell_id']
        secret = instance / 'k0s/overlays/community/secret.env'
        assert stat.S_IMODE(secret.stat().st_mode) == 0o600
        assert stat.S_IMODE((instance / 'operator.token').stat().st_mode) == 0o600
        original = secret.read_bytes()
        result = subprocess.run((sys.executable, str(initializer), str(instance)),
                                capture_output=True)
        assert result.returncode != 0 and secret.read_bytes() == original
        assert (instance / 'k0s/overlays/community/resources-small.patch.yaml').exists()
        assert (instance / 'k0s/overlays/community/canary-rules.patch.yaml').exists()
        assert (destination / 'SHA256SUMS').read_bytes() == (second / 'SHA256SUMS').read_bytes()
        for module in ('contracts', 'tools/tucunar-cell'):
            for command in (('go', 'test', './...'), ('go', 'build', './...'), ('go', 'vet', './...')):
                run(*command, cwd=destination / module)
    print('PASS Community checksums, isolation, entry documentation, reproducible re-export, private initialization and Go checks')


if __name__ == '__main__':
    main()
