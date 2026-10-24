import email
import email.policy
import os
import tempfile
import unittest

from verifier.props import external_validation as ev

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
INTERVALS = [5.0, 10.0]
ADAPTATION = {"baseline": 0, "always-defer": 0, "gmail-model": 300}
SRC = ev.SRC_IP


def dsn_bytes(rcpt):
    return (
        "From: MAILER-DAEMON@haraka.test\r\nTo: retry@baseline.test\r\n"
        "Content-Type: multipart/report; report-type=delivery-status; boundary=\"b\"\r\n\r\n"
        "--b\r\nContent-Type: text/plain\r\n\r\nfailed\r\n"
        "--b\r\nContent-Type: message/delivery-status\r\n\r\n"
        "Reporting-MTA: dns;haraka.test\r\n\r\n"
        f"Final-Recipient: rfc822;{rcpt}\r\nAction: failed\r\nStatus: 4.3.0\r\n\r\n"
        "--b--\r\n").encode()


class Log:
    """A synthetic receiver.jsonl and its captured DSNs."""

    def __init__(self):
        self.lines, self.emls, self.conn = [], {}, 0

    def rec(self, at_ms, ev_, conn, **fields):
        self.lines.append(dict({"seq": len(self.lines) + 1, "at_ms": at_ms, "ev": ev_,
                                "conn": conn}, **fields))

    def txn(self, at_ms, profile, mail_from, rcpt, code, stage="data_end", src=SRC, sha=None):
        """One connection with one transaction answered at stage with code."""
        self.conn += 1
        c = self.conn
        self.rec(at_ms, "connect", c, profile=profile, src_ip=src)
        self.rec(at_ms, "mail", c, mail_from=mail_from)
        self.rec(at_ms, "rcpt", c, rcpt=rcpt)
        if stage == "rcpt":
            self.rec(at_ms, "reply", c, code=code)
        elif stage == "drop":
            self.rec(at_ms, "reply", c, code=250)
            self.rec(at_ms, "drop", c, rule="x")
            return
        else:
            self.rec(at_ms, "reply", c, code=250)
            self.rec(at_ms, "data_end", c, code=code, mail_from=mail_from, rcpt=rcpt, sha256=sha)
        self.rec(at_ms, "close", c)

    def dsn(self, at_ms, to, about):
        sha = f"dsn-{about}"
        self.emls[sha] = email.message_from_bytes(dsn_bytes(about), policy=email.policy.default)
        self.txn(at_ms, "baseline", "", to, 250, sha=sha)

    def read(self, sha):
        return self.emls.get(sha)


def passing():
    """A run where Haraka behaves: the retry schedule, a 5xx, a 2xx and a
    retried 4xx, with the gmail-model profile seen past its adaptation."""
    log = Log()
    retry = "retry0@always-defer.test"
    for t in (0, 5600, 16900):
        log.txn(t, "always-defer", "retry@baseline.test", retry, 451)
    log.dsn(17000, "retry@baseline.test", retry)
    log.txn(100, "gmail-model", "notify@baseline.test", "nouser0@gmail-model.test", 550, stage="rcpt")
    log.dsn(200, "notify@baseline.test", "nouser0@gmail-model.test")
    log.txn(300, "baseline", "notify@baseline.test", "user0@baseline.test", 250)
    log.txn(400, "gmail-model", "notify@baseline.test", "user1@gmail-model.test", 421, stage="rcpt")
    log.txn(400_000, "gmail-model", "notify@baseline.test", "user1@gmail-model.test", 250)
    injected = [
        {"i": 0, "kind": "retry", "from": "retry@baseline.test", "to": retry},
        {"i": 1, "kind": "nouser", "from": "notify@baseline.test", "to": "nouser0@gmail-model.test"},
        {"i": 2, "kind": "load", "from": "notify@baseline.test", "to": "user0@baseline.test"},
        {"i": 3, "kind": "load", "from": "notify@baseline.test", "to": "User1@gmail-model.test"},
    ]
    return log, injected


def run_check(log, injected, intervals=INTERVALS):
    lines = sorted(log.lines, key=lambda r: (r["at_ms"], r["seq"]))
    return ev.check(lines, injected, intervals, ADAPTATION, log.read)


class CheckTest(unittest.TestCase):
    def test_passing_run(self):
        log, injected = passing()
        ok, details, failures = run_check(log, injected)
        self.assertEqual(failures, [])
        self.assertTrue(ok)
        self.assertEqual(details["evaluated"], list(ev.EVALUATED))
        self.assertEqual(set(details["not_evaluated"]), set(ev.NOT_EVALUATED))
        self.assertTrue(all(details["not_evaluated"].values()))
        self.assertFalse(set(details["evaluated"]) & set(details["not_evaluated"]))
        b = details["results"]["backoff_expiry"]
        self.assertEqual((b["expired"], b["dsn_null_sender"], b["max_interval_s"]), (1, 1, 11.3))
        p = details["results"]["profile_behavior"]["profiles"]
        self.assertEqual(p["gmail-model"]["rejected"], 1)
        self.assertEqual(p["gmail-model"]["accepted"], 1)
        self.assertEqual(details["results"]["receiver_limits"]["violations"]["gmail-model"], {SRC: 0})

    def test_counted_violation_fails(self):
        log, injected = passing()
        log.rec(400_001, "violation", log.conn, profile="gmail-model", src_ip=SRC,
                rule="gmail.rate_ip", counted=True)
        ok, details, failures = run_check(log, injected)
        self.assertFalse(ok)
        self.assertEqual(details["results"]["receiver_limits"]["violations"]["gmail-model"][SRC], 1)
        self.assertEqual(details["results"]["receiver_limits"]["verdict"], "fail")

    def test_run_shorter_than_adaptation_fails(self):
        log, injected = passing()
        log.lines = [r for r in log.lines if r["at_ms"] < 400_000]
        log.txn(200_000, "gmail-model", "notify@baseline.test", "user1@gmail-model.test", 250)
        ok, _, failures = run_check(log, injected)
        self.assertFalse(ok)
        self.assertTrue(any("adaptation period" in f for f in failures), failures)

    def test_retry_too_early_fails(self):
        log, injected = passing()
        log.lines = [r for r in log.lines if not (r.get("conn") == 2)]
        log.txn(4000, "always-defer", "retry@baseline.test", "retry0@always-defer.test", 451)
        ok, _, failures = run_check(log, injected)
        self.assertFalse(ok)
        self.assertTrue(any("before its 5000 ms" in f for f in failures), failures)

    def test_retry_too_late_fails(self):
        log, injected = passing()
        ok, _, failures = run_check(log, injected, intervals=[5.0, 8.0])
        self.assertFalse(ok)
        self.assertTrue(any("more than 8000" in f for f in failures), failures)

    def test_extra_attempt_and_missing_dsn_fail(self):
        log, injected = passing()
        log.lines = [r for r in log.lines
                     if not (r.get("sha256") == "dsn-retry0@always-defer.test")]
        log.txn(40_000, "always-defer", "retry@baseline.test", "retry0@always-defer.test", 451)
        ok, _, failures = run_check(log, injected)
        self.assertFalse(ok)
        self.assertTrue(any("4 attempts, want 3" in f for f in failures), failures)
        self.assertTrue(any("no DSN" in f for f in failures), failures)

    def test_retry_after_5xx_fails(self):
        log, injected = passing()
        log.txn(500, "gmail-model", "notify@baseline.test", "nouser0@gmail-model.test", 550, stage="rcpt")
        ok, _, failures = run_check(log, injected)
        self.assertFalse(ok)
        self.assertTrue(any("after the final 550" in f for f in failures), failures)

    def test_resend_after_2xx_fails(self):
        log, injected = passing()
        log.txn(600, "baseline", "notify@baseline.test", "user0@baseline.test", 250)
        ok, _, failures = run_check(log, injected)
        self.assertFalse(ok)
        self.assertTrue(any("after the final 250" in f for f in failures), failures)

    def test_message_without_end_fails(self):
        log, injected = passing()
        injected.append({"i": 4, "kind": "load", "from": "notify@baseline.test",
                         "to": "user9@yahoo-model.test"})
        log.txn(700, "yahoo-model", "notify@baseline.test", "user9@yahoo-model.test", None, stage="drop")
        ok, details, failures = run_check(log, injected)
        self.assertFalse(ok)
        self.assertTrue(any("no final answer" in f for f in failures), failures)
        self.assertEqual(details["results"]["profile_behavior"]["profiles"]["yahoo-model"]["answers"],
                         {"no_reply": 1})

    def test_dsn_for_delivered_message_fails(self):
        log, injected = passing()
        log.dsn(800, "notify@baseline.test", "user0@baseline.test")
        ok, _, failures = run_check(log, injected)
        self.assertFalse(ok)
        self.assertTrue(any("reported failed in a DSN" in f for f in failures), failures)

    def test_dsn_transactions_are_not_attempts(self):
        log, _ = passing()
        atts, dsns = ev.attempts(log.lines, log.read)
        self.assertNotIn("retry@baseline.test", atts)
        self.assertEqual([d["action"] for d in dsns["retry0@always-defer.test"]], ["failed"])


class ConfigTest(unittest.TestCase):
    def test_parse_intervals(self):
        self.assertEqual(ev.parse_intervals("5s, 1m*2,1h"), [5, 60, 60, 3600])
        for bad in ("4s", "5", "5x", "", "5s*0"):
            with self.assertRaises(ValueError):
                ev.parse_intervals(bad)

    def test_project_config(self):
        cfg = os.path.join(ROOT, ev.HARAKA_DIR, "config")
        out = ev.read_outbound(cfg)
        self.assertEqual(out["local_mx_ok"], "true")
        self.assertTrue(all(s >= 5 for s in ev.parse_intervals(out["temp_fail_intervals"])))
        self.assertEqual(ev.pinned_version(os.path.join(ROOT, ev.HARAKA_DIR)), "3.3.5")
        digest, files = ev.config_digest(cfg)
        self.assertEqual(len(digest), 64)
        self.assertIn("outbound.ini", files)

    def test_config_digest_changes_with_content(self):
        with tempfile.TemporaryDirectory() as tmp:
            with open(os.path.join(tmp, "a"), "w") as f:
                f.write("1")
            d1, _ = ev.config_digest(tmp)
            with open(os.path.join(tmp, "a"), "w") as f:
                f.write("2")
            self.assertNotEqual(d1, ev.config_digest(tmp)[0])

    def test_not_installed(self):
        with tempfile.TemporaryDirectory() as tmp:
            why = ev.not_installed(tmp)
            self.assertIn("not installed", why)
            self.assertIn("make deps", why)

    def test_plan_is_seeded(self):
        spec = {"tenants": [{"from": "a@baseline.test", "weight": 1}],
                "domains": [{"domain": "baseline.test", "weight": 1}, {"domain": "gmail-model.test", "weight": 1}]}
        a = ev.plan(spec, {"campaign_messages": 20}, 1)
        self.assertEqual(a, ev.plan(spec, {"campaign_messages": 20}, 1))
        self.assertEqual(len(a), ev.RETRY_MESSAGES + ev.NOUSER_MESSAGES + 20)
        self.assertEqual(len({m["to"] for m in a}), len(a))
        self.assertEqual(len({m["msg_id"] for m in a}), len(a))


if __name__ == "__main__":
    unittest.main()
