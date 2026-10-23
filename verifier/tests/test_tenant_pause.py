import unittest

from verifier.props import tenant_pause

THRESHOLD, WINDOW, BOUND_S, MARGIN = 0.25, 4, 60, 1.10


class Run:
    """A synthetic event log: one call per event, at_ms in seconds for brevity."""

    def __init__(self):
        self.events = []
        self.n = 0

    def add(self, at_s, tenant, kind, msg_id=None, detail=None):
        self.events.append({"seq": len(self.events) + 1, "at_ms": int(at_s * 1000), "tenant": tenant,
                            "kind": kind, "msg_id": msg_id, "domain": None, "detail": detail or {}})

    def message(self, at_s, tenant, result):
        self.n += 1
        m = f"m{self.n:08d}"
        self.add(at_s, tenant, "accepted", m)
        self.add(at_s, tenant, "attempt_started", m)
        self.add(at_s, tenant, result, m)
        return m


def runs(pause_delay_s=0, attempt_while_paused=False, others_while_paused=True, resume=True):
    """bad crosses at t=4 (2 of 4 bounced); good delivers one message per second."""
    bad, base = Run(), Run()
    for r, results in ((bad, ["bounced", "delivered", "bounced", "delivered"]),
                       (base, ["delivered"] * 4)):
        for i, res in enumerate(results, 1):
            r.message(i, "bad", res)
    bad.add(4 + pause_delay_s, "bad", "tenant_paused",
            detail={"bounce_rate": 0.5, "window": WINDOW, "threshold": THRESHOLD})
    held = [f"held{i}" for i in range(3)]
    for r in (bad, base):
        for m in held:
            r.add(10, "bad", "accepted", m)
    if attempt_while_paused:
        bad.add(20, "bad", "attempt_started", held[0])
    for t in range(5, 300):
        for r in (bad, base):
            if r is base or others_while_paused:
                r.add(t, "good", "accepted", f"g{t}")
                r.add(t, "good", "delivered", f"g{t}")
    if resume:
        bad.add(304, "bad", "tenant_resumed", detail={"paused_at_ms": 4000})
        for m in held:
            bad.add(305, "bad", "attempt_started", m)
            bad.add(305, "bad", "delivered", m)
    for m in held:
        base.add(10, "bad", "attempt_started", m)
        base.add(10, "bad", "delivered", m)
    return bad.events, base.events


def check(bad, base):
    return tenant_pause.check(bad, base, THRESHOLD, WINDOW, BOUND_S, MARGIN)


class TenantPauseTest(unittest.TestCase):
    def test_pause_passes(self):
        ok, details, failures = check(*runs())
        self.assertTrue(ok, failures)
        self.assertEqual(details["tenant"], "bad")
        self.assertEqual(details["rate_ratio_others"], 1.0)
        self.assertEqual(details["max_pause_delay_ms"], 0)
        self.assertEqual(details["paused_ms"], 300_000)
        self.assertEqual(details["lost"], 0)
        for key in ("threshold", "window", "pause_bound_s"):
            self.assertIn(key, details)

    def test_window_must_be_full(self):
        r = Run()
        for i in range(3):
            r.message(i, "bad", "bounced")
        pauses, failures = tenant_pause.monitor(r.events, THRESHOLD, WINDOW, BOUND_S * 1000)
        self.assertEqual((pauses, failures), ([], []))
        r.add(4, "bad", "tenant_paused", detail={"bounce_rate": 1.0, "window": WINDOW, "threshold": THRESHOLD})
        _, failures = tenant_pause.monitor(r.events, THRESHOLD, WINDOW, BOUND_S * 1000)
        self.assertTrue(any("without crossing" in f for f in failures), failures)

    def test_threshold_is_strict(self):
        r = Run()
        for res in ("bounced", "delivered", "delivered", "delivered"):
            r.message(1, "bad", res)
        _, failures = tenant_pause.monitor(r.events, THRESHOLD, WINDOW, BOUND_S * 1000)
        self.assertEqual(failures, [])  # 1/4 is not above 0.25: no pause owed

    def test_late_pause_fails(self):
        ok, _, failures = check(*runs(pause_delay_s=61))
        self.assertFalse(ok)
        self.assertTrue(any("bound 60000 ms" in f for f in failures), failures)

    def test_missing_pause_fails(self):
        bad, base = runs()
        bad = [e for e in bad if e["kind"] not in ("tenant_paused", "tenant_resumed")]
        ok, _, failures = check(bad, base)
        self.assertFalse(ok)
        self.assertTrue(any("was not paused" in f for f in failures), failures)

    def test_attempt_while_paused_fails(self):
        ok, _, failures = check(*runs(attempt_while_paused=True))
        self.assertFalse(ok)
        self.assertTrue(any("attempt of held0 while bad is paused" in f for f in failures), failures)

    def test_wrong_detail_fails(self):
        bad, base = runs()
        for e in bad:
            if e["kind"] == "tenant_paused":
                e["detail"] = {"bounce_rate": 0.5, "window": 200, "threshold": 0.04}
        ok, _, failures = check(bad, base)
        self.assertFalse(ok)
        self.assertTrue(any("detail has threshold" in f for f in failures), failures)

    def test_others_slowed_fails(self):
        ok, details, failures = check(*runs(others_while_paused=False))
        self.assertFalse(ok)
        self.assertEqual(details["rate_ratio_others"], 0.0)

    def test_never_resumed_loses_messages(self):
        ok, details, failures = check(*runs(resume=False))
        self.assertFalse(ok)
        self.assertEqual(details["lost"], 3)

    def test_pause_in_baseline_fails(self):
        bad, _ = runs()
        ok, _, failures = check(bad, bad)
        self.assertFalse(ok)
        self.assertTrue(any(f.startswith("baseline:") for f in failures), failures)


if __name__ == "__main__":
    unittest.main()
