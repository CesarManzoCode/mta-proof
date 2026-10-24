import unittest

from verifier.props import outbound_auth

GOOD, BROKEN = outbound_auth.GOOD_DOMAIN, outbound_auth.BROKEN_DOMAIN
PASS = {"spf": "pass", "dkim": "pass", "dmarc": "pass"}
FAIL = {"spf": "fail", "dkim": "fail", "dmarc": "fail"}
REJECT_515 = dict(code=550, enhanced="5.7.515",
                  text=f"Access denied, sending domain {BROKEN} does not meet the required authentication level.")


class Run:
    """A synthetic run: engine events and receiver lines."""

    def __init__(self):
        self.events, self.receiver = [], []
        self.ids = {"good": [], "broken": []}

    def ev(self, msg_id, kind, cls=None, subtype=None, rule_id=None):
        self.events.append({"seq": len(self.events) + 1, "at_ms": 0, "msg_id": msg_id, "kind": kind,
                            "class": cls, "subtype": subtype, "rule_id": rule_id, "detail": {}})

    def data_end(self, msg_id, domain, auth, **reply):
        self.receiver.append(dict({"seq": len(self.receiver) + 1, "at_ms": 0, "ev": "data_end",
                                   "mail_from": f"notify@{domain}", "msg_id": msg_id, "auth": auth}, **reply))

    def good(self, m):
        self.ids["good"].append(m)
        self.ev(m, "accepted")
        self.ev(m, "attempt_started")
        self.data_end(m, GOOD, PASS, code=250, enhanced="2.0.0", text="OK")
        self.ev(m, "delivered")

    def broken(self, m):
        self.ids["broken"].append(m)
        self.ev(m, "accepted")
        self.ev(m, "attempt_started")
        self.data_end(m, BROKEN, FAIL, **REJECT_515)
        self.ev(m, "classified", "permanent", "AuthConfig", "microsoft.auth_level")
        self.ev(m, "bounced", "permanent", "AuthConfig", "microsoft.auth_level")


def make_run():
    r = Run()
    for i in range(3):
        r.good(f"g{i}")
        r.broken(f"b{i}")
    return r


def check(r):
    return outbound_auth.check(r.events, r.receiver, r.ids)


class OutboundAuthTest(unittest.TestCase):
    def test_well_configured_and_broken_pass(self):
        ok, details, failures = check(make_run())
        self.assertTrue(ok, failures)
        self.assertEqual(details["good_domain"], GOOD)
        self.assertEqual(details["broken_domain"], BROKEN)
        self.assertEqual(details["auth"]["good"], {"pass/pass/pass": 3})
        self.assertEqual(details["broken_replies"], ["550 5.7.515"])
        self.assertEqual(details["broken_attempts"], 3)

    def test_second_attempt_after_515_fails(self):
        # The engine treats 5.7.515 as transient: a deferral and another try.
        r = make_run()
        r.events = [e for e in r.events if not (e["msg_id"] == "b1" and e["kind"] == "bounced")]
        r.ev("b1", "deferred", "permanent", "AuthConfig", "microsoft.auth_level")
        r.ev("b1", "attempt_started")
        r.data_end("b1", BROKEN, FAIL, **REJECT_515)
        r.ev("b1", "classified", "permanent", "AuthConfig", "microsoft.auth_level")
        r.ev("b1", "bounced", "permanent", "AuthConfig", "microsoft.auth_level")
        ok, details, failures = check(r)
        self.assertFalse(ok)
        self.assertTrue(any("2 attempts" in f and "2 data_end" in f for f in failures), failures)
        self.assertTrue(any("deferred" in f for f in failures), failures)
        self.assertEqual(details["broken_attempts"], 4)

    def test_good_message_without_dkim_fails(self):
        r = make_run()
        r.receiver[0]["auth"] = {"spf": "pass", "dkim": "fail", "dmarc": "pass"}
        ok, _, failures = check(r)
        self.assertFalse(ok)
        self.assertIn("auth", failures[0])

    def test_broken_accepted_or_other_code_fails(self):
        r = make_run()
        for line in r.receiver:
            if line["msg_id"] == "b0":
                line.update(code=550, enhanced="5.7.509")
        ok, _, failures = check(r)
        self.assertFalse(ok)
        self.assertTrue(any("want 550 5.7.515" in f for f in failures), failures)

    def test_wrong_classification_fails(self):
        r = make_run()
        for e in r.events:
            if e["msg_id"] == "b2" and e["kind"] == "classified":
                e.update({"class": "undetermined", "subtype": None})
        ok, _, failures = check(r)
        self.assertFalse(ok)
        self.assertTrue(any("permanent/AuthConfig" in f for f in failures), failures)

    def test_broken_that_passes_a_check_fails(self):
        r = make_run()
        for line in r.receiver:
            if line["msg_id"] == "b0":
                line["auth"] = {"spf": "pass", "dkim": "fail", "dmarc": "fail"}
        ok, _, failures = check(r)
        self.assertFalse(ok)

    def test_no_messages_fails(self):
        ok, _, failures = outbound_auth.check([], [], {"good": [], "broken": []})
        self.assertFalse(ok)
        self.assertEqual(len(failures), 2)


if __name__ == "__main__":
    unittest.main()
