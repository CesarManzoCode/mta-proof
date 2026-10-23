"""Synthetic traces for the properties of the real campaign (no_loss,
bounded_duplicates, idempotency, dns_mx): each has a passing trace and the
failing ones the contract names."""

import os
import tempfile
import unittest

from verifier import campaign, context, formats, lab
from verifier.props import bounded_duplicates, dns_mx, idempotency, no_loss

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
PARAMS = {"campaign_messages": 2, "campaign_kills": 1, "campaign_kills_post_250": 1}


def ev(seq, at, msg_id, kind, detail=None, **cols):
    e = {c: None for c in formats.EVENT_COLUMNS}
    e.update(seq=seq, at_ms=at, msg_id=msg_id, kind=kind, detail=detail or {}, **cols)
    return e


def rl(seq, at, conn, evname, **kw):
    r = {"seq": seq, "at_ms": at, "conn": conn, "ev": evname, "src_ip": "127.0.2.1",
         "profile": "baseline"}
    r.update(kw)
    return r


def data_end(seq, at, conn, msg_id, code=250, sha="aa"):
    return rl(seq, at, conn, "data_end", msg_id=msg_id, code=code, sha256=sha)


class NoLossTest(unittest.TestCase):
    def setUp(self):
        self.events = [ev(1, 10, "m1", "accepted", {"idempotency_key": "k1"}),
                       ev(2, 11, "m2", "accepted", {"idempotency_key": "k2"}),
                       ev(3, 30, "m1", "delivered"), ev(4, 60, "m2", "delivered")]
        self.ingest = [{"key": "k1", "kind": "new", "status": 202, "msg_id": "m1"},
                       {"key": "k2", "kind": "new", "status": 202, "msg_id": "m2"}]
        self.harness = [{"at_ms": 1, "action": "start"},
                        {"at_ms": 45, "action": "kill", "window": "post_250", "msg_id": "m2"},
                        {"at_ms": 46, "action": "start"}]
        self.receiver = [data_end(1, 40, 1, "m2"), data_end(2, 55, 2, "m2")]

    def test_every_accepted_terminal_passes(self):
        ok, d, failures = no_loss.check(self.events, self.ingest, self.harness, self.receiver, PARAMS)
        self.assertTrue(ok, failures)
        self.assertEqual((d["messages"], d["kills"], d["kills_post_250"], d["lost"]), (2, 1, 1, 0))

    def test_post_202_kill_without_a_202_counts(self):
        # The original engine: no 202 during the stall.
        harness = self.harness + [{"at_ms": 50, "action": "kill", "window": "post_202", "msg_id": None},
                                  {"at_ms": 51, "action": "start"}]
        ok, d, failures = no_loss.check(self.events, self.ingest, harness, self.receiver, PARAMS,
                                        kills_post_202=1)
        self.assertTrue(ok, failures)
        self.assertEqual((d["kills"], d["kills_post_202"], d["answered_during_stall"]), (2, 1, 0))

    def test_post_202_kill_needs_its_202_in_ingest(self):
        harness = self.harness + [{"at_ms": 50, "action": "kill", "window": "post_202", "msg_id": "m9"}]
        ok, _, failures = no_loss.check(self.events, self.ingest, harness, self.receiver, PARAMS,
                                        kills_post_202=1)
        self.assertFalse(ok)
        self.assertIn("post_202 kill at 50: no 202 for m9 in ingest.jsonl", failures)

    def test_post_202_kill_lost_message_fails(self):
        # The mutant's case: the API answered 202 for m3, the kill came
        # right after it and m3 never reached engine.db.
        ingest = self.ingest + [{"key": "k3", "kind": "new", "status": 202, "msg_id": "m3"}]
        harness = self.harness + [{"at_ms": 50, "action": "kill", "window": "post_202", "msg_id": "m3"}]
        ok, d, failures = no_loss.check(self.events, ingest, harness, self.receiver, PARAMS,
                                        kills_post_202=1)
        self.assertFalse(ok)
        self.assertEqual(d["lost_acknowledged"], 1)
        self.assertIn("m3: answered 202 but has no accepted event", failures)

    def test_msg_id_reused_after_a_lost_202_fails(self):
        # k3 got 202 with m2 but was lost; after the restart the engine gave
        # m2 to k2. The msg_id has an accepted event, of another key.
        ingest = self.ingest + [{"key": "k3", "kind": "new", "status": 202, "msg_id": "m2"}]
        ok, d, failures = no_loss.check(self.events, ingest, self.harness, self.receiver, PARAMS)
        self.assertFalse(ok)
        self.assertEqual(d["lost_acknowledged"], 1)
        self.assertIn("m2: answered 202 to another key than its accepted event's", failures)

    def test_too_few_post_202_kills_fail(self):
        ok, _, failures = no_loss.check(self.events, self.ingest, self.harness, self.receiver, PARAMS,
                                        kills_post_202=1)
        self.assertFalse(ok)
        self.assertIn("0 confirmed post_202 kills, fewer than kills_post_202 1", failures)

    def test_accepted_without_terminal_fails(self):
        events = self.events[:3]
        ok, d, failures = no_loss.check(events, self.ingest, self.harness, self.receiver, PARAMS)
        self.assertFalse(ok)
        self.assertEqual(d["lost"], 1)
        self.assertIn("m2: accepted without a terminal event", failures)

    def test_acknowledged_without_accepted_fails(self):
        ingest = self.ingest + [{"key": "k3", "kind": "new", "status": 202, "msg_id": "m3"}]
        ok, d, failures = no_loss.check(self.events, ingest, self.harness, self.receiver, PARAMS)
        self.assertFalse(ok)
        self.assertEqual(d["lost_acknowledged"], 1)

    def test_post_250_kill_needs_the_receivers_2xx(self):
        ok, d, failures = no_loss.check(self.events, self.ingest, self.harness, [], PARAMS)
        self.assertFalse(ok)
        self.assertEqual(d["kills_post_250"], 0)

    def test_campaign_below_parameters_fails(self):
        ok, _, failures = no_loss.check(self.events, self.ingest, self.harness, self.receiver,
                                        dict(PARAMS, campaign_messages=3))
        self.assertFalse(ok)


class BoundedDuplicatesTest(unittest.TestCase):
    events = [ev(1, 10, "m1", "accepted"), ev(2, 70, "m1", "delivered")]

    def test_duplicate_with_kill_in_window_passes(self):
        rec = [data_end(1, 20, 1, "m1"), data_end(2, 60, 2, "m1")]
        ok, d, failures = bounded_duplicates.check(rec, self.events, [{"at_ms": 30, "action": "kill"}])
        self.assertTrue(ok, failures)
        self.assertEqual((d["duplicates"], d["duplicates_outside_window"]), (1, 0))

    def test_duplicate_without_kill_in_window_fails(self):
        rec = [data_end(1, 20, 1, "m1"), data_end(2, 60, 2, "m1")]
        # The only kill comes after the delivered event.
        ok, d, failures = bounded_duplicates.check(rec, self.events, [{"at_ms": 80, "action": "kill"}])
        self.assertFalse(ok)
        self.assertEqual(d["duplicates_outside_window"], 1)

    def test_kill_between_mail_and_the_receivers_2xx_passes(self):
        # The sender died after sending DATA; the receiver accepted afterwards.
        rec = [rl(1, 15, 1, "mail"), data_end(2, 20, 1, "m1"), rl(3, 21, 1, "close"),
               data_end(4, 60, 2, "m1")]
        ok, d, failures = bounded_duplicates.check(rec, self.events, [{"at_ms": 18, "action": "kill"}])
        self.assertTrue(ok, failures)
        self.assertEqual(d["duplicates_by_kill"], 1)

    def test_kill_before_the_transaction_fails(self):
        rec = [rl(1, 15, 1, "mail"), data_end(2, 20, 1, "m1"), data_end(3, 60, 2, "m1")]
        ok, _, _ = bounded_duplicates.check(rec, self.events, [{"at_ms": 12, "action": "kill"}])
        self.assertFalse(ok)

    def test_duplicate_after_control_cut_passes(self):
        rec = [data_end(1, 20, 1, "m1"), rl(2, 21, 1, "drop", rule="control.cut"),
               data_end(3, 60, 2, "m1")]
        ok, d, failures = bounded_duplicates.check(rec, self.events, [{"at_ms": 19, "action": "cut"}])
        self.assertTrue(ok, failures)
        self.assertEqual(d["duplicates_by_cut"], 1)

    def test_drop_by_a_profile_rule_does_not_explain(self):
        rec = [data_end(1, 20, 1, "m1"), rl(2, 21, 1, "drop", rule="gmail.rate_ip"),
               data_end(3, 60, 2, "m1")]
        ok, _, _ = bounded_duplicates.check(rec, self.events, [{"at_ms": 19, "action": "cut"}])
        self.assertFalse(ok)

    def test_copies_with_other_bytes_fail(self):
        rec = [data_end(1, 20, 1, "m1"), data_end(2, 60, 2, "m1", sha="bb")]
        ok, _, failures = bounded_duplicates.check(rec, self.events, [{"at_ms": 30, "action": "kill"}])
        self.assertFalse(ok)
        self.assertIn("m1: duplicate copies with different sha256", failures)


class IdempotencyTest(unittest.TestCase):
    def setUp(self):
        self.events = [
            ev(1, 1, "m1", "accepted", {"idempotency_key": "k1"}),
            ev(2, 2, "m1", "idempotent_replay", {"idempotency_key": "k1"}),
            ev(3, 3, "m1", "idempotency_conflict", {"idempotency_key": "k1"}),
        ]
        self.ingest = [
            {"key": "k1", "kind": "new", "status": 202, "msg_id": "m1", "replayed": False},
            {"key": "k1", "kind": "replay", "status": 202, "msg_id": "m1", "replayed": True},
            {"key": "k1", "kind": "conflict", "status": 409, "msg_id": "m1", "replayed": False},
        ]

    def test_replay_and_conflict_pass(self):
        ok, d, failures = idempotency.check(self.ingest, self.events, 1, 1)
        self.assertTrue(ok, failures)
        self.assertEqual((d["replays"], d["conflicts_rejected"]), (1, 1))

    def test_second_accepted_for_a_key_fails(self):
        events = self.events + [ev(4, 4, "m2", "accepted", {"idempotency_key": "k1"})]
        ok, _, failures = idempotency.check(self.ingest, events, 1, 1)
        self.assertFalse(ok)
        self.assertIn("key k1: 2 accepted messages ['m1', 'm2']", failures)

    def test_conflict_accepted_fails(self):
        ingest = self.ingest[:2] + [{"key": "k1", "kind": "conflict", "status": 202,
                                     "msg_id": "m1", "replayed": True}]
        ok, _, _ = idempotency.check(ingest, self.events, 1, 1)
        self.assertFalse(ok)

    def test_conflict_without_event_fails(self):
        ok, _, _ = idempotency.check(self.ingest, self.events[:2], 1, 1)
        self.assertFalse(ok)


ZONE = {"records": [
    {"name": "nullmx.test", "type": "MX", "pref": 0, "host": "."},
    {"name": "fallback.test", "type": "MX", "pref": 10, "host": "mx1.fallback.test"},
    {"name": "fallback.test", "type": "MX", "pref": 20, "host": "mx2.fallback.test"},
    {"name": "mx1.fallback.test", "type": "A", "addr": "127.0.1.7"},
    {"name": "mx2.fallback.test", "type": "A", "addr": "127.0.1.5"},
]}


class DNSMXTest(unittest.TestCase):
    def setUp(self):
        self.events = [
            ev(1, 1, "m1", "accepted", domain="nullmx.test"),
            ev(2, 2, "m1", "routed", {"null_mx": True}, domain="nullmx.test"),
            ev(3, 2, "m1", "classified", domain="nullmx.test", rule_id="rfc7505.null_mx"),
            ev(4, 2, "m1", "bounced", domain="nullmx.test", subtype="NullMX"),
            ev(5, 3, "m2", "accepted", domain="fallback.test"),
            ev(6, 4, "m2", "attempt_started", domain="fallback.test"),
            ev(7, 5, "m2", "routed", {"mx": ["mx1.fallback.test", "mx2.fallback.test"],
                                      "addr": "127.0.1.5:25",
                                      "tried": [{"host": "mx1.fallback.test", "addr": "127.0.1.7:25",
                                                 "error": "connection refused"}]},
               domain="fallback.test", mx_host="mx2.fallback.test"),
            ev(8, 6, "m2", "delivered", domain="fallback.test", mx_host="mx2.fallback.test"),
        ]
        self.receiver = [rl(1, 5, 1, "connect", dst_ip="127.0.1.5"),
                         rl(2, 5, 1, "rcpt", rcpt="user1@fallback.test"),
                         dict(data_end(3, 6, 1, "m2"), dst_ip="127.0.1.5")]

    def test_null_mx_and_fallback_pass(self):
        ok, d, failures = dns_mx.check(self.events, self.receiver, ZONE)
        self.assertTrue(ok, failures)
        self.assertEqual((d["null_mx_messages"], d["fallback_delivered"]), (1, 1))
        self.assertEqual((d["preferred_mx"], d["served_mx"]), ("mx1.fallback.test", "mx2.fallback.test"))

    def test_null_mx_with_a_connection_fails(self):
        rec = self.receiver + [rl(4, 7, 2, "connect", dst_ip="127.0.1.5"),
                               rl(5, 7, 2, "rcpt", rcpt="user0@nullmx.test")]
        ok, _, failures = dns_mx.check(self.events, rec, ZONE)
        self.assertFalse(ok)
        self.assertTrue(any("null MX domain" in f for f in failures), failures)

    def test_null_mx_retry_fails(self):
        events = self.events + [ev(9, 9, "m1", "deferred", domain="nullmx.test")]
        ok, _, _ = dns_mx.check(events, self.receiver, ZONE)
        self.assertFalse(ok)

    def test_wrong_mx_order_fails(self):
        events = [dict(e) for e in self.events]
        events[6] = dict(events[6], detail=dict(events[6]["detail"],
                                                mx=["mx2.fallback.test", "mx1.fallback.test"]))
        ok, _, _ = dns_mx.check(events, self.receiver, ZONE)
        self.assertFalse(ok)

    def test_preferred_mx_reached_fails(self):
        rec = self.receiver + [rl(4, 7, 3, "connect", dst_ip="127.0.1.7")]
        ok, _, _ = dns_mx.check(self.events, rec, ZONE)
        self.assertFalse(ok)


class LabControlPathTest(unittest.TestCase):
    def lab_in(self, run_dir):
        os.makedirs(run_dir)
        return lab.Lab(run_dir, run_dir, run_dir, {}, None)

    def test_short_run_dir_keeps_the_socket_in_it(self):
        with tempfile.TemporaryDirectory() as tmp:
            lb = self.lab_in(os.path.join(tmp, "run"))
            self.assertEqual(lb.control_path, os.path.join(tmp, "run", "control.sock"))

    def test_deep_run_dir_moves_the_socket_within_sun_path(self):
        with tempfile.TemporaryDirectory() as tmp:
            deep = os.path.join(tmp, "out", "runs", "mutation", "mut_ack_before_persist", "runs", "lab-1",
                                "campaign" + "x" * 40)
            lb = self.lab_in(deep)
            self.assertLessEqual(len(os.fsencode(lb.control_path)), lab.SUN_PATH_MAX)
            self.assertFalse(lb.control_path.startswith(deep))


class CampaignPlanTest(unittest.TestCase):
    def setUp(self):
        self.spec = campaign.load_spec(os.path.join(ROOT, "lab", "campaign.json"))
        self.params = context.load_params(os.path.join(ROOT, "lab", "params.json"))

    def test_plan_is_seeded(self):
        a = campaign.plan(self.spec, self.params, 1)
        b = campaign.plan(self.spec, self.params, 1)
        c = campaign.plan(self.spec, self.params, 2)
        self.assertEqual(a["faults"], b["faults"])
        self.assertEqual([m["body"] for m in a["messages"]], [m["body"] for m in b["messages"]])
        self.assertNotEqual(a["faults"], c["faults"])

    def test_plan_meets_the_parameters(self):
        p = campaign.plan(self.spec, self.params, 1)
        kills = [f for f in p["faults"] if f["kind"] == "kill"]
        self.assertEqual(len(p["messages"]), self.params["campaign_messages"])
        self.assertEqual(len(kills), self.params["campaign_kills"])
        self.assertEqual(sum(f["window"] == "post_250" for f in kills),
                         self.params["campaign_kills_post_250"])
        self.assertEqual(sum(f["window"] == "post_202" for f in kills), self.spec["kills_post_202"])
        self.assertEqual(sum(f["kind"] == "switch" for f in p["faults"]), 1)
        self.assertEqual(sum(f["kind"] == "cut" for f in p["faults"]), self.spec["cuts"]["count"])
        self.assertTrue(p["replays"] and p["conflicts"] and not p["replays"] & p["conflicts"])
        domains = {m["body"]["to"].split("@")[1] for m in p["messages"]}
        self.assertTrue({"nullmx.test", "fallback.test", "gmail-model.test", "yahoo-model.test",
                         "outlook-model.test"} <= domains)

    def test_merge_is_key_by_key(self):
        self.assertEqual(lab.merge({"a": {"b": 1, "c": 2}, "d": [1]}, {"a": {"c": 3}, "d": [2]}),
                         {"a": {"b": 1, "c": 3}, "d": [2]})


if __name__ == "__main__":
    unittest.main()
