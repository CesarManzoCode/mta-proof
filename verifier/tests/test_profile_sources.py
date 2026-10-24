import unittest

from verifier.props import profile_sources

RESEARCH = """
| Id | Fuente | Tipo | Texto |
| --- | --- | --- | --- |
| R-YAHOO | https://example.test | primaria | x |
| R-SUPED-YAHOO | https://example.test | secundaria | y |
"""


def profile(rules, sources=("R-YAHOO",), limitations=""):
    return {"name": "p", "sources": list(sources), "limitations": limitations, "rules": rules}


def rule(rid, source="R-YAHOO", assumption=False):
    r = {"id": rid, "kind": "respond", "params": {}, "assumption": assumption}
    if source is not None:
        r["source"] = source
    return r


class ProfileSourcesTest(unittest.TestCase):
    def setUp(self):
        self.sources = profile_sources.research_sources(RESEARCH)

    def test_research_parsing(self):
        self.assertEqual(self.sources, {"R-YAHOO": "primary", "R-SUPED-YAHOO": "secondary"})

    def test_sourced_and_assumed_rules_pass(self):
        ok, details, failures = profile_sources.check(
            {"p": profile([rule("a"), rule("b", source=None, assumption=True)])}, self.sources)
        self.assertTrue(ok, failures)
        self.assertEqual(details, {"profiles": 1, "rules": 2})

    def test_rule_without_source_fails(self):
        ok, _, failures = profile_sources.check(
            {"p": profile([rule("a", source=None, assumption=False)])}, self.sources)
        self.assertFalse(ok)
        self.assertIn("p/a: no source and not marked assumption", failures)

    def test_source_missing_from_research_fails(self):
        ok, _, failures = profile_sources.check(
            {"p": profile([rule("a", source="R-NOPE")], sources=("R-YAHOO", "R-NOPE"))},
            self.sources)
        self.assertFalse(ok)
        self.assertTrue(any("not in RESEARCH" in f for f in failures), failures)

    def test_secondary_source_without_limitations_fails(self):
        p = profile([rule("a", source="R-SUPED-YAHOO", assumption=True)],
                    sources=("R-YAHOO", "R-SUPED-YAHOO"))
        ok, _, failures = profile_sources.check({"p": p}, self.sources)
        self.assertFalse(ok)
        self.assertTrue(any("without limitations" in f for f in failures), failures)
        p["limitations"] = "texto de fuente secundaria"
        self.assertTrue(profile_sources.check({"p": p}, self.sources)[0])

    def test_rule_source_outside_profile_sources_fails(self):
        ok, _, _ = profile_sources.check(
            {"p": profile([rule("a", source="R-SUPED-YAHOO")], limitations="x")}, self.sources)
        self.assertFalse(ok)


if __name__ == "__main__":
    unittest.main()
