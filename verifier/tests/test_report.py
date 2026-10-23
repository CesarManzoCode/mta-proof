import datetime
import json
import os
import tempfile
import unittest

from verifier import cli, context, formats, report
from verifier.tests import fixtures

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))


class ReportTest(unittest.TestCase):
    def test_global_verdict_needs_every_property(self):
        props = [report.result("a", True, {}), report.result("b", False, {}, failures=["x"])]
        rep = report.build(ROOT, datetime.datetime.now(datetime.timezone.utc), 1.0, 10, 20, props)
        self.assertEqual(rep["format"], "mta-proof-report/1")
        self.assertEqual(rep["verdict"], "fail")
        self.assertEqual(props[1]["details"]["failures"], ["x"])
        rep = report.build(ROOT, datetime.datetime.now(datetime.timezone.utc), 1.0, 10, 20,
                           props[:1])
        self.assertEqual(rep["verdict"], "pass")
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "out", "report.json")
            report.write(path, rep)
            with open(path) as f:
                self.assertEqual(json.load(f)["properties"][0]["name"], "a")

    def test_failures_turn_a_pass_into_fail(self):
        self.assertEqual(report.result("a", True, {}, failures=["x"])["verdict"], "fail")

    def test_only_selects_implemented_properties(self):
        self.assertEqual(cli.select("resources,classification"), ["classification", "resources"])
        self.assertEqual(cli.select(None), list(cli.PROPERTIES))
        with self.assertRaises(SystemExit):
            cli.select("tenant_pause")


class ParamsTest(unittest.TestCase):
    def test_lab_params_are_section_5(self):
        p = context.load_params(os.path.join(ROOT, "lab", "params.json"))
        self.assertEqual(p["retry_policy"],
                         {"initial_s": 1800, "factor": 2, "max_s": 10800, "lifetime_s": 432000})
        self.assertEqual(p["memory_limit_bytes"], 8 * 1024 ** 3)
        self.assertGreaterEqual(p["campaign_messages"], 10000)
        self.assertGreaterEqual(p["campaign_kills"] + p["campaign_kills_post_250"], 25)

    def test_unknown_key_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "params.json")
            p = formats.load_json(os.path.join(ROOT, "lab", "params.json"))
            p["extra"] = 1
            with open(path, "w") as f:
                json.dump(p, f)
            with self.assertRaises(formats.FormatError):
                context.load_params(path)


class FormatsTest(unittest.TestCase):
    def test_canonical_events_escapes_like_contract(self):
        with tempfile.TemporaryDirectory() as tmp:
            run = fixtures.write_run(tmp, [], [fixtures.event(1, 5, "m1", "accepted",
                                                              text="ñ\n\U0001f600")])
            line = formats.canonical_events(os.path.join(run, "engine.db"))
        self.assertIn(b'"text":"\\u00f1\\n\\ud83d\\ude00"', line)
        self.assertTrue(line.startswith(b'{"at_ms":5,"attempt":null,'))
        self.assertTrue(line.endswith(b"}\n"))

    def test_bad_jsonl_line_reported(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "receiver.jsonl")
            with open(path, "w") as f:
                f.write('{"seq":1,"at_ms":0,"ev":"connect"}\nnot json\n')
            with self.assertRaises(formats.FormatError):
                formats.read_receiver(tmp)


if __name__ == "__main__":
    unittest.main()
