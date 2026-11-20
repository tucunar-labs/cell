#!/usr/bin/env python3
"""Download the pinned Linux amd64 build tools without installing a container runtime."""
import argparse
import hashlib
import json
from pathlib import Path
import platform
import shutil
import tarfile
import tempfile
from urllib.request import urlopen


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directory', type=Path, help='new directory for verified binaries (no system files changed)')
    parser.add_argument("--raven", action="store_true", help="prepare the pinned native library and notices instead of build tools")
    args = parser.parse_args()
    if platform.system() != 'Linux' or platform.machine() not in ('x86_64', 'amd64'):
        parser.error('this tool manifest currently supports Linux amd64 only')
    destination = args.directory.resolve()
    if destination.exists():
        parser.error('destination exists; choose a new directory to avoid replacing tools')
    manifest = json.loads((Path(__file__).resolve().parent.parent / 'community-release.json').read_text())
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='.cell-tools-', dir=destination.parent) as temporary:
        temporary = Path(temporary)
        binaries = temporary / 'bin'
        binaries.mkdir()
        for name, tool in manifest['desktop_tools' if args.raven else 'tools'].items():
            archive = temporary / name
            print(f"Downloading {name} {tool['version']}...", flush=True)
            with urlopen(tool['url'], timeout=60) as response, archive.open('wb') as output:
                shutil.copyfileobj(response, output)
            with archive.open('rb') as source:
                digest = hashlib.sha256(source.read()).hexdigest()
            if digest != tool['sha256']:
                raise RuntimeError(f'{name}: checksum mismatch; no tools installed')
            if 'members' in tool:
                with tarfile.open(archive, 'r:gz') as bundle:
                    for member_name, target in tool['members'].items():
                        if Path(target).name != target or target in ('.', '..'):
                            raise RuntimeError(f'{name}: invalid executable destination')
                        member = bundle.getmember(member_name)
                        if not member.isfile():
                            raise RuntimeError(f'{name}: expected a regular executable')
                        with bundle.extractfile(member) as source, (binaries / target).open('wb') as output:
                            shutil.copyfileobj(source, output)
            else:
                shutil.copyfile(archive, binaries / name)
        for binary in binaries.iterdir():
            binary.chmod(0o644 if binary.name in ('LICENSE', 'NOTICE') else 0o755)
        if args.raven:
            (binaries / 'libraven.so').symlink_to('libraven.so.0')
        binaries.rename(destination)
    print(f'Verified binaries prepared in {destination}; no containerd daemon installed')


if __name__ == '__main__':
    main()
