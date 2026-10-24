import unittest

from verifier.props import classification

CASES = [
    {"id": "a", "provider": "gmail-model", "code": 550, "enhanced": "5.1.1", "text": "x",
     "expected_class": "permanent", "expected_subtype": "NoEmail", "source": "R-GOOGLE-ERR"},
    {"id": "b", "provider": "", "code": 599, "enhanced": "", "text": "y",
     "expected_class": "undetermined", "expected_subtype": "", "source": "R-3463"},
]
GOOD = [
    {"id": "a", "class": "permanent", "subtype": "NoEmail", "rule_id": "google.no_account"},
    {"id": "b", "class": "undetermined", "subtype": "", "rule_id": "d14.undetermined"},
]


class ClassificationTest(unittest.TestCase):
    def test_matching_corpus_passes(self):
        ok, details, failures = classification.check(CASES, GOOD)
        self.assertTrue(ok, failures)
        self.assertEqual(details, {"cases": 2, "matched": 2})

    def test_misclassified_case_fails(self):
        bad = [dict(GOOD[0], subtype="General"), GOOD[1]]
        ok, details, failures = classification.check(CASES, bad)
        self.assertFalse(ok)
        self.assertEqual(details["matched"], 1)
        self.assertTrue(any(f.startswith("a: expected permanent/NoEmail") for f in failures))

    def test_unruled_response_permanent_fails(self):
        bad = [GOOD[0], dict(GOOD[1], **{"class": "permanent", "subtype": "General"})]
        ok, _, failures = classification.check(CASES, bad)
        self.assertFalse(ok)
        self.assertIn("b: response without rule classified permanent", failures)

    def test_decision_without_rule_id_fails(self):
        ok, _, failures = classification.check(CASES, [dict(GOOD[0], rule_id=""), GOOD[1]])
        self.assertFalse(ok)
        self.assertIn("a: decision without rule_id", failures)

    def test_missing_or_reordered_decisions_fail(self):
        self.assertFalse(classification.check(CASES, GOOD[:1])[0])
        ok, _, failures = classification.check(CASES, GOOD[::-1])
        self.assertIn("decisions are not one per corpus case in corpus order", failures)
        self.assertTrue(failures)


if __name__ == "__main__":
    unittest.main()
