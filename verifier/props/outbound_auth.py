"""outbound_auth: SPF, DKIM and DMARC of the engine's mail at the receiver (4).

One real run inside the namespace. The lab zone (lab/zone.json) publishes
two sender domains: auth-good.test, with SPF for the pool, the run's DKIM
key and DMARC, and auth-broken.test, whose DKIM record holds another key
and whose SPF record excludes the pool. mtasim generates the DKIM keys of
the run and the engine signs with them (dkim.key_dir). The run sends the
same number of messages from each domain to outlook-model.test, whose
auth rule requires the three checks.

Every data_end from the good domain must be accepted with auth pass on
spf, dkim and dmarc. Every message from the broken domain must see exactly
one attempt: one attempt_started, one data_end with 550 5.7.515 and spf,
dkim and dmarc fail, an engine classification permanent/AuthConfig and a
bounce, never a deferral.
"""

import os
import sqlite3
import time

from verifier import formats, report
from verifier.props.tls_policy import _post

NAME = "outbound_auth"
GOOD_DOMAIN = "auth-good.test"
BROKEN_DOMAIN = "auth-broken.test"
RCPT_DOMAIN = "outlook-model.test"
# Messages per sender domain (supuesto: enough to reuse connections).
MESSAGES = 10
SEED = 1
RUN_TIMEOUT_S = 60
POLL_S = 0.2
CHECKS = ("spf", "dkim", "dmarc")


def check(events, receiver, ids, good=GOOD_DOMAIN, broken=BROKEN_DOMAIN):
    """ids: {"good": [msg_id], "broken": [msg_id]} as the API returned them.
    Returns (ok, details, failures)."""
    failures = []
    fail = failures.append
    by_msg = {}
    for e in events:
        by_msg.setdefault(e["msg_id"], []).append(e)
    data_ends = {}
    for r in receiver:
        if r["ev"] != "data_end":
            continue
        sender = str(r.get("mail_from", "")).lower()
        data_ends.setdefault(r.get("msg_id"), []).append(r)
        if sender.endswith("@" + good):
            a = r.get("auth") or {}
            if any(a.get(k) != "pass" for k in CHECKS):
                fail(f"receiver seq {r['seq']}: {r.get('msg_id')} from {good} has auth {r.get('auth')}")
            if not (isinstance(r.get("code"), int) and 200 <= r["code"] < 300):
                fail(f"receiver seq {r['seq']}: {r.get('msg_id')} from {good} answered {r.get('code')} {r.get('enhanced')}")

    for side in ("good", "broken"):
        if not ids.get(side):
            fail(f"no message from the {side} domain")
    for m in ids.get("good", []):
        evs = by_msg.get(m, [])
        if not any(e["kind"] == "delivered" for e in evs):
            fail(f"{m} from {good} not delivered")
        if not data_ends.get(m):
            fail(f"{m} from {good} never reached the receiver")
    for m in ids.get("broken", []):
        evs = by_msg.get(m, [])
        starts = [e for e in evs if e["kind"] == "attempt_started"]
        ends = data_ends.get(m, [])
        if len(starts) != 1 or len(ends) != 1:
            fail(f"{m} from {broken}: {len(starts)} attempts in engine.db and {len(ends)} data_end in the receiver, want 1 and 1")
        for r in ends:
            if r.get("code") != 550 or r.get("enhanced") != "5.7.515":
                fail(f"receiver seq {r['seq']}: {m} from {broken} answered {r.get('code')} {r.get('enhanced')}, want 550 5.7.515")
            a = r.get("auth") or {}
            if any(a.get(k) != "fail" for k in CHECKS):
                fail(f"receiver seq {r['seq']}: {m} from {broken} has auth {r.get('auth')}, want fail on all three")
        classified = [e for e in evs if e["kind"] == "classified"]
        if not classified or any((e["class"], e["subtype"]) != ("permanent", "AuthConfig") for e in classified):
            fail(f"{m} from {broken}: classified {[(e['class'], e['subtype'], e['rule_id']) for e in classified]}, want permanent/AuthConfig")
        if any(e["kind"] == "deferred" for e in evs):
            fail(f"{m} from {broken} was deferred")
        if not any(e["kind"] == "bounced" for e in evs):
            fail(f"{m} from {broken} not bounced")

    def auth_counts(side):
        out = {}
        for m in ids.get(side, []):
            for r in data_ends.get(m, []):
                a = r.get("auth") or {}
                key = "/".join(str(a.get(k)) for k in CHECKS)
                out[key] = out.get(key, 0) + 1
        return out

    details = {
        "good_domain": good,
        "broken_domain": broken,
        "rcpt_domain": RCPT_DOMAIN,
        "messages": {side: len(ids.get(side, [])) for side in ("good", "broken")},
        "auth": {side: auth_counts(side) for side in ("good", "broken")},
        "broken_replies": sorted({f"{r.get('code')} {r.get('enhanced')}" for m in ids.get("broken", [])
                                  for r in data_ends.get(m, [])}),
        "broken_attempts": sum(1 for m in ids.get("broken", []) for e in by_msg.get(m, [])
                               if e["kind"] == "attempt_started"),
    }
    return not failures, details, failures


def _settled(db, ids):
    """Whether every good message is delivered and every broken one bounced."""
    if not os.path.exists(db):
        return False
    conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True, timeout=10)
    try:
        rows = conn.execute("SELECT msg_id, kind FROM events WHERE kind IN ('delivered', 'bounced')").fetchall()
    except sqlite3.OperationalError:
        return False
    finally:
        conn.close()
    delivered = {m for m, k in rows if k == "delivered"}
    bounced = {m for m, k in rows if k == "bounced"}
    return all(m in delivered for m in ids["good"]) and all(m in bounced for m in ids["broken"])


def real_run(ctx, run_dir, seed=SEED):
    """The run; returns (ids, summary)."""
    from verifier.lab import Harness, Lab, merge  # the lab needs the namespace

    base = formats.load_json(ctx.path("lab", "engine.json"))
    cfg = merge(base, {"retry_policy": dict(ctx.params["campaign_retry_policy"]), "seed": seed})
    harness = Harness(os.path.join(run_dir, "harness.jsonl"))
    lab = Lab(ctx.root, ctx.bin, run_dir, cfg, harness)
    t0 = time.monotonic()
    ids = {"good": [], "broken": []}
    summary = {}
    try:
        lab.start_sim()
        lab.start_engine()
        for i in range(MESSAGES):
            for side, domain in (("good", GOOD_DOMAIN), ("broken", BROKEN_DOMAIN)):
                ids[side].append(_post(cfg["http"], f"auth-{side}-{i}", {
                    "tenant": "acme", "from": f"notify@{domain}", "to": f"user{i}@{RCPT_DOMAIN}",
                    "subject": f"Auth {side} message {i}", "text": f"Message {i} from the {side} sender.\n",
                    "msg_type": "transactional"}))
        deadline = time.monotonic() + RUN_TIMEOUT_S
        settled = False
        while time.monotonic() < deadline:
            if _settled(lab.db, ids):
                settled = True
                break
            time.sleep(POLL_S)
        summary["settled"] = settled
        summary["exit_codes"] = lab.stop()
    finally:
        if lab.engine is not None and lab.engine.alive():
            lab.engine.stop()
        if lab.sim is not None and lab.sim.alive():
            lab.sim.stop()
        harness.close()
    summary["duration_s"] = round(time.monotonic() - t0, 3)
    return ids, summary


def run(ctx):
    trace = ctx.fresh_dir(f"auth-{SEED}")
    ids, summary = real_run(ctx, trace)
    failures = [] if summary["settled"] else [f"the run did not settle in {RUN_TIMEOUT_S} s"]
    ok, details, more = check(formats.read_events(os.path.join(trace, "engine.db")),
                              formats.read_receiver(trace), ids)
    details["summary"] = summary
    return report.result(NAME, ok, details, seed=SEED, trace=ctx.rel(trace), failures=failures + more)
