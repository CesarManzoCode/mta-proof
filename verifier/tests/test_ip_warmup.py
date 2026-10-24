import unittest

from verifier.props import ip_warmup

DAY, HOUR = ip_warmup.DAY_MS, ip_warmup.HOUR_MS
DAILY = [2, 3, 4, 5, 6, 7, 8]
HOURLY = [None, None, None, 1, 1, 2, 2]
START = {"127.0.2.1": 1, "127.0.2.2": 3, "127.0.2.3": 5, "127.0.2.100": 2}
KNOWN = ["127.0.2.1", "127.0.2.2", "127.0.2.3"]
PLAN = {ip: {"daily": DAILY, "hourly": HOURLY, "start_day": d} for ip, d in START.items()}


def trace():
    """Every IP sends exactly its daily cap on each of the seven run days, one
    data_end per hour; the unknown IP gets one 421 a day."""
    out = []

    def add(ev, ip, at, **kw):
        out.append({"seq": len(out) + 1, "at_ms": at, "conn": 1, "src_ip": ip, "ev": ev, **kw})

    for day in range(7):
        for ip in sorted(PLAN):
            cap = ip_warmup.cap_at(DAILY, START[ip], day)
            for i in range(cap):
                add("data_end", ip, day * DAY + i * HOUR, code=250, msg_id=f"{ip}-{day}-{i}")
            if ip not in KNOWN:
                add("reply", ip, day * DAY + 23 * HOUR, code=421, text="[IPTS04] Message deferred")
    return out


class IPWarmupTest(unittest.TestCase):
    def test_plan_index(self):
        self.assertEqual(ip_warmup.cap_at(DAILY, 3, 0), 4)
        self.assertEqual(ip_warmup.cap_at(DAILY, 3, 10), 8)
        self.assertIsNone(ip_warmup.cap_at(HOURLY, 1, 0))
        self.assertEqual(ip_warmup.cap_at(HOURLY, 2, 2), 1)

    def test_within_caps_passes(self):
        ok, details, failures = ip_warmup.check(trace(), PLAN, KNOWN, 0.05)
        self.assertTrue(ok, failures)
        self.assertEqual(details["over_daily"], 0)
        self.assertEqual(details["over_hourly"], 0)
        self.assertEqual(details["throttle_rate_known"], 0.0)
        self.assertGreater(details["throttle_rate_unknown"], 0.0)
        self.assertEqual(details["known_ips"], KNOWN)
        self.assertEqual(details["plan"], PLAN)

    def test_over_hourly_cap_fails(self):
        # 127.0.2.3 is on plan day 5 on run day 0: hourly cap 1, already used in hour 0.
        t = trace() + [{"seq": 10_000, "at_ms": 1, "conn": 2, "src_ip": "127.0.2.3", "ev": "data_end",
                        "code": 250, "msg_id": "extra"}]
        ok, details, failures = ip_warmup.check(t, PLAN, KNOWN, 0.05)
        self.assertFalse(ok)
        self.assertEqual(details["over_hourly"], 1)
        self.assertEqual(details["over_daily"], 1)
        self.assertIn("127.0.2.3 sent 2 in hour 0 (run day 0), hourly cap 1", failures)

    def test_unknown_not_throttled_more_fails(self):
        t = [r for r in trace() if r["ev"] != "reply"]
        ok, _, failures = ip_warmup.check(t, PLAN, KNOWN, 0.05)
        self.assertFalse(ok)
        self.assertTrue(any("is not above" in f for f in failures), failures)

    def test_spread_off_plan_fails(self):
        # Drop half of 127.0.2.3's sends on run day 6: its share falls below the plan.
        t = [r for r in trace() if not (r["src_ip"] == "127.0.2.3" and r["ev"] == "data_end"
                                        and r["at_ms"] // DAY == 6 and r["msg_id"].endswith(("-4", "-5", "-6", "-7")))]
        ok, details, failures = ip_warmup.check(t, PLAN, KNOWN, 0.05)
        self.assertFalse(ok)
        self.assertGreater(details["max_share_diff"], 0.05)

    def test_missing_days_fails(self):
        t = [r for r in trace() if r["at_ms"] < 6 * DAY]
        ok, _, failures = ip_warmup.check(t, PLAN, KNOWN, 0.05)
        self.assertFalse(ok)
        self.assertTrue(any("run days [6]" in f for f in failures), failures)


if __name__ == "__main__":
    unittest.main()
