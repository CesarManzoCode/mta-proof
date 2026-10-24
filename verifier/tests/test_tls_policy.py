import copy
import unittest

from verifier.props import tls_policy

REQ, OPP = tls_policy.REQUIRED_DOMAIN, tls_policy.OPPORTUNISTIC_DOMAIN
TLS_ON = {"version": "TLS 1.3", "cipher": "TLS_AES_128_GCM_SHA256"}


class Run:
    """A synthetic run: the engine events and receiver lines of one certificate."""

    def __init__(self):
        self.events, self.receiver = [], []

    def ev(self, msg_id, kind, domain, detail=None, subtype=None):
        self.events.append({"seq": len(self.events) + 1, "at_ms": 0, "msg_id": msg_id, "kind": kind,
                            "domain": domain, "subtype": subtype, "detail": detail or {}})

    def rec(self, ev, **fields):
        self.receiver.append(dict({"seq": len(self.receiver) + 1, "at_ms": 0, "ev": ev}, **fields))


def negotiated(policy, verified, version, error):
    return {"conn": 1, "addr": "127.0.1.5:25", "policy": policy, "version": version,
            "cipher": TLS_ON["cipher"] if version else None, "verified": verified, "error": error,
            "fallback": False}


def make_run(cert):
    """A run that behaves as D16 says for cert: two messages per domain."""
    r = Run()
    valid = cert == tls_policy.VALID
    err = None if valid else "tls: failed to verify certificate: x509: certificate signed by unknown authority"
    for i, domain in enumerate((OPP, OPP, REQ, REQ)):
        m = f"m{i}"
        r.ev(m, "accepted", domain)
        r.ev(m, "attempt_started", domain)
        policy = "required" if domain == REQ else "opportunistic"
        up = valid or domain == OPP
        r.ev(m, "tls_negotiated", domain, negotiated(policy, valid, TLS_ON["version"] if up else None, err))
        if up:
            r.rec("starttls", tls=TLS_ON)
            r.rec("rcpt", rcpt=f"user{i}@{domain}")
            r.rec("data_end", msg_id=m, code=250, tls=TLS_ON)
            r.ev(m, "delivered", domain)
        else:
            r.rec("starttls", tls={"error": "remote error: tls: bad certificate"})
            r.ev(m, "deferred", domain, subtype="TLSPolicy")
    return r


def runs(**changed):
    out = {c: make_run(c) for c in tls_policy.CERTS}
    out.update(changed)
    return {c: (r.events, r.receiver) for c, r in out.items()}


class TLSPolicyTest(unittest.TestCase):
    def test_policies_as_d16_pass(self):
        ok, details, failures = tls_policy.check(runs())
        self.assertTrue(ok, failures)
        self.assertEqual(details["required_domain"], REQ)
        self.assertEqual(details["opportunistic_domain"], OPP)
        self.assertEqual(details["results"]["other-ca"]["delivered"], {REQ: 0, OPP: 2})
        self.assertEqual(details["results"]["valid"]["delivered"], {REQ: 2, OPP: 2})
        self.assertEqual(details["results"]["wrong-name"]["deferred_tls_policy"], 2)

    def test_required_delivered_with_invalid_certificate_fails(self):
        r = make_run("wrong-name")
        r.ev("m2", "delivered", REQ)
        r.rec("rcpt", rcpt=f"user2@{REQ}")
        r.rec("data_end", msg_id="m2", code=250, tls=TLS_ON)
        ok, _, failures = tls_policy.check(runs(**{"wrong-name": r}))
        self.assertFalse(ok)
        self.assertTrue(any("RCPT to the required domain" in f for f in failures), failures)
        self.assertTrue(any("m2 to the required domain delivered" in f for f in failures), failures)

    def test_required_without_tls_policy_deferral_fails(self):
        r = make_run("other-ca")
        r.events = [e for e in r.events if not (e["kind"] == "deferred" and e["msg_id"] == "m3")]
        ok, _, failures = tls_policy.check(runs(**{"other-ca": r}))
        self.assertFalse(ok)
        self.assertTrue(any("m3" in f and "TLSPolicy" in f for f in failures), failures)

    def test_opportunistic_not_delivered_fails(self):
        r = make_run("other-ca")
        r.events = [e for e in r.events if not (e["kind"] == "delivered" and e["msg_id"] == "m0")]
        ok, _, failures = tls_policy.check(runs(**{"other-ca": r}))
        self.assertFalse(ok)
        self.assertIn("other-ca: m0 to baseline.test not delivered", failures)

    def test_plain_text_acceptance_fails(self):
        r = make_run("valid")
        for line in r.receiver:
            if line["ev"] == "data_end" and line["msg_id"] == "m3":
                del line["tls"]
        ok, _, failures = tls_policy.check(runs(valid=r))
        self.assertFalse(ok)
        self.assertIn(f"valid: m3 to {REQ} accepted by the receiver without TLS", failures)

    def test_invalid_certificate_reported_verified_fails(self):
        r = make_run("other-ca")
        for e in r.events:
            if e["kind"] == "tls_negotiated" and e["domain"] == OPP:
                e["detail"] = negotiated("opportunistic", True, TLS_ON["version"], None)
        ok, _, failures = tls_policy.check(runs(**{"other-ca": r}))
        self.assertFalse(ok)
        self.assertTrue(any("verified an invalid certificate" in f for f in failures), failures)

    def test_valid_certificate_not_verified_fails(self):
        r = make_run("valid")
        bad = copy.deepcopy(r.events)
        for e in bad:
            if e["kind"] == "tls_negotiated" and e["domain"] == REQ:
                e["detail"]["verified"] = False
        r.events = bad
        ok, _, failures = tls_policy.check(runs(valid=r))
        self.assertFalse(ok)
        self.assertTrue(any("with the valid certificate" in f for f in failures), failures)

    def test_receiver_must_back_the_failed_handshake(self):
        r = make_run("wrong-name")
        r.receiver = [x for x in r.receiver if not (x["ev"] == "starttls" and "error" in x["tls"])]
        ok, _, failures = tls_policy.check(runs(**{"wrong-name": r}))
        self.assertFalse(ok)
        self.assertIn("wrong-name: the receiver logged no failed handshake for the required policy", failures)

    def test_missing_run_and_missing_negotiation_fail(self):
        r = make_run("valid")
        r.events = [e for e in r.events if e["kind"] != "tls_negotiated"]
        all_runs = runs(valid=r)
        del all_runs["other-ca"]
        ok, _, failures = tls_policy.check(all_runs)
        self.assertFalse(ok)
        self.assertIn("no run with the other-ca certificate", failures)
        self.assertIn(f"valid: no tls_negotiated event for {REQ}", failures)


if __name__ == "__main__":
    unittest.main()
