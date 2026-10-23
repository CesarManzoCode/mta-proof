import os
import tempfile
import unittest

from verifier.props import determinism
from verifier.tests import fixtures


def receiver(n):
    return [{"seq": 1, "at_ms": n, "conn": 1, "profile": "baseline", "ev": "connect",
             "src_ip": "127.0.2.1"}]


EVENTS = [fixtures.event(1, 0, "m00000001", "accepted", {"payload_sha256": "a"},
                         text="café \"q\"")]


class DeterminismTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = self._tmp.name

    def tearDown(self):
        self._tmp.cleanup()

    def run_dir(self, name, n, **kw):
        return fixtures.write_run(os.path.join(self.tmp, name), receiver(n), EVENTS, **kw)

    def test_same_seed_same_hash_passes(self):
        ok, details, failures = determinism.check(
            self.run_dir("a", 1), self.run_dir("b", 1), self.run_dir("o", 2))
        self.assertTrue(ok, failures)
        self.assertEqual(details["hash_a"], details["hash_b"])
        self.assertNotEqual(details["hash_a"], details["hash_other"])

    def test_different_hash_same_seed_fails(self):
        ok, _, failures = determinism.check(
            self.run_dir("a", 1), self.run_dir("b", 3), self.run_dir("o", 2))
        self.assertFalse(ok)
        self.assertIn("same seed gave different traces", failures)

    def test_other_seed_same_hash_fails(self):
        ok, _, failures = determinism.check(
            self.run_dir("a", 1), self.run_dir("b", 1), self.run_dir("o", 1))
        self.assertFalse(ok)
        self.assertIn("another seed gave the same trace", failures)

    def test_recorded_hash_is_not_trusted(self):
        b = self.run_dir("b", 1)
        with open(os.path.join(b, "trace.sha256"), "w") as f:
            f.write("0" * 64 + "\n")
        ok, _, failures = determinism.check(self.run_dir("a", 1), b, self.run_dir("o", 2))
        self.assertFalse(ok)
        self.assertTrue(any("differs from the recomputed" in f for f in failures), failures)

    def test_events_export_must_match_db(self):
        b = self.run_dir("b", 1)
        with open(os.path.join(b, "events.jsonl"), "ab") as f:
            f.write(b"{}\n")
        ok, _, failures = determinism.check(self.run_dir("a", 1), b, self.run_dir("o", 2))
        self.assertFalse(ok)
        self.assertIn("b: events.jsonl differs from engine.db", failures)


if __name__ == "__main__":
    unittest.main()
