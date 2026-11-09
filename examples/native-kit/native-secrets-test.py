#!/usr/bin/env python3
"""Local host-store writer checks; uses fake credentials only."""
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
import uuid

spec = importlib.util.spec_from_file_location("native_secrets", Path(__file__).with_name("native-secrets.py"))
secrets = importlib.util.module_from_spec(spec)
spec.loader.exec_module(secrets)


class HostStoreTests(unittest.TestCase):
    def test_rotation_isolation_and_delete(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "secrets.json"
            project, other = str(uuid.uuid4()), str(uuid.uuid4())
            secrets.update(path, project, "Key", "first")
            secrets.update(path, other, "key", "other-project")
            secrets.update(path, project, "key", "rotated")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(json.loads(path.read_text())["projects"],
                             {project: {"key": "rotated"}, other: {"key": "other-project"}})
            secrets.update(path, project, "key", None)
            self.assertEqual(json.loads(path.read_text())["projects"],
                             {project: {}, other: {"key": "other-project"}})

    def test_symlink_and_public_file_are_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            path, target = Path(directory) / "store", Path(directory) / "target"
            target.write_text("untouched")
            path.symlink_to(target)
            with self.assertRaises(OSError):
                secrets.update(path, str(uuid.uuid4()), "key", "fake")
            self.assertEqual(target.read_text(), "untouched")
            path.unlink()
            path.write_text('{"projects":{}}')
            os.chmod(path, 0o644)
            with self.assertRaises(ValueError):
                secrets.update(path, str(uuid.uuid4()), "key", "fake")
            self.assertEqual(path.read_text(), '{"projects":{}}')


if __name__ == "__main__":
    unittest.main()
