import os
import tempfile
import unittest

from verifier.props import resources

GIB8 = 8589934592


class ResourcesTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = self._tmp.name

    def tearDown(self):
        self._tmp.cleanup()

    def test_reads_own_scope_peak(self):
        proc = os.path.join(self.tmp, "cgroup")
        with open(proc, "w") as f:
            f.write("0::/user.slice/app.slice/run-x.scope\n")
        cg = resources.own_cgroup(proc)
        self.assertEqual(cg, "/user.slice/app.slice/run-x.scope")
        os.makedirs(os.path.join(self.tmp, "fs", cg.lstrip("/")))
        with open(os.path.join(self.tmp, "fs", cg.lstrip("/"), "memory.peak"), "w") as f:
            f.write("215048192\n")
        self.assertEqual(resources.memory_peak(cg, os.path.join(self.tmp, "fs")), 215048192)
        self.assertIsNone(resources.memory_peak("/missing", os.path.join(self.tmp, "fs")))

    def test_within_limit_passes(self):
        ok, details, failures = resources.check(1 << 20, GIB8, 1.5, "/x")
        self.assertTrue(ok, failures)
        self.assertEqual(details["duration_s"], 1.5)

    def test_over_limit_fails(self):
        self.assertFalse(resources.check(GIB8 + 1, GIB8, 1.0, "/x")[0])

    def test_unreadable_peak_fails(self):
        self.assertFalse(resources.check(None, GIB8, 1.0, None)[0])


if __name__ == "__main__":
    unittest.main()
