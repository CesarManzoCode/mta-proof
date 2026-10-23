import unittest

from verifier.props import receiver_limits


def line(seq, ev, **kw):
    r = {"seq": seq, "at_ms": seq, "conn": 1, "src_ip": "127.0.2.1", "profile": "yahoo-model",
         "ev": ev}
    r.update(kw)
    return r


BASE = [line(1, "connect"), line(2, "data_end", code=250, msg_id="m1")]


class ReceiverLimitsTest(unittest.TestCase):
    def test_only_uncounted_violations_pass(self):
        trace = BASE + [line(3, "violation", rule="yahoo.max_msgs_per_conn", counted=False)]
        ok, details, failures = receiver_limits.check(trace)
        self.assertTrue(ok, failures)
        self.assertEqual(details["violations"], {"yahoo-model": {"127.0.2.1": 0}})
        self.assertEqual(details["uncounted"], {"yahoo-model": {"127.0.2.1": 1}})

    def test_counted_violation_fails(self):
        trace = BASE + [line(3, "violation", rule="yahoo.max_msgs_per_conn", counted=True)]
        ok, details, failures = receiver_limits.check(trace)
        self.assertFalse(ok)
        self.assertEqual(details["violations"], {"yahoo-model": {"127.0.2.1": 1}})
        self.assertEqual(details["counted_by_rule"], {"yahoo.max_msgs_per_conn": 1})
        self.assertIn("yahoo-model/127.0.2.1: 1 counted violations", failures)

    def test_violation_without_counted_fails(self):
        ok, _, _ = receiver_limits.check(BASE + [line(3, "violation", rule="r")])
        self.assertFalse(ok)

    def test_empty_run_fails(self):
        ok, _, _ = receiver_limits.check([line(1, "connect")])
        self.assertFalse(ok)


if __name__ == "__main__":
    unittest.main()
