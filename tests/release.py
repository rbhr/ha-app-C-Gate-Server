#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("release", Path(__file__).resolve().parents[1] / "scripts/check-release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)

class ReleaseTests(unittest.TestCase):
    def test_new_version(self):
        release.validate_release("v1.2.0", "1.2.0", "false", ["latest", "1.1.12", "sha-abc"])

    def test_reject_overwrite_and_backwards_latest(self):
        for version in ["1.1.12", "1.1.11"]:
            with self.assertRaises(ValueError):
                release.validate_release("v"+version, version, "false", ["1.1.12"])

    def test_tag_mismatch_and_prerelease(self):
        for args in [("v1.1.12", "1.1.13", "false"), ("v1.1.13", "1.1.13", "true"), ("v1.1.13-rc1", "1.1.13-rc1", "false")]:
            with self.assertRaises(ValueError):
                release.validate_release(*args, [])

unittest.main()
