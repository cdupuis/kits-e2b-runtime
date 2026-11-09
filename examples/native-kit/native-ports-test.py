#!/usr/bin/env python3
"""Host registry listing tests; no SDK, API key or deployment required."""
import contextlib
import fcntl
import importlib.util
import io
import json
import pathlib
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("native_kit", pathlib.Path(__file__).with_name("native-kit.py"))
module = importlib.util.module_from_spec(spec)
with patch.dict(sys.modules, {
    "e2b": types.SimpleNamespace(Sandbox=None, PtySize=None),
    "e2b.sandbox.commands.command_handle": types.SimpleNamespace(CommandExitException=RuntimeError),
}):
    spec.loader.exec_module(module)


class PortListingTests(unittest.TestCase):
    def test_live_and_stale_records(self):
        with tempfile.TemporaryDirectory() as root:
            directory = pathlib.Path(root) / "team" / "sandbox"
            directory.mkdir(parents=True)
            record = {"team": "team", "instance": "sandbox", "lifecycle": "one",
                      "endpoints": [{"name": "app", "container": 3000, "transport": "tcp",
                                     "family": "ipv4", "address": "127.0.0.1:12345"}]}
            path = directory / "one.json"
            path.write_text(json.dumps(record))
            args = types.SimpleNamespace(registry=root, sandbox="sandbox", team=None, json=True)
            with path.open() as owner:
                fcntl.flock(owner, fcntl.LOCK_EX)
                output = io.StringIO()
                with contextlib.redirect_stdout(output):
                    module.ports(args)
                self.assertEqual([record], json.loads(output.getvalue()))
                args.team = "another-team"
                output = io.StringIO()
                with contextlib.redirect_stdout(output):
                    module.ports(args)
                self.assertEqual([], json.loads(output.getvalue()))
            args.team = None
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                module.ports(args)
            self.assertEqual([], json.loads(output.getvalue()), "unlocked crash records must be hidden")

    def test_path_traversal_refused(self):
        with self.assertRaises(ValueError):
            module.ports(types.SimpleNamespace(sandbox="../../etc"))


if __name__ == "__main__":
    unittest.main()
