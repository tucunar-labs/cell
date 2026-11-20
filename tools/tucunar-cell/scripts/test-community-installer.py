#!/usr/bin/env python3
"""Regression checks for startup races and preserving an existing kubeconfig."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class InstallerTests(unittest.TestCase):
    def test_waits_for_admin_config_and_preserves_existing_config(self):
        installer = Path(__file__).resolve().parent.parent / 'deploy/install.sh'
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fake = root / 'k0s'
            fake.write_text('''#!/bin/bash
count=$(cat "$COUNTER" 2>/dev/null || echo 0)
count=$((count+1))
echo "$count" > "$COUNTER"
if ((count < 3)); then exit 1; fi
printf 'synthetic-kubeconfig\\n'
''')
            fake.chmod(0o755)
            env = dict(os.environ, INSTALLER=str(installer), FAKE_K0S=str(fake),
                       COUNTER=str(root / 'counter'), KUBECONFIG=str(root / 'config'))
            command = ['bash', '-c', 'source "$INSTALLER"; k0s_bin="$FAKE_K0S"; sleep() { :; }; configure_kubeconfig']
            result = subprocess.run(command, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            config = root / 'config'
            self.assertEqual(config.read_text(), 'synthetic-kubeconfig\n')
            self.assertEqual(config.stat().st_mode & 0o777, 0o600)
            self.assertEqual((root / 'counter').read_text().strip(), '3')
            config.write_text('existing-unrelated-cluster\n')
            result = subprocess.run(command, env=env, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('already exists with different contents', result.stderr)
            self.assertEqual(config.read_text(), 'existing-unrelated-cluster\n')


if __name__ == '__main__':
    unittest.main()
