"""debug_timeline: for every message of the real campaign, `mtaq timeline`
matches the receiver's trace (4, 4.2).

mtaq is run as a subprocess on the campaign's engine.db. Its events must be
exactly the rows of `events` for the message, and its attempts must explain
every data_end of the message in receiver.jsonl, in order. A data_end
belongs to the last attempt that started at or before it (the three logs
share the machine's wall clock, 3.6). Then:

- an attempt that ended with a reply at data_end has exactly one data_end,
  with the same code and enhanced code;
- an attempt whose outcome the engine could not see (interrupted by a kill,
  still open, or a connection lost after the final dot: stage data_end
  without a reply) has at most one: the receiver may or may not have
  accepted before the failure;
- any other attempt (refused before DATA, no connection) has none.

A data_end before the first attempt, or of a msg_id without a timeline,
fails.
"""

import json
import os

from verifier import formats, report

NAME = "debug_timeline"
RESULTS = ("delivered", "deferred", "bounced", "expired")
TERMINAL = ("delivered", "bounced", "expired", "suppressed")


def _norm(e):
    e = dict(e)
    e["detail"] = e.get("detail") or {}
    return e


def _enh(v):
    return v or None


def kind_of(a):
    """answered, unseen (the engine cannot know whether DATA ended) or none."""
    if a.get("result") in RESULTS and a.get("stage") == "data_end":
        return "answered" if a.get("code") is not None else "unseen"
    if a.get("result") in (None, "interrupted"):
        return "unseen"
    return "none"


def check_one(tl, raw, data_ends):
    """Failures of one message's timeline against its raw events and data_ends."""
    m = tl["msg_id"]
    failures = []
    if [_norm(e) for e in tl.get("events", [])] != [_norm(e) for e in raw]:
        failures.append(f"{m}: timeline events differ from the events table")
    terminal = [e["kind"] for e in raw if e["kind"] in TERMINAL]
    if terminal and tl.get("state") != terminal[-1]:
        failures.append(f"{m}: state {tl.get('state')}, events end {terminal[-1]}")
    if not terminal and tl.get("state") in TERMINAL:
        failures.append(f"{m}: state {tl.get('state')} without a terminal event")

    attempts = tl.get("attempts", [])
    started = sum(1 for e in raw if e["kind"] == "attempt_started")
    if len(attempts) != started:
        failures.append(f"{m}: {len(attempts)} attempts in the timeline, {started} attempt_started")
    for prev, a in zip(attempts, attempts[1:]):
        if not (a["attempt"] or 0) > (prev["attempt"] or 0) or a["started_at_ms"] < prev["started_at_ms"]:
            failures.append(f"{m}: attempts out of order: {prev['attempt']} then {a['attempt']}")

    owned = [[] for _ in attempts]
    for r in data_ends:
        i = max((j for j, a in enumerate(attempts) if a["started_at_ms"] <= r["at_ms"]), default=None)
        if i is None:
            failures.append(f"{m}: receiver data_end seq {r['seq']} before any attempt")
        else:
            owned[i].append(r)
    for a, rs in zip(attempts, owned):
        k, n = kind_of(a), a["attempt"]
        if k == "answered":
            if len(rs) != 1:
                failures.append(f"{m}: attempt {n} ended with {a['code']} at data_end; "
                                f"the receiver has {len(rs)} data_end for it")
            elif (rs[0].get("code"), _enh(rs[0].get("enhanced"))) != (a["code"], _enh(a.get("enhanced"))):
                failures.append(f"{m}: attempt {n} has {a['code']} {a.get('enhanced')}, the receiver's "
                                f"data_end seq {rs[0]['seq']} {rs[0].get('code')} {rs[0].get('enhanced')}")
        elif k == "unseen" and len(rs) > 1:
            failures.append(f"{m}: attempt {n} ({a.get('result')}) owns {len(rs)} data_end")
        elif k == "none" and rs:
            failures.append(f"{m}: attempt {n} ended at {a.get('stage')} but the receiver has a "
                            f"data_end (seq {rs[0]['seq']})")
    return failures, attempts, owned


def check(timelines, events, receiver):
    """Returns (ok, details, failures)."""
    failures = []
    raw = {}
    for e in events:
        if e["msg_id"]:
            raw.setdefault(e["msg_id"], []).append(e)
    accepted = {e["msg_id"] for e in events if e["kind"] == "accepted"}
    by_msg = {}
    for tl in timelines:
        if tl["msg_id"] in by_msg:
            failures.append(f"{tl['msg_id']}: two timelines")
        by_msg[tl["msg_id"]] = tl
    missing = sorted(set(raw) - set(by_msg))
    extra = sorted(set(by_msg) - set(raw))
    failures += [f"{m}: no timeline" for m in missing]
    failures += [f"{m}: timeline of a message without events" for m in extra]

    ends = {}
    for r in receiver:
        if r["ev"] == "data_end" and r.get("msg_id"):
            ends.setdefault(r["msg_id"], []).append(r)
    failures += [f"{m}: receiver data_end without a timeline" for m in sorted(set(ends) - set(by_msg))]

    counts = {"attempts": 0, "answered": 0, "unseen": 0, "unseen_with_data_end": 0, "none": 0,
              "interrupted": 0}
    matched = 0
    for m in sorted(set(by_msg) & set(raw)):
        f, attempts, owned = check_one(by_msg[m], raw[m], ends.get(m, []))
        failures += f
        matched += not f
        for a, rs in zip(attempts, owned):
            k = kind_of(a)
            counts["attempts"] += 1
            counts[k] += 1
            counts["unseen_with_data_end"] += k == "unseen" and len(rs) == 1
            counts["interrupted"] += a.get("result") == "interrupted"
    if not accepted:
        failures.append("no accepted message in the run")
    details = {
        "messages": len(by_msg),
        "accepted": len(accepted),
        "matched": matched,
        "data_end": sum(len(v) for v in ends.values()),
        "attempts": counts["attempts"],
        "attempts_answered_at_data_end": counts["answered"],
        "attempts_unseen": counts["unseen"],
        "attempts_unseen_with_data_end": counts["unseen_with_data_end"],
        "attempts_interrupted": counts["interrupted"],
        "attempts_without_data_end": counts["none"],
    }
    return not failures, details, failures


def run(ctx):
    c = ctx.campaign()
    d = c["dir"]
    db = os.path.join(d, "engine.db")
    out_dir = os.path.join(ctx.lab_dir(c["seed"]), "debug")
    os.makedirs(out_dir, exist_ok=True)
    path = os.path.join(out_dir, "timeline.jsonl")
    if os.path.lexists(path):
        os.remove(path)
    text = ctx.mtaq("timeline", "--db", db, "--all")
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)
    timelines = [json.loads(line) for line in text.splitlines() if line.strip()]
    ok, details, failures = check(timelines, formats.read_events(db), formats.read_receiver(d))
    details["timeline"] = ctx.rel(path)
    return report.result(NAME, ok, details, seed=c["seed"], trace=ctx.rel(d), failures=failures)

