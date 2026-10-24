"""idempotency: same key and payload, one accepted; same key and another
payload, a 409 and an idempotency_conflict event (3.10, 4).

Decided on ingest.jsonl (each final HTTP answer the orchestrator got, with
its key and kind: `new`, `replay` = the same payload resent after its 202,
`conflict` = another payload under the same key) and on engine.db.
"""

from verifier import formats, report

NAME = "idempotency"


def check(ingest, events, planned_replays=0, planned_conflicts=0):
    """Returns (ok, details, failures)."""
    failures = []
    accepted_by_key = {}
    replay_keys, conflict_keys = set(), set()
    for e in events:
        key = e["detail"].get("idempotency_key")
        if key is None:
            continue
        if e["kind"] == "accepted":
            accepted_by_key.setdefault(key, []).append(e["msg_id"])
        elif e["kind"] == "idempotent_replay":
            replay_keys.add((key, e["msg_id"]))
        elif e["kind"] == "idempotency_conflict":
            conflict_keys.add((key, e["msg_id"]))
    for key, ids in sorted(accepted_by_key.items()):
        if len(ids) > 1:
            failures.append(f"key {key}: {len(ids)} accepted messages {ids}")

    replays = conflicts = resends = 0
    for r in ingest:
        key, kind, status = r["key"], r["kind"], r["status"]
        ids = accepted_by_key.get(key, [])
        original = ids[0] if ids else None
        if kind in ("new", "replay") and status == 202:
            if r.get("msg_id") != original:
                failures.append(f"{kind} {key}: 202 with {r.get('msg_id')}, accepted is {original}")
                continue
            if r.get("replayed"):
                if (key, original) not in replay_keys:
                    failures.append(f"{kind} {key}: replayed without an idempotent_replay event")
                    continue
                if kind == "replay":
                    replays += 1
                else:
                    resends += 1
            elif kind == "replay":
                failures.append(f"replay {key}: 202 without Idempotent-Replayed")
        elif kind == "conflict" and status == 409:
            if r.get("msg_id") != original:
                failures.append(f"conflict {key}: 409 names {r.get('msg_id')}, accepted is {original}")
            elif (key, original) not in conflict_keys:
                failures.append(f"conflict {key}: 409 without an idempotency_conflict event")
            else:
                conflicts += 1
        else:
            failures.append(f"{kind} {key}: unexpected HTTP {status}")
    if replays < planned_replays:
        failures.append(f"{replays} confirmed replays of {planned_replays} planned")
    if conflicts < planned_conflicts:
        failures.append(f"{conflicts} rejected conflicts of {planned_conflicts} planned")
    if replays == 0 or conflicts == 0:
        failures.append("the run has no replay or no conflict: nothing was exercised")
    details = {
        "replays": replays,
        "conflicts_rejected": conflicts,
        "resends_replayed": resends,
        "keys": len(accepted_by_key),
    }
    return not failures, details, failures


def run(ctx):
    c = ctx.campaign()
    d = c["dir"]
    s = c["summary"]
    ok, details, failures = check(formats.read_jsonl(f"{d}/ingest.jsonl"),
                                  formats.read_events(f"{d}/engine.db"),
                                  s["replays_planned"], s["conflicts_planned"])
    return report.result(NAME, ok, details, seed=c["seed"], trace=ctx.rel(d), failures=failures)
