import hashlib
import os
import tempfile
import unittest

from verifier import formats
from verifier.props import backoff_expiry
from verifier.tests import fixtures

# Small policy: waits 10, 20, 40, 40, ... s; lifetime 200 s.
POLICY = {"initial_s": 10, "factor": 2, "max_s": 40, "lifetime_s": 200}
GOOD_TIMES_S = [0, 10, 30, 70, 110, 150, 190]  # next would be 230 > 200: expires at 190


def attempt(seq, at_s, msg_id="m1", code=451):
    return {"seq": seq, "at_ms": at_s * 1000, "conn": seq, "src_ip": "127.0.2.1",
            "profile": "always-defer", "ev": "data_end", "mail_from": "notify@baseline.test",
            "rcpt": "user0@always-defer.test", "msg_id": msg_id, "sha256": "x" * 64,
            "code": code, "enhanced": "4.3.0", "text": "Requested action aborted"}


def build(tmp, times_s=GOOD_TIMES_S, expire_s=None, dsn_from="", dsn_body=None,
          extra_receiver=()):
    expire_s = times_s[-1] if expire_s is None else expire_s
    dsn = dsn_body if dsn_body is not None else fixtures.dsn_bytes("m1", "m2")
    receiver = [attempt(i + 1, t) for i, t in enumerate(times_s)]
    receiver += list(extra_receiver)
    receiver.append({
        "seq": len(receiver) + 1, "at_ms": expire_s * 1000, "conn": 99, "src_ip": "127.0.2.1",
        "profile": "baseline", "ev": "data_end", "mail_from": dsn_from,
        "rcpt": "notify@baseline.test", "msg_id": "m2",
        "sha256": hashlib.sha256(dsn).hexdigest(),
        "code": 250, "enhanced": "2.0.0", "text": "OK"})
    events = [
        fixtures.event(1, 0, "m1", "accepted", {"payload_sha256": "a"}),
        fixtures.event(2, expire_s * 1000, "m1", "expired", {"stage": "data_end"}),
        fixtures.event(3, expire_s * 1000, "m2", "accepted",
                       {"dsn_for": "m1", "payload_sha256": "b"}),
        fixtures.event(4, expire_s * 1000, "m1", "dsn_enqueued", {"dsn_msg_id": "m2"}),
    ]
    run = fixtures.write_run(os.path.join(tmp, "run"), receiver, events, messages=[dsn])
    return backoff_expiry.check(formats.read_receiver(run), formats.read_events(run + "/engine.db"),
                                POLICY, lambda sha: formats.read_eml(run, sha))


class BackoffExpiryTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = self._tmp.name

    def tearDown(self):
        self._tmp.cleanup()

    def test_good_trace_passes(self):
        ok, details, failures = build(self.tmp)
        self.assertEqual(failures, [])
        self.assertTrue(ok)
        self.assertEqual(details["max_interval_s"], 40)
        self.assertEqual((details["messages"], details["expired"], details["dsn_null_sender"]),
                         (1, 1, 1))

    def test_decreasing_intervals_fail(self):
        ok, _, failures = build(self.tmp, times_s=[0, 10, 50, 70, 110, 150, 190])
        self.assertFalse(ok)
        self.assertTrue(any("shorter than interval" in f for f in failures), failures)

    def test_interval_above_max_fails(self):
        _, _, failures = build(self.tmp, times_s=[0, 10, 30, 70, 120, 160, 199])
        self.assertTrue(any("above max_s" in f for f in failures), failures)

    def test_constant_minimum_interval_fails(self):
        # mut_no_backoff: constant, never decreasing and below max, but no backoff.
        _, _, failures = build(self.tmp, times_s=list(range(0, 200, 10)))
        self.assertTrue(any("below the policy wait" in f for f in failures), failures)

    def test_attempt_after_lifetime_fails(self):
        _, _, failures = build(self.tmp, times_s=GOOD_TIMES_S + [230])
        self.assertTrue(any("after the lifetime" in f for f in failures), failures)

    def test_early_expiry_fails(self):
        _, _, failures = build(self.tmp, times_s=GOOD_TIMES_S[:-1])
        self.assertTrue(any("expired early" in f for f in failures), failures)

    def test_dsn_with_sender_fails(self):
        ok, details, failures = build(self.tmp, dsn_from="postmaster@engine.mtaproof.test")
        self.assertFalse(ok)
        self.assertEqual(details["dsn_null_sender"], 0)
        self.assertTrue(any("MAIL FROM:<>" in f for f in failures), failures)

    def test_dsn_that_is_not_a_report_fails(self):
        _, _, failures = build(self.tmp, dsn_body=b"Subject: hi\r\n\r\nhello\r\n")
        self.assertTrue(any("not a delivery-status report" in f for f in failures), failures)

    def test_wait_schedule(self):
        self.assertEqual([backoff_expiry.wait_ms(POLICY, k) // 1000 for k in range(1, 5)],
                         [10, 20, 40, 40])


if __name__ == "__main__":
    unittest.main()
