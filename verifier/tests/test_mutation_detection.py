import os
import tempfile
import unittest
from unittest import mock

from verifier import context
from verifier.props import mutation_detection as md


def rep(*failed, names=("backoff_expiry", "no_loss", "outbound_auth", "receiver_limits",
                        "destination_isolation", "ip_warmup")):
    return {"properties": [{"name": n, "verdict": "fail" if n in failed else "pass"} for n in names]}


def original(code=0, report=None):
    return {"name": "original", "exit_code": code, "report": rep() if report is None else report}


def mutants(codes=None, failing=None):
    """One entry per mutant of 4.3; by default each fails exactly its property."""
    out = []
    for name, expected in md.MUTANTS:
        failed = (failing or {}).get(name, (expected,))
        code = (codes or {}).get(name, 1 if failed else 0)
        out.append({"name": name, "expected": expected, "exit_code": code, "report": rep(*failed)})
    return out


class CheckTest(unittest.TestCase):
    def test_all_detected_passes(self):
        ok, details, failures = md.check(original(), mutants())
        self.assertTrue(ok, failures)
        self.assertEqual(details["original_exit_code"], 0)
        self.assertEqual([m["name"] for m in details["mutants"]], [m for m, _ in md.MUTANTS])
        for m in details["mutants"]:
            self.assertEqual(m["exit_code"], 1)
            self.assertEqual(m["failed_properties"], [m["expected_property"]])

    def test_mutant_that_passes_the_suite_fails(self):
        ok, _, failures = md.check(original(), mutants(failing={"mut_shared_queue": ()}))
        self.assertFalse(ok)
        self.assertTrue(any("mut_shared_queue: suite exit code 0" in f for f in failures), failures)

    def test_mutant_caught_by_another_property_only_fails(self):
        ok, _, failures = md.check(original(), mutants(failing={"mut_no_backoff": ("no_loss",)}))
        self.assertFalse(ok)
        self.assertEqual(failures, ["mut_no_backoff: backoff_expiry passed (failed: ['no_loss'])"])

    def test_original_must_exit_zero(self):
        ok, _, failures = md.check(original(code=1, report=rep("no_loss")), mutants())
        self.assertFalse(ok)
        self.assertIn("original engine: suite exit code 1", failures[0])

    def test_missing_report_fails(self):
        ms = mutants()
        ms[0].update(exit_code=2, report=None)
        ok, _, failures = md.check(original(), ms)
        self.assertFalse(ok)
        self.assertTrue(any("no report.json" in f for f in failures), failures)

    def test_leaked_or_untagged_binaries_fail(self):
        o = original()
        o["leaked"] = {"mtaengine": ["mut_no_backoff"]}
        ms = mutants()
        ms[1]["untagged"] = ["mtadst"]
        ok, _, failures = md.check(o, ms)
        self.assertFalse(ok)
        self.assertEqual(len(failures), 2, failures)

    def test_inner_suite_excludes_itself_and_external(self):
        self.assertEqual(md.inner_properties(["no_loss", "mutation_detection", "external_validation",
                                              "resources"]), ["no_loss", "resources"])


class RunTest(unittest.TestCase):
    """run() with synthetic suite exit codes and binaries."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = self._tmp.name
        self.bin = os.path.join(self.root, "bin")
        for b in md.BINARIES:
            self.write(os.path.join(self.bin, b), b"original")
            for name, _ in md.MUTANTS:
                self.write(os.path.join(self.bin, "mut", name, b), b"x mutant " + name.encode())
        self.ctx = context.Context(self.root, "out", self.bin, params={})

    def tearDown(self):
        self._tmp.cleanup()

    @staticmethod
    def write(path, data):
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(data)

    def fake_suite(self, codes):
        calls = []

        def run_suite(ctx, name, bin_dir, properties):
            calls.append((name, bin_dir, properties))
            expected = dict(md.MUTANTS).get(name)
            code = codes.get(name, 1 if expected else 0)
            return {"name": name, "exit_code": code, "duration_s": 1.0,
                    "report": rep(expected) if code else rep()}
        return calls, run_suite

    def test_runs_original_and_each_mutant(self):
        calls, fake = self.fake_suite({})
        with mock.patch.object(md, "run_suite", fake):
            res = md.run(self.ctx)
        self.assertEqual(res["verdict"], "pass", res["details"].get("failures"))
        self.assertEqual([c[0] for c in calls], ["original"] + [m for m, _ in md.MUTANTS])
        self.assertEqual(calls[0][1], self.bin)
        self.assertEqual(calls[1][1], os.path.join(self.bin, "mut", "mut_no_backoff"))
        self.assertNotIn("mutation_detection", calls[0][2])
        self.assertIn("no_loss", calls[0][2])
        self.assertEqual(res["details"]["reports"]["mut_exceed_warmup"],
                         os.path.join("out", "runs", "mutation", "mut_exceed_warmup", "report.json"))

    def test_undetected_mutant_fails_the_property(self):
        _, fake = self.fake_suite({"mut_ack_before_persist": 0})
        with mock.patch.object(md, "run_suite", fake):
            res = md.run(self.ctx)
        self.assertEqual(res["verdict"], "fail")
        self.assertIn("mut_ack_before_persist: suite exit code 0, the mutant went undetected",
                      res["details"]["failures"])

    def test_leaked_mutant_in_original_binary_fails(self):
        self.write(os.path.join(self.bin, "mtadst"), b"... mut_shared_queue ...")
        _, fake = self.fake_suite({})
        with mock.patch.object(md, "run_suite", fake):
            res = md.run(self.ctx)
        self.assertEqual(res["verdict"], "fail")
        self.assertEqual(res["details"]["failures"], ["original mtadst holds the mutant names ['mut_shared_queue']"])

    def test_run_suite_reads_the_inner_report(self):
        # A stand-in for python3 -m verifier: writes a failing report and exits 1.
        def fake_run(cmd, cwd, stdout, stderr):
            out = cmd[cmd.index("--out") + 1]
            self.write(os.path.join(out, "report.json"), b'{"properties": [{"name": "no_loss", "verdict": "fail"}]}')
            return mock.Mock(returncode=1)
        with mock.patch.object(md.subprocess, "run", fake_run):
            m = md.run_suite(self.ctx, "mut_ack_before_persist", self.bin, ["no_loss"])
        self.assertEqual(m["exit_code"], 1)
        self.assertEqual(md.failed_properties(m["report"]), ["no_loss"])
        self.assertTrue(os.path.exists(os.path.join(self.root, "out", "runs", "mutation",
                                                    "mut_ack_before_persist", "verify.log")))


if __name__ == "__main__":
    unittest.main()
