import unittest

from verifier.props import destination_isolation as iso

SLOW = "slow.test"


def run(other_ms, slow_ms, n=100):
    """n messages to fast.test taking other_ms[i % len] ms and n to SLOW taking slow_ms."""
    events = []

    def add(at, kind, m, domain, detail=None):
        events.append({"seq": len(events) + 1, "at_ms": at, "kind": kind, "msg_id": m,
                       "domain": domain, "tenant": "acme", "detail": detail or {}})

    for i in range(n):
        for domain, ms in (("fast.test", other_ms[i % len(other_ms)]), (SLOW, slow_ms)):
            m = f"{domain}-{i}"
            add(i * 1000, "accepted", m, domain)
            if ms is not None:
                add(i * 1000 + ms, "delivered", m, domain)
    return events


class DestinationIsolationTest(unittest.TestCase):
    def test_percentile_nearest_rank(self):
        values = list(range(1, 101))
        self.assertEqual(iso.percentile(values, 50), 50)
        self.assertEqual(iso.percentile(values, 99), 99)
        self.assertEqual(iso.percentile([7], 99), 7)
        self.assertEqual(iso.percentile([3, 1, 2], 50), 2)

    def test_isolated_passes(self):
        ok, details, failures = iso.check(run([200, 210], 0), run([200, 220], 60_000), SLOW, 1.10)
        self.assertTrue(ok, failures)
        self.assertEqual(details["baseline"], {"p50_ms": 200, "p99_ms": 210, "messages": 100})
        self.assertEqual(details["throttled"], {"p50_ms": 200, "p99_ms": 220, "messages": 100})
        self.assertEqual(details["throttled_domain"], SLOW)
        self.assertEqual(details["margin"], 1.10)

    def test_degraded_p99_fails(self):
        ok, _, failures = iso.check(run([200, 200], 0), run([200] * 98 + [300, 300], 60_000), SLOW, 1.10)
        self.assertFalse(ok)
        self.assertTrue(any(f.startswith("p99_ms") for f in failures), failures)

    def test_degraded_p50_fails(self):
        ok, _, failures = iso.check(run([200], 0), run([400], 60_000), SLOW, 1.10)
        self.assertFalse(ok)
        self.assertTrue(any(f.startswith("p50_ms") for f in failures), failures)

    def test_undelivered_other_fails(self):
        ok, _, failures = iso.check(run([200], 0), run([200] * 99 + [None], 60_000), SLOW, 1.10)
        self.assertFalse(ok)
        self.assertTrue(any("not delivered" in f for f in failures), failures)

    def test_unthrottled_variant_fails(self):
        ok, _, failures = iso.check(run([200], 0), run([200], 0), SLOW, 1.10)
        self.assertFalse(ok)
        self.assertTrue(any("does not slow it" in f for f in failures), failures)

    def test_different_load_fails(self):
        ok, _, failures = iso.check(run([200], 0), run([200], 60_000, n=90), SLOW, 1.10)
        self.assertFalse(ok)
        self.assertTrue(any("not equivalent" in f for f in failures), failures)

    def test_throttled_domain_from_variant(self):
        sc = {"variants": {"throttled": {"engine": {"set": {"limits": {"domain": {SLOW: {"connections": 1}}}}}}}}
        self.assertEqual(iso.throttled_domain(sc), SLOW)


if __name__ == "__main__":
    unittest.main()
