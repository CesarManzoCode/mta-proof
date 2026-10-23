"""debug_timeline and debug_aggregates on synthetic runs: mtaq's output is
written by hand, as the tool prints it (contract 4.2)."""

import copy
import os
import tempfile
import unittest

from verifier import formats
from verifier.props import debug_aggregates, debug_timeline
from verifier.tests import fixtures


def ev(seq, at, msg_id, kind, detail=None, **cols):
    e = {c: None for c in formats.EVENT_COLUMNS}
    e.update(seq=seq, at_ms=at, msg_id=msg_id, kind=kind, tenant="acme", domain="gmail-model.test",
             detail=detail or {}, **cols)
    return e


def attempt(n, started, result, ended=None, stage=None, code=None, enhanced=None):
    return {"attempt": n, "started_at_ms": started, "ended_at_ms": ended, "result": result,
            "stage": stage, "code": code, "enhanced": enhanced, "src_ip": "127.0.2.1",
            "mx_host": "mx1.gmail-model.test", "tls": []}


def data_end(seq, at, msg_id, code, enhanced=""):
    return {"seq": seq, "at_ms": at, "conn": seq, "ev": "data_end", "msg_id": msg_id, "code": code,
            "enhanced": enhanced}


class DebugTimelineTest(unittest.TestCase):
    def setUp(self):
        # m1: 451 at data_end, then delivered. m2: killed after the receiver's
        # 250 (post_250), recovered, delivered again. m3: refused at RCPT.
        self.events = [
            ev(1, 10, "m1", "accepted"),
            ev(2, 11, "m1", "attempt_started", attempt=1),
            ev(3, 12, "m1", "deferred", {"stage": "data_end"}, attempt=1, code=451, enhanced="4.3.0"),
            ev(4, 20, "m1", "attempt_started", attempt=2),
            ev(5, 21, "m1", "delivered", attempt=2, code=250, enhanced="2.0.0"),
            ev(6, 30, "m2", "accepted"),
            ev(7, 31, "m2", "attempt_started", attempt=1),
            ev(8, 40, "m2", "recovered", {"from_state": "inflight"}, attempt=1),
            ev(9, 41, "m2", "attempt_started", attempt=2),
            ev(10, 42, "m2", "delivered", attempt=2, code=250, enhanced="2.0.0"),
            ev(11, 50, "m3", "accepted"),
            ev(12, 51, "m3", "attempt_started", attempt=1),
            ev(13, 52, "m3", "bounced", {"stage": "rcpt"}, attempt=1, code=550, enhanced="5.1.1"),
        ]
        by = {}
        for e in self.events:
            by.setdefault(e["msg_id"], []).append(e)
        self.timelines = [
            {"msg_id": "m1", "state": "delivered", "events": by["m1"], "attempts": [
                attempt(1, 11, "deferred", 12, "data_end", 451, "4.3.0"),
                attempt(2, 20, "delivered", 21, "data_end", 250, "2.0.0")]},
            {"msg_id": "m2", "state": "delivered", "events": by["m2"], "attempts": [
                attempt(1, 31, "interrupted", 40),
                attempt(2, 41, "delivered", 42, "data_end", 250, "2.0.0")]},
            {"msg_id": "m3", "state": "bounced", "events": by["m3"], "attempts": [
                attempt(1, 51, "bounced", 52, "rcpt", 550, "5.1.1")]},
        ]
        self.receiver = [data_end(1, 12, "m1", 451, "4.3.0"), data_end(2, 21, "m1", 250, "2.0.0"),
                         data_end(3, 33, "m2", 250, "2.0.0"), data_end(4, 42, "m2", 250, "2.0.0")]

    def check(self, timelines=None, events=None, receiver=None):
        return debug_timeline.check(self.timelines if timelines is None else timelines,
                                    self.events if events is None else events,
                                    self.receiver if receiver is None else receiver)

    def test_matching_timeline_passes(self):
        ok, d, failures = self.check()
        self.assertTrue(ok, failures)
        self.assertEqual((d["messages"], d["matched"], d["data_end"]), (3, 3, 4))
        self.assertEqual((d["attempts_answered_at_data_end"], d["attempts_unseen_with_data_end"],
                          d["attempts_without_data_end"]), (3, 1, 1))

    def test_codes_in_another_order_fail(self):
        rec = [data_end(1, 12, "m1", 250, "2.0.0"), data_end(2, 21, "m1", 451, "4.3.0")] + self.receiver[2:]
        ok, _, failures = self.check(receiver=rec)
        self.assertFalse(ok)
        self.assertTrue(any("m1: attempt 1 has 451" in f for f in failures), failures)

    def test_attempt_with_data_without_receiver_line_fails(self):
        ok, _, failures = self.check(receiver=self.receiver[1:])
        self.assertFalse(ok)
        self.assertTrue(any("attempt 1 ended with 451" in f for f in failures), failures)

    def test_data_end_for_an_attempt_refused_before_data_fails(self):
        ok, _, failures = self.check(receiver=self.receiver + [data_end(5, 52, "m3", 250)])
        self.assertFalse(ok)
        self.assertTrue(any("m3: attempt 1 ended at rcpt" in f for f in failures), failures)

    def test_interrupted_attempt_with_two_data_end_fails(self):
        rec = self.receiver[:3] + [data_end(9, 35, "m2", 250)] + self.receiver[3:]
        ok, _, failures = self.check(receiver=rec)
        self.assertFalse(ok)

    def test_missing_timeline_fails(self):
        ok, d, failures = self.check(timelines=self.timelines[:2])
        self.assertFalse(ok)
        self.assertIn("m3: no timeline", failures)

    def test_timeline_dropping_an_event_fails(self):
        tls = copy.deepcopy(self.timelines)
        del tls[1]["events"][1]
        ok, _, failures = self.check(timelines=tls)
        self.assertFalse(ok)
        self.assertIn("m2: timeline events differ from the events table", failures)

    def test_wrong_state_fails(self):
        tls = copy.deepcopy(self.timelines)
        tls[2]["state"] = "delivered"
        ok, _, _ = self.check(timelines=tls)
        self.assertFalse(ok)

    def test_lost_attempt_fails(self):
        tls = copy.deepcopy(self.timelines)
        del tls[0]["attempts"][0]
        ok, _, failures = self.check(timelines=tls)
        self.assertFalse(ok)
        self.assertIn("m1: 1 attempts in the timeline, 2 attempt_started", failures)

    def test_receiver_message_without_timeline_fails(self):
        ok, _, failures = self.check(receiver=self.receiver + [data_end(5, 60, "m9", 250)])
        self.assertFalse(ok)
        self.assertIn("m9: receiver data_end without a timeline", failures)

    def test_data_end_before_the_first_attempt_fails(self):
        rec = [data_end(0, 5, "m1", 451, "4.3.0")] + self.receiver
        ok, _, failures = self.check(receiver=rec)
        self.assertFalse(ok)
        self.assertTrue(any("before any attempt" in f for f in failures), failures)


class DebugAggregatesTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        events = [
            fixtures.event(1, 1, "m1", "accepted", domain="gmail-model.test"),
            fixtures.event(2, 2, "m1", "attempt_started", domain="gmail-model.test",
                           provider="gmail-model", src_ip="127.0.2.1"),
            fixtures.event(3, 3, "m1", "delivered", domain="gmail-model.test",
                           provider="gmail-model", src_ip="127.0.2.1"),
            fixtures.event(4, 4, "m2", "accepted", domain="nullmx.test"),
            fixtures.event(5, 4, "m2", "bounced", domain="nullmx.test"),
            fixtures.event(6, 5, None, "tenant_paused"),
        ]
        run = fixtures.write_run(self.tmp.name, [], events)
        self.conn = formats.connect_events_db(os.path.join(run, "engine.db"))
        self.main = {"by": list(debug_aggregates.MAIN), "filters": {}, "rows": [
            {"tenant": "acme", "domain": None, "provider": None, "src_ip": None, "kind": "tenant_paused", "count": 1},
            {"tenant": "acme", "domain": "gmail-model.test", "provider": None, "src_ip": None, "kind": "accepted", "count": 1},
            {"tenant": "acme", "domain": "gmail-model.test", "provider": "gmail-model", "src_ip": "127.0.2.1",
             "kind": "attempt_started", "count": 1},
            {"tenant": "acme", "domain": "gmail-model.test", "provider": "gmail-model", "src_ip": "127.0.2.1",
             "kind": "delivered", "count": 1},
            {"tenant": "acme", "domain": "nullmx.test", "provider": None, "src_ip": None, "kind": "accepted", "count": 1},
            {"tenant": "acme", "domain": "nullmx.test", "provider": None, "src_ip": None, "kind": "bounced", "count": 1},
        ]}
        self.filtered = {"by": ["kind"], "filters": {"domain": "nullmx.test"}, "rows": [
            {"kind": "accepted", "count": 1}, {"kind": "bounced", "count": 1}]}

    def tearDown(self):
        self.conn.close()
        self.tmp.cleanup()

    def test_exact_counts_pass(self):
        ok, d, failures = debug_aggregates.check([self.main, self.filtered], self.conn)
        self.assertTrue(ok, failures)
        self.assertEqual((d["events"], d["queries"], d["groups"], d["mismatched"]), (6, 2, 8, 0))

    def test_queries_cover_every_dimension_and_filter(self):
        qs = debug_aggregates.queries(self.conn)
        self.assertEqual(qs[0], (debug_aggregates.MAIN, {}))
        self.assertTrue(all(((dim,), {}) in qs for dim in debug_aggregates.DIMENSIONS))
        self.assertIn((debug_aggregates.MAIN, {"kind": "accepted"}), qs)
        self.assertIn((debug_aggregates.MAIN, {"provider": "gmail-model"}), qs)

    def test_wrong_count_fails(self):
        bad = copy.deepcopy(self.main)
        bad["rows"][3]["count"] = 2
        ok, d, failures = debug_aggregates.check([bad], self.conn)
        self.assertFalse(ok)
        self.assertEqual(d["mismatched"], 1)

    def test_missing_group_fails(self):
        bad = copy.deepcopy(self.filtered)
        bad["rows"] = bad["rows"][:1]
        ok, d, _ = debug_aggregates.check([self.main, bad], self.conn)
        self.assertFalse(ok)
        self.assertEqual(d["mismatched"], 1)

    def test_filter_ignored_fails(self):
        bad = copy.deepcopy(self.filtered)
        bad["rows"][0]["count"] = 2  # what an unfiltered count of accepted gives
        ok, _, _ = debug_aggregates.check([self.main, bad], self.conn)
        self.assertFalse(ok)

    def test_duplicated_group_and_extra_keys_fail(self):
        bad = copy.deepcopy(self.filtered)
        bad["rows"].append({"kind": "bounced", "count": 0})
        ok, _, failures = debug_aggregates.check([self.main, bad], self.conn)
        self.assertFalse(ok)
        bad = copy.deepcopy(self.filtered)
        bad["rows"][0]["domain"] = "nullmx.test"
        ok, _, failures = debug_aggregates.check([self.main, bad], self.conn)
        self.assertFalse(ok)

    def test_main_query_required(self):
        ok, _, failures = debug_aggregates.check([self.filtered], self.conn)
        self.assertFalse(ok)


if __name__ == "__main__":
    unittest.main()
